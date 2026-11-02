package ir

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"vyasa/src/llm"
	"vyasa/src/models"

	"golang.org/x/net/html"
)

// segmentMatchers classify a link by keyword hits on its anchor text and
// URL, in priority order (transcript beats annual report beats presentation
// beats quarterly).
var segmentMatchers = []struct {
	seg string
	kw  []string
}{
	{SegTranscript, []string{"transcript", "earnings call", "con-call", "concall"}},
	{SegAnnualReport, []string{"annual report", "annualreport", "annual_report"}},
	{SegPresentation, []string{"investor presentation", "presentation", "analyst meet", "analyst call"}},
	{SegQuarterly, []string{"financial result", "quarterly result", "quarterly financial", "financial performance", "results", "quarter"}},
}

// quarterlyHint detects Q1..Q4 markers used by results filings (Q1, Q1FY24,
// Q1 FY26, quarter 3...).
var quarterlyHint = regexp.MustCompile(`(?i)(?:^|[^a-z])q([1-4])(?:fy|\b|[^a-z0-9])`)

// nameWindow is how far back a document URL may sit from its record's
// "name"/"title" field in an embedded JSON payload.
const nameWindow = 800

// denylist keeps compliance noise (policies, certificates, shareholding
// patterns...) out of the candidate pool.
var denylist = []string{
	"shareholding", "grievance", "credit rating", "code of conduct", "code of fair",
	"policy", "policies", "intimation", "newspaper", "advert", "postal ballot",
	"notice", "reconciliation", "subsidiar", "compliance under", "certificate under",
	"investor education", "protection fund", "iepf", "unpaid dividend", "demateriali",
	"kyc", "nomination", "debenture trustee", "terms of use", "privacy", "contact us",
	"headquarters", "voting", "scrutinizer", "scrutiniser", "monitoring agency",
}

var (
	fyPattern    = regexp.MustCompile(`(?i)(?:^|[^a-z])fy\s*'?(\d{2}|\d{4})\b`)
	year4Pattern = regexp.MustCompile(`\b(?:19|20)\d{2}\b`)
	yearRange    = regexp.MustCompile(`\b(\d{4})\s*[-–/]\s*(\d{2,4})\b`)

	monthDayYear = regexp.MustCompile(`(?i)\b(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(\d{4})\b`)
	dayMonthYear = regexp.MustCompile(`(?i)\b(\d{1,2})(?:st|nd|rd|th)?\s+(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?,?\s+(\d{4})\b`)
	numericDate  = regexp.MustCompile(`\b(\d{1,2})[./](\d{1,2})[./](\d{4})\b`)
)

// monthIndex maps month prefixes to month numbers.
var monthIndex = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

// normKeywords lowercases and flattens separators so URL-style names
// ("integrated-annual-report") still match keyword phrases.
func normKeywords(s string) string {
	return strings.ToLower(strings.NewReplacer("%20", " ", "-", " ", "_", " ", ".", " ").Replace(s))
}

// collectCandidates fetches the investor relations page and a bounded set of
// doc-bearing sub-pages (two levels deep), returning every deduplicated link.
func (c *Client) collectCandidates(ctx context.Context, irURL string) ([]candidate, error) {
	all := map[string]candidate{}
	add := func(page []candidate) {
		for _, cand := range page {
			if _, ok := all[cand.URL]; !ok {
				all[cand.URL] = cand
			}
		}
	}

	first, err := c.candidatesFromPage(ctx, irURL)
	if err != nil {
		return nil, err
	}
	add(first)
	visited := map[string]bool{irURL: true}

	var level2Links []candidate
	for _, pageURL := range rankSubPages(irURL, first, maxSubPages) {
		visited[pageURL] = true
		page, err := c.candidatesFromPage(ctx, pageURL)
		if err != nil {
			log.Printf("[ir] sub-page %s: %v", pageURL, err)
			continue
		}
		add(page)
		level2Links = append(level2Links, page...)
	}

	for _, pageURL := range rankSubPages(irURL, level2Links, maxSubPages/2) {
		if visited[pageURL] {
			continue
		}
		visited[pageURL] = true
		page, err := c.candidatesFromPage(ctx, pageURL)
		if err != nil {
			log.Printf("[ir] sub-page %s: %v", pageURL, err)
			continue
		}
		add(page)
	}

	out := make([]candidate, 0, len(all))
	for _, cand := range all {
		out = append(out, cand)
	}
	return out, nil
}

// fetchHTML downloads a page with browser headers, returning its body.
func (c *Client) fetchHTML(ctx context.Context, pageURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, err
	}
	c.setBrowseHeaders(req, "")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ir: %s: %s", pageURL, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxPageBytes))
}

// jsonFlatten unescapes the escape sequences embedded JSON payloads (Next.js
// flight data, __NEXT_DATA__, CMS blobs) wrap their strings in, so plain
// string scans can find quoted values inside them.
var jsonFlatten = strings.NewReplacer(
	`\"`, `"`,
	`\/`, "/",
	`\u0026`, "&",
	`\u002F`, "/", `\u002f`, "/",
	`\u003C`, "<", `\u003c`, "<",
	`\u003E`, ">", `\u003e`, ">",
)

var (
	// jsonPDFRe matches quoted .pdf URLs anywhere in a flattened payload.
	jsonPDFRe = regexp.MustCompile(`(?i)"([^"\s]+\.pdf)"`)
	// jsonNameRe matches non-empty "name"/"title" values used as record titles.
	jsonNameRe = regexp.MustCompile(`"(?:name|title)":"([^"]{2,200})"`)
	// jsonPageRe matches internal page references ("url":"/investor/...").
	jsonPageRe = regexp.MustCompile(`"url":"(/[^"\s]+)"`)
)

// jsonCandidates mines an embedded JSON payload for documents and internal
// pages. JS-rendered IR sites ship their filing lists this way instead of as
// <a href> anchors, so scraping raw HTML alone finds nothing. Each document
// is paired with the nearest preceding "name"/"title" value (the record's own
// title) so classification and recency ranking still work.
func jsonCandidates(base *url.URL, body []byte, pageURL string) []candidate {
	flat := jsonFlatten.Replace(string(body))
	if !strings.Contains(flat, ".pdf") && !strings.Contains(flat, `"url":"`) {
		return nil
	}

	names := jsonNameRe.FindAllStringSubmatchIndex(flat, -1)
	pdfs := jsonPDFRe.FindAllStringSubmatchIndex(flat, -1)

	var out []candidate
	seen := map[string]bool{}
	ni := 0
	for _, loc := range pdfs {
		start := loc[2]
		for ni < len(names)-1 && names[ni+1][3] <= start {
			ni++
		}
		text := ""
		if ni < len(names) && names[ni][3] <= start && start-names[ni][3] <= nameWindow {
			text = flat[names[ni][2]:names[ni][3]]
		}
		abs := absoluteURL(base, flat[start:loc[3]])
		if abs == "" || isSocialURL(abs) || seen[abs] {
			continue
		}
		seen[abs] = true
		out = append(out, candidate{URL: abs, Text: text, PageURL: pageURL})
	}
	for _, m := range jsonPageRe.FindAllStringSubmatch(flat, -1) {
		abs := absoluteURL(base, m[1])
		if abs == "" || isSocialURL(abs) || seen[abs] || !sameSiteURL(abs, pageURL) {
			continue
		}
		seen[abs] = true
		out = append(out, candidate{URL: abs, PageURL: pageURL})
	}
	return out
}

// pageCandidates returns every link candidate on one fetched page: <a href>
// anchors plus anything referenced by the page's embedded JSON payloads.
// Payloads usually repeat the visible links, so URL-level dedupe keeps the
// anchor text when both exist.
func pageCandidates(base *url.URL, body []byte, pageURL string) []candidate {
	var out []candidate
	seen := map[string]bool{}
	add := func(cand candidate) bool {
		if cand.URL == "" || seen[cand.URL] {
			return false
		}
		seen[cand.URL] = true
		out = append(out, cand)
		return true
	}
	for _, n := range anchors(body) {
		abs := absoluteURL(base, attr(n, "href"))
		if abs == "" || isSocialURL(abs) {
			continue
		}
		add(candidate{URL: abs, Text: nodeText(n), PageURL: pageURL})
	}
	for _, cand := range jsonCandidates(base, body, pageURL) {
		add(cand)
	}
	return out
}

// candidatesFromPage fetches one IR page and returns its absolute links —
// anchors, JSON-payload references and filter-dropdown listing URLs. It also
// synthesises candidates for the page's filter dropdowns (e.g. a "Report
// category" select), so documents hidden behind a category query parameter are
// discovered even when they never appear as anchors.
func (c *Client) candidatesFromPage(ctx context.Context, pageURL string) ([]candidate, error) {
	body, err := c.fetchHTML(ctx, pageURL)
	if err != nil {
		return nil, err
	}

	base, err := url.Parse(pageURL)
	if err != nil {
		return nil, err
	}

	out := pageCandidates(base, body, pageURL)
	out = append(out, filterCandidates(base, body, pageURL)...)
	return out, nil
}

// indexHints mark URLs that are likely to be filings/reports index pages and
// therefore worth mining for filter dropdowns; other pages re-use the same
// shared filter without listing any filings.
var indexHints = []string{"report", "filing", "financial", "presentation", "result", "disclosure", "announcement", "document"}

func looksLikeIndex(pageURL string) bool {
	p := strings.ToLower(pageURL)
	for _, h := range indexHints {
		if strings.Contains(p, h) {
			return true
		}
	}
	return false
}

// filterCandidates turns segment-relevant filter dropdown options into
// listing-page candidates (page URL plus the option as a query parameter).
// Only options whose label already maps to a segment are kept, so the pool is
// not flooded with year/sort selectors.
func filterCandidates(base *url.URL, body []byte, pageURL string) []candidate {
	if !looksLikeIndex(pageURL) {
		return nil
	}
	var out []candidate
	seen := map[string]bool{}
	for _, opt := range selectOptions(body) {
		if opt.Name == "" || opt.Value == "" || opt.Value == "All" {
			continue
		}
		u := *base
		q := u.Query()
		q.Set(opt.Name, opt.Value)
		u.RawQuery = q.Encode()
		cand := candidate{URL: u.String(), Text: opt.Text, PageURL: pageURL}
		if seen[cand.URL] || isSocialURL(cand.URL) || classify(cand) == "" {
			continue
		}
		seen[cand.URL] = true
		out = append(out, cand)
	}
	return out
}

// filterOption is one usable <select> option on a listing page.
type filterOption struct {
	Name  string
	Value string
	Text  string
}

// selectOptions parses every named <select> on a page and returns its options
// with a non-empty label and value.
func selectOptions(body []byte) []filterOption {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	var out []filterOption
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "select" {
			if name := attr(n, "name"); name != "" {
				for o := n.FirstChild; o != nil; o = o.NextSibling {
					if o.Type != html.ElementNode || o.Data != "option" {
						continue
					}
					val := strings.TrimSpace(attr(o, "value"))
					label := strings.TrimSpace(nodeText(o))
					if val == "" || label == "" {
						continue
					}
					out = append(out, filterOption{Name: name, Value: val, Text: label})
				}
			}
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(doc)
	return out
}

// socialHosts are share widgets and social profiles; their URLs carry doc
// names as query params and are never filings.
var socialHosts = []string{
	"facebook.com", "twitter.com", "x.com", "linkedin.com", "wa.me",
	"whatsapp.com", "youtube.com", "instagram.com", "t.me", "telegram.me",
	"pinterest.com", "sharer.php",
}

func isSocialURL(u string) bool {
	lower := strings.ToLower(u)
	for _, h := range socialHosts {
		if strings.Contains(lower, h) {
			return true
		}
	}
	return false
}

// absoluteURL resolves href against the page URL, keeping only http(s) links.
func absoluteURL(base *url.URL, href string) string {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "mailto:") || strings.HasPrefix(href, "javascript:") || strings.HasPrefix(href, "tel:") {
		return ""
	}
	ref, err := url.Parse(href)
	if err != nil {
		return ""
	}
	res := base.ResolveReference(ref)
	if res.Scheme != "http" && res.Scheme != "https" {
		return ""
	}
	res.Fragment = ""
	res.User = nil
	return res.String()
}

// rankSubPages picks internal pages likely to list filings (annual reports,
// presentations, results, transcripts), excluding PDFs.
func rankSubPages(irURL string, links []candidate, limit int) []string {
	base, err := url.Parse(irURL)
	if err != nil {
		return nil
	}
	irHost := strings.ToLower(base.Hostname())

	type scored struct {
		u     string
		score int
	}
	var subs []scored
	seen := map[string]bool{}
	for _, cand := range links {
		u, err := url.Parse(cand.URL)
		if err != nil || !sameSite(u.Hostname(), irHost) {
			continue
		}
		if isPDF(cand.URL) || cand.URL == irURL || seen[cand.URL] {
			continue
		}
		seen[cand.URL] = true
		if _, paginated := u.Query()["page"]; paginated {
			continue // pagination links, not content pages
		}
		if s := subPageScore(cand); s > 0 {
			subs = append(subs, scored{cand.URL, s})
		}
	}
	sort.Slice(subs, func(i, j int) bool {
		if subs[i].score != subs[j].score {
			return subs[i].score > subs[j].score
		}
		return subs[i].u < subs[j].u
	})
	if len(subs) > limit {
		subs = subs[:limit]
	}
	out := make([]string, 0, len(subs))
	for _, s := range subs {
		out = append(out, s.u)
	}
	return out
}

// sameSiteURL reports whether two absolute URLs share a registrable domain.
func sameSiteURL(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil {
		return false
	}
	return sameSite(ua.Hostname(), ub.Hostname())
}

// sameSite matches hosts that share a registrable suffix (ir.ril.com vs
// ril.com vs www.ril.com).
func sameSite(a, b string) bool {
	if strings.EqualFold(a, b) {
		return true
	}
	la, lb := strings.Split(a, "."), strings.Split(b, ".")
	if len(la) < 2 || len(lb) < 2 {
		return false
	}
	return strings.EqualFold(strings.Join(la[len(la)-2:], "."), strings.Join(lb[len(lb)-2:], "."))
}

func subPageScore(cand candidate) int {
	joined := normKeywords(cand.Text + " " + cand.URL)
	score := 0
	for _, kw := range []string{
		"annual report", "presentations", "presentation", "results", "financials",
		"financial result", "financial information", "quarterly", "transcript",
		"reports", "performance", "filing", "annual", "financial",
	} {
		if strings.Contains(joined, kw) {
			score += 2
		}
	}
	return score
}

// classify maps a link to a segment by keyword, or "" when it matches none.
func classify(cand candidate) string {
	joined := normKeywords(cand.Text + " " + cand.URL)
	for _, bad := range denylist {
		if strings.Contains(joined, bad) {
			return ""
		}
	}
	for _, m := range segmentMatchers {
		for _, kw := range m.kw {
			if strings.Contains(joined, kw) {
				return m.seg
			}
		}
	}
	return ""
}

// selectFilings classifies every candidate and keeps the most recent few per
// segment (latest first, so downloads fall back to slightly older filings).
func selectFilings(cands []candidate) map[string][]filing {
	bySeg := map[string][]filing{}
	for _, cand := range cands {
		seg := classify(cand)
		if seg == "" {
			continue
		}
		bySeg[seg] = append(bySeg[seg], filing{
			Segment: seg,
			URL:     cand.URL,
			Text:    cand.Text,
			PageURL: cand.PageURL,
		})
	}
	for seg, list := range bySeg {
		sort.Slice(list, func(i, j int) bool { return recencyLess(list[i], list[j]) })
		bySeg[seg] = list
	}
	return bySeg
}

// missingSegments lists segments with no pick.
func missingSegments(picks map[string][]filing) []string {
	var out []string
	for _, seg := range segmentOrder {
		if len(picks[seg]) == 0 {
			out = append(out, seg)
		}
	}
	return out
}

// expandListings replaces non-PDF picks (listing / flipbook pages) with the
// segment-relevant PDFs linked on those pages, so downloads only chase real
// documents. Results are cached per page URL across calls; mu guards the
// cache when harvests also run from fetchAndParse workers.
func (c *Client) expandListings(ctx context.Context, co models.Company, picks map[string][]filing, cache map[string][]filing, mu *sync.Mutex) map[string][]filing {
	for seg, list := range picks {
		var pdfs, listings []filing
		taken := map[string]bool{}
		keepListing := 0
		for _, f := range list {
			if taken[f.URL] {
				continue
			}
			taken[f.URL] = true
			if isPDF(f.URL) {
				pdfs = append(pdfs, f)
				continue
			}
			if keepListing >= maxListings {
				continue // too many listing pages already
			}
			keepListing++
			if !sameSiteURL(f.URL, f.PageURL) {
				continue // external page (share widget etc), not a listing
			}
			for _, g := range c.expandedListing(ctx, co, seg, f, cache, mu) {
				if taken[g.URL] {
					continue
				}
				taken[g.URL] = true
				pdfs = append(pdfs, g)
			}
			listings = append(listings, f)
		}
		// listing pages trail the harvested PDFs as last-resort picks; a
		// failed download just falls through to the next candidate
		sort.Slice(pdfs, func(i, j int) bool { return recencyLess(pdfs[i], pdfs[j]) })
		sort.Slice(listings, func(i, j int) bool { return recencyLess(listings[i], listings[j]) })
		merged := append(pdfs, listings...)
		if len(merged) > 0 {
			picks[seg] = merged
		} else {
			delete(picks, seg)
		}
	}
	return picks
}

// pdfsOnPage fetches a listing page and returns the documents on it that
// belong to seg: PDFs matching the segment plus download endpoints (e.g.
// /print/pdf/<id>) which have no .pdf suffix but still serve a document.
// When the page itself offers no such document it goes one step further and
// follows the record-list pagination the page's payload advertises, so
// filings hidden behind a "show more" API are still discovered.
func (c *Client) pdfsOnPage(ctx context.Context, co models.Company, seg string, listing filing) []filing {
	body, err := c.fetchHTML(ctx, listing.URL)
	if err != nil {
		log.Printf("[ir] %s: %s: expand listing %s: %v", co.Symbol, seg, listing.URL, err)
		return nil
	}
	base, err := url.Parse(listing.URL)
	if err != nil {
		return nil
	}

	out := docLinksOnBody(base, body, listing.URL, seg)
	if len(out) == 0 {
		out = c.paginatedListingDocs(ctx, co, seg, listing, body)
	}
	return out
}

// docLinksOnBody harvests the documents on one fetched page that belong to
// seg: segment-relevant PDFs plus download endpoints.
func docLinksOnBody(base *url.URL, body []byte, pageURL, seg string) []filing {
	var out []filing
	for _, cand := range pageCandidates(base, body, pageURL) {
		if !isDocLink(seg, cand) {
			continue
		}
		out = append(out, filing{Segment: seg, URL: cand.URL, Text: cand.Text, PageURL: pageURL})
	}
	return out
}

// isDocLink reports whether a link found on a listing page is a document worth
// downloading for seg. A .pdf link must carry a segment marker; a link to a
// download endpoint without the extension is trusted because the page itself
// was already classified into seg.
func isDocLink(seg string, cand candidate) bool {
	if isPDF(cand.URL) {
		return pdfRelevance(seg, cand) > 0
	}
	return isDownloadLink(cand)
}

// isDownloadLink detects links that trigger a document download even though
// the URL has no .pdf suffix (IR sites commonly serve files from /print/pdf/
// or ?download= endpoints).
func isDownloadLink(cand candidate) bool {
	s := strings.ToLower(cand.Text + " " + cand.URL)
	for _, hint := range []string{
		"download pdf", "downloadpdf", "print/pdf", "getpdf", "download file",
		"downloadfile", "?download=", "/downloadfile", "/file/download",
	} {
		if strings.Contains(s, hint) {
			return true
		}
	}
	return false
}

// pdfRelevance scores how strongly a PDF link matches a segment (0 = no
// match); used to filter and rank PDFs found on listing pages.
func pdfRelevance(seg string, cand candidate) int {
	joined := normKeywords(cand.Text + " " + cand.URL)
	score := 0
	for _, m := range segmentKeywords[seg] {
		if m.weight > score && strings.Contains(joined, m.kw) {
			score = m.weight
		}
	}
	return score
}

// segmentKeywords are the weighted per-segment markers used when ranking
// PDFs harvested from listing pages (higher weight = stronger match).
var segmentKeywords = map[string][]struct {
	kw     string
	weight int
}{
	SegAnnualReport: {{"annual report", 3}, {"integrated annual", 3}, {"iar", 2}},
	SegPresentation: {{"investor presentation", 3}, {"presentation", 2}},
	SegTranscript:   {{"transcript", 3}, {"earnings call", 2}, {"con-call", 2}, {"concall", 2}},
	SegQuarterly:    {{"financial result", 3}, {"quarterly result", 3}, {"quarterly financial", 3}, {"financial", 2}, {"quarter", 1}},
}

// recencyLess orders filings by detected fiscal recency (year, then
// sub-year bucket), then segment relevance, preferring PDFs.
func recencyLess(a, b filing) bool {
	ay, asub := recency(a.URL + " " + a.Text)
	by, bsub := recency(b.URL + " " + b.Text)
	if ay != by {
		return ay > by
	}
	if asub != bsub {
		return asub > bsub
	}
	if ra, rb := pdfRelevance(a.Segment, candidate{URL: a.URL, Text: a.Text}),
		pdfRelevance(b.Segment, candidate{URL: b.URL, Text: b.Text}); ra != rb {
		return ra > rb
	}
	if ap, bp := isPDF(a.URL), isPDF(b.URL); ap != bp {
		return ap
	}
	return a.URL < b.URL
}

func isPDF(u string) bool {
	return strings.HasSuffix(strings.ToLower(u), ".pdf")
}

// recency extracts (year, sub) signals from text+url: year and a sub-year
// bucket (1-4, from quarter or month markers). Defaults to (0, 0) when
// nothing is detected. Full dates ("June 30, 2026", "30.06.2026") are the
// most precise signal; URL-escaped spaces are undone first so names like
// "RIL_IAR%202026.pdf" still yield 2026.
func recency(s string) (year, sub int) {
	s = strings.ReplaceAll(s, "%20", " ")
	lower := strings.ToLower(s)

	if y, m, ok := fullDate(lower); ok {
		return clampYear(y), monthSub(m)
	}
	if m := yearRange.FindStringSubmatch(s); m != nil {
		first, _ := strconv.Atoi(m[1])
		second, _ := strconv.Atoi(m[2])
		if second < 100 {
			second += 2000
		}
		if second >= first { // 2025-26 is fiscal, 2026-07 is a month
			year = clampYear(second)
		}
	}
	if year == 0 {
		if m := fyPattern.FindStringSubmatch(lower); m != nil {
			n, _ := strconv.Atoi(m[1])
			if n < 100 {
				n += 2000
			}
			year = clampYear(n)
		}
	}
	if year == 0 {
		for _, m := range year4Pattern.FindAllString(s, -1) {
			y, _ := strconv.Atoi(m)
			y = clampYear(y)
			if y > year {
				year = y
			}
		}
	}

	if m := quarterlyHint.FindStringSubmatch(lower); m != nil {
		if q, _ := strconv.Atoi(m[1]); q > sub {
			sub = q
		}
	}
	return year, sub
}

// fullDate finds a month-named date ("June 30, 2026", "30 June 2026") or a
// numeric one ("30.06.2026", dd/mm preferred), returning (year, month).
func fullDate(lower string) (int, int, bool) {
	if m := monthDayYear.FindStringSubmatch(lower); m != nil {
		if month, ok := monthIndex[m[1]]; ok {
			return atoi(m[3]), month, true
		}
	}
	if m := dayMonthYear.FindStringSubmatch(lower); m != nil {
		if month, ok := monthIndex[m[2]]; ok {
			return atoi(m[3]), month, true
		}
	}
	if m := numericDate.FindStringSubmatch(lower); m != nil {
		day, month := atoi(m[1]), atoi(m[2])
		if day >= 1 && day <= 31 && month >= 1 && month <= 12 {
			return atoi(m[3]), month, true
		}
	}
	return 0, 0, false
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// monthSub buckets a month into a 1-4 quarter-like value for ordering.
func monthSub(month int) int {
	return (month + 2) / 3
}

func clampYear(y int) int {
	if y < 1990 || y > 2040 {
		return 0
	}
	return y
}

// llmAssist asks the LLM agent to fill segments keyword matching missed; the
// LLM must return indexes into the candidate list, so its answer is
// verifiable. Returns the merged picks.
func (c *Client) llmAssist(ctx context.Context, co models.Company, cands []candidate, picks map[string][]filing, missing []string) map[string][]filing {
	if c.llm == nil || llm.Exhausted(ctx) || len(cands) == 0 {
		return picks
	}

	var b strings.Builder
	listed := len(cands)
	if listed > maxCandidatesLLM {
		listed = maxCandidatesLLM
	}
	for i, cand := range cands[:listed] {
		fmt.Fprintf(&b, "%d. %s | %s\n", i, trunc(cand.Text, 80), trunc(cand.URL, 160))
	}

	name := orDefault(co.Name, co.Symbol)
	segDesc := ""
	for _, seg := range missing {
		switch seg {
		case SegAnnualReport:
			segDesc += "\n- annual_report: the latest full annual report"
		case SegPresentation:
			segDesc += "\n- investor_presentation: the latest investor/analyst presentation"
		case SegTranscript:
			segDesc += "\n- earnings_call_transcript: the latest earnings/con-call transcript"
		case SegQuarterly:
			segDesc += "\n- quarterly_financials: the latest quarterly financial results filing"
		}
	}
	system := "You are a precise research assistant. Reply with strict JSON only, no commentary."
	user := fmt.Sprintf(`Company %q (BSE symbol %s). Below are links from its investor relations pages. Pick the index (the number before the dot) of the best link for each of these segments:%s

Links:
%s

Reply with JSON mapping each segment to the chosen index or null, e.g. {"annual_report": 3, "investor_presentation": null, "earnings_call_transcript": null, "quarterly_financials": 7}. Only use indexes from the list. Prefer the most recent document.`, name, co.Symbol, segDesc, b.String())
	var raw json.RawMessage
	err := llm.CompleteJSON(ctx, c.llm, system, user, &raw)
	if err != nil {
		log.Printf("[ir] %s: llm classification failed: %v", co.Symbol, err)
		return picks
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		log.Printf("[ir] %s: llm classification unreadable: %v", co.Symbol, err)
		return picks
	}

	for _, seg := range missing {
		idx := pickIndex(out[seg])
		if idx < 0 || idx >= listed {
			continue
		}
		cand := cands[idx]
		if kwSeg := classify(cand); kwSeg != "" && kwSeg != seg {
			continue // keywords already assign this link elsewhere
		}
		if isPDF(cand.URL) && pdfRelevance(seg, cand) == 0 {
			continue // a PDF with no segment marker is too risky to trust
		}
		picks[seg] = append(picks[seg], filing{
			Segment: seg,
			URL:     cand.URL,
			Text:    cand.Text,
			PageURL: cand.PageURL,
		})
		sort.Slice(picks[seg], func(i, j int) bool { return recencyLess(picks[seg][i], picks[seg][j]) })
	}
	return picks
}

// pickIndex reads the index the LLM chose for one segment (number or digit
// string, null otherwise).
func pickIndex(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			return n
		}
	}
	return -1
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
