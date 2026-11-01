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
	"strings"
	"time"

	"vyasa/src/llm"
	"vyasa/src/models"

	"golang.org/x/net/html"
)

const ddgSearchURL = "https://html.duckduckgo.com/html/"

// linkSet is the resolved company_links payload.
type linkSet struct {
	Website string
	IR      string
}

// resolveLinks returns the company's official website and investor relations
// page, preferring the stored company_links document, then LLM guesses, then
// a DuckDuckGo search; every candidate is verified with a live fetch before
// being accepted, and the winner is persisted.
func (c *Client) resolveLinks(ctx context.Context, co models.Company) (linkSet, error) {
	existing := c.loadLinks(ctx, co)
	if existing != nil && existing.InvestorRelationsLink != "" && c.verifyPage(ctx, existing.InvestorRelationsLink, false) {
		ls := linkSet{Website: existing.OfficialWebsite, IR: existing.InvestorRelationsLink}
		return ls, nil
	}

	var seeds []string
	if existing != nil {
		if existing.InvestorRelationsLink != "" {
			seeds = append(seeds, existing.InvestorRelationsLink)
		}
	}

	var guesses linkSet
	if c.llm != nil && !llm.Exhausted(ctx) {
		guesses = c.llmLinks(ctx, co)
	}

	ir := ""
	for _, u := range dedupe(append(seeds, guesses.IR)) {
		if c.verifyPage(ctx, u, true) {
			ir = u
			break
		}
	}
	if ir == "" {
		for _, u := range c.ddgResults(ctx, co, " investor relations", true) {
			if c.verifyPage(ctx, u, true) {
				ir = u
				break
			}
		}
	}
	if ir == "" {
		return linkSet{}, fmt.Errorf("could not resolve investor relations link")
	}

	website := ""
	for _, u := range dedupe([]string{guesses.Website, existingWebsite(existing)}) {
		if u != "" && c.verifyPage(ctx, u, false) {
			website = u
			break
		}
	}
	if website == "" {
		website = baseSite(ir)
	}

	ls := linkSet{Website: website, IR: ir}
	if err := c.persistLinks(ctx, co, ls); err != nil {
		return ls, err
	}
	return ls, nil
}

func existingWebsite(l *models.CompanyLinks) string {
	if l == nil {
		return ""
	}
	return l.OfficialWebsite
}

// loadLinks returns the stored company_links document or nil.
func (c *Client) loadLinks(ctx context.Context, co models.Company) *models.CompanyLinks {
	if c.store == nil {
		return nil
	}
	var l models.CompanyLinks
	err := c.store.Pool.QueryRow(ctx, `SELECT official_website, investor_relations_link
		FROM company_links WHERE company_id = $1`, co.ID).
		Scan(&l.OfficialWebsite, &l.InvestorRelationsLink)
	if err != nil {
		return nil
	}
	l.CompanyID = co.ID
	return &l
}

// persistLinks upserts the resolved links into company_links.
func (c *Client) persistLinks(ctx context.Context, co models.Company, ls linkSet) error {
	if c.store == nil {
		return nil
	}
	now := time.Now()
	_, err := c.store.Pool.Exec(ctx, `
		INSERT INTO company_links (company_id, official_website, investor_relations_link, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $4)
		ON CONFLICT (company_id) DO UPDATE SET
			official_website = COALESCE(NULLIF(EXCLUDED.official_website, ''), company_links.official_website),
			investor_relations_link = COALESCE(NULLIF(EXCLUDED.investor_relations_link, ''), company_links.investor_relations_link),
			updated_at = EXCLUDED.updated_at`,
		co.ID, ls.Website, ls.IR, now)
	if err != nil {
		return fmt.Errorf("ir: save company links: %w", err)
	}
	return nil
}

// llmLinks asks the LLM agent for the company's website and IR page.
func (c *Client) llmLinks(ctx context.Context, co models.Company) linkSet {
	name := co.Name
	if name == "" {
		name = co.Symbol
	}
	system := "You are a precise research assistant. Reply with strict JSON only, no commentary."
	user := fmt.Sprintf(`What are the official website and the investor relations page of the Indian listed company %q (BSE symbol %s)?
Reply with JSON: {"website": "https://... or null", "investor_relations": "https://... or null"}
Only give URLs you are confident about. The investor relations page is the section hosting annual reports, investor presentations and quarterly results.`, name, co.Symbol)
	var raw json.RawMessage
	err := llm.CompleteJSON(ctx, c.llm, system, user, &raw)
	if err != nil {
		log.Printf("[ir] %s: llm link guess failed: %v", co.Symbol, err)
		return linkSet{}
	}
	var out struct {
		Website           string `json:"website"`
		InvestorRelations string `json:"investor_relations"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		log.Printf("[ir] %s: llm link guess unreadable: %v", co.Symbol, err)
		return linkSet{}
	}
	return linkSet{Website: normalizeURL(out.Website), IR: normalizeURL(out.InvestorRelations)}
}

// ddgResults queries DuckDuckGo's HTML endpoint and returns up to six
// external result URLs.
func (c *Client) ddgResults(ctx context.Context, co models.Company, suffix string, needInvestorWord bool) []string {
	name := co.Name
	if name == "" {
		name = co.Symbol
	}
	q := url.Values{}
	q.Set("q", name+suffix)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ddgSearchURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil
	}
	c.setBrowseHeaders(req, "")

	resp, err := c.hc.Do(req)
	if err != nil {
		log.Printf("[ir] %s: duckduckgo search: %v", co.Symbol, err)
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes))
	if err != nil {
		return nil
	}

	var out []string
	for _, n := range anchors(body) {
		href := attr(n, "href")
		if strings.HasPrefix(href, "//duckduckgo.com/l/") || strings.Contains(href, "duckduckgo.com/l/") {
			if u := extractDDGRedirect(href); u != "" {
				href = u
			}
		}
		if !strings.HasPrefix(href, "http") || strings.Contains(href, "duckduckgo.com") {
			continue
		}
		if needInvestorWord && !strings.Contains(strings.ToLower(href+nodeText(n)), "invest") {
			continue
		}
		out = append(out, normalizeURL(href))
		if len(out) >= 6 {
			break
		}
	}
	return out
}

// extractDDGRedirect pulls the uddg= target out of a duckduckgo redirect.
func extractDDGRedirect(href string) string {
	u, err := url.Parse("https:" + href)
	if err != nil {
		return ""
	}
	return normalizeURL(u.Query().Get("uddg"))
}

// verifyPage fetches u and requires a 2xx HTML response; when needKeyword is
// set the body or host must mention "investor".
func (c *Client) verifyPage(ctx context.Context, u string, needKeyword bool) bool {
	if u == "" {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false
	}
	c.setBrowseHeaders(req, "")

	resp, err := c.hc.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	if !needKeyword {
		return true
	}
	if strings.Contains(strings.ToLower(u), "invest") {
		return true
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return false
	}
	return bytes.Contains(bytes.ToLower(body), []byte("invest"))
}

// baseSite derives the official website host from the IR URL, dropping
// ir./investors.-style subdomains.
func baseSite(irURL string) string {
	u, err := url.Parse(irURL)
	if err != nil || u.Host == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	labels := strings.Split(host, ".")
	if len(labels) > 2 {
		switch labels[0] {
		case "ir", "investors", "investor", "investorrelations":
			host = strings.Join(labels[1:], ".")
		}
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return scheme + "://" + host
}

// normalizeURL trims and validates a scheme-ful http(s) URL.
func normalizeURL(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		if strings.HasPrefix(u, "//") {
			u = "https:" + u
		} else {
			return ""
		}
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.Host == "" {
		return ""
	}
	parsed.Fragment = ""
	return parsed.String()
}

func dedupe(urls []string) []string {
	seen := make(map[string]bool, len(urls))
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		u = normalizeURL(u)
		if u == "" || seen[strings.ToLower(u)] {
			continue
		}
		seen[strings.ToLower(u)] = true
		out = append(out, u)
	}
	return out
}

// setBrowseHeaders makes requests look like a normal browser (many IR sites
// reject default Go clients).
func (c *Client) setBrowseHeaders(req *http.Request, referer string) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/pdf,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
}

// anchors parses an HTML document and returns its <a> nodes.
func anchors(body []byte) []*html.Node {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			for _, a := range n.Attr {
				if strings.EqualFold(a.Key, "href") && a.Val != "" {
					out = append(out, n)
					break
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

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

// nodeText collects the visible text of an HTML node (an <a> anchor or an
// <option> label).
func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}
