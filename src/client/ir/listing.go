package ir

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"vyasa/src/models"
)

// Some IR sites are headless-CMS driven: their pages embed a record list
// (annual reports, presentations...) that only shows the first page, with the
// rest served by the CMS's own filter API. The payload of such a listing page
// carries the full contract:
//
//	"pages":{"filters":{"url":"/filter-data","template_id":"20","limit":8},
//	         "current_page":1,"total_pages":4,"limit":8,"total_items":32}
//	"conditions":[],"tag_conditions":[{"id":"30","value":"478"}],
//	"sort":{"sort_by":"published_date","sort_type":"desc"}
//
// listingPagination captures that contract so the remaining pages can be
// fetched and mined like any other page.
type listingPagination struct {
	Path          string // API path, resolved against the listing page
	TemplateID    string
	Limit         int
	TotalPages    int
	Conditions    string // raw JSON carried into the request body
	TagConditions string
	Sort          string
	Store         string // CMS session context, when the payload carries it
	Locale        string
}

// listingPagesRe locates the "pages" object of the pagination contract.
var listingPagesRe = regexp.MustCompile(`"pages"\s*:\s*`)

// pagesPayload mirrors the fields the miner needs from the "pages" object;
// template_id and limit arrive as string or number depending on the site.
type pagesPayload struct {
	Filters struct {
		URL        string          `json:"url"`
		TemplateID json.RawMessage `json:"template_id"`
		Limit      json.RawMessage `json:"limit"`
	} `json:"filters"`
	CurrentPage int `json:"current_page"`
	TotalPages  int `json:"total_pages"`
	TotalItems  int `json:"total_items"`
}

// findListingPagination extracts the pagination contract from a flattened
// payload, or nil when the page does not advertise one. Sibling request
// fields (conditions, tag_conditions, sort) are taken from the nearest
// occurrence after the pages object, within a bounded window.
func findListingPagination(flat string) *listingPagination {
	for _, loc := range listingPagesRe.FindAllStringIndex(flat, 32) {
		obj, ok := jsonObjectAt(flat, loc[1])
		if !ok {
			continue
		}
		var p pagesPayload
		if json.Unmarshal([]byte(obj), &p) != nil {
			continue
		}
		if !strings.HasPrefix(p.Filters.URL, "/") || p.TotalPages < 2 || p.TotalItems <= 0 {
			continue
		}
		tid, err := rawString(p.Filters.TemplateID)
		if err != nil || tid == "" {
			continue
		}
		limit, err := rawInt(p.Filters.Limit, 0)
		if err != nil || limit <= 0 {
			continue
		}

		tail := flat[loc[1]+len(obj):]
		if len(tail) > 4000 {
			tail = tail[:4000]
		}
		spec := &listingPagination{
			Path:          p.Filters.URL,
			TemplateID:    tid,
			Limit:         limit,
			TotalPages:    p.TotalPages,
			Conditions:    jsonFieldValue(tail, "conditions"),
			TagConditions: jsonFieldValue(tail, "tag_conditions"),
			Sort:          jsonFieldValue(tail, "sort"),
			Store:         jsonScalarValue(tail, "store"),
			Locale:        jsonScalarValue(tail, "locale"),
		}
		return spec
	}
	return nil
}

// rawString reads a JSON string-or-number raw value as a plain string.
func rawString(raw json.RawMessage) (string, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "", fmt.Errorf("empty")
	}
	if s[0] == '"' {
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", err
		}
		return v, nil
	}
	if _, err := strconv.Atoi(s); err != nil {
		return "", err
	}
	return s, nil
}

// rawInt reads a JSON string-or-number raw value as an int.
func rawInt(raw json.RawMessage, def int) (int, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return def, fmt.Errorf("empty")
	}
	if s[0] == '"' {
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return 0, err
		}
		s = strings.TrimSpace(v)
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// jsonObjectAt returns the JSON object or array that starts at i (which must
// point at '{' or '['), as the raw substring including delimiters.
func jsonObjectAt(s string, i int) (string, bool) {
	if i >= len(s) || (s[i] != '{' && s[i] != '[') {
		return "", false
	}
	open, closer := s[i], byte('}')
	if open == '[' {
		closer = ']'
	}
	depth, inStr, esc := 0, false, false
	for j := i; j < len(s); j++ {
		c := s[j]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case open:
			depth++
		case closer:
			depth--
			if depth == 0 {
				return s[i : j+1], true
			}
		}
	}
	return "", false
}

// jsonFieldValue returns the raw JSON array/object bound to key in s
// (first occurrence), or "" when the key is absent or holds a scalar.
func jsonFieldValue(s, key string) string {
	i := indexKey(s, key)
	if i < 0 {
		return ""
	}
	v, ok := jsonObjectAt(s, i)
	if !ok {
		return ""
	}
	return v
}

// jsonScalarValue returns the string value bound to key in s (first
// occurrence), or "" when absent.
func jsonScalarValue(s, key string) string {
	i := indexKey(s, key)
	if i < 0 {
		return ""
	}
	var v string
	if err := json.Unmarshal([]byte(s[i:]), &v); err != nil {
		return ""
	}
	return v
}

// indexKey returns the position of the value bound to key in s (first
// occurrence of `"key":`, just past the colon), or -1.
func indexKey(s, key string) int {
	needle := `"` + key + `":`
	i := strings.Index(s, needle)
	if i < 0 {
		return -1
	}
	return i + len(needle)
}

// responseTotalPages reads total_pages out of a listing API response.
var totalPagesRe = regexp.MustCompile(`"total_pages"\s*:\s*(\d+)`)

func responseTotalPages(body []byte) int {
	if m := totalPagesRe.FindSubmatch(body); m != nil {
		n, _ := strconv.Atoi(string(m[1]))
		return n
	}
	return 0
}

// paginatedListingDocs fetches the record pages a listing page advertises and
// returns the ones that belong to seg. The API request body is rebuilt from
// the contract embedded in the page, so no endpoint knowledge is hardcoded.
func (c *Client) paginatedListingDocs(ctx context.Context, co models.Company, seg string, listing filing, body []byte) []filing {
	spec := findListingPagination(jsonFlatten.Replace(string(body)))
	if spec == nil {
		return nil
	}
	log.Printf("[ir] %s: %s: listing %s advertises %d pages of records, fetching the rest",
		co.Symbol, seg, listing.URL, spec.TotalPages)

	base, err := url.Parse(listing.URL)
	if err != nil {
		return nil
	}

	var out []filing
	seen := map[string]bool{}
	for _, resp := range c.fetchListingPages(ctx, co, listing.URL, spec) {
		for _, cand := range jsonCandidates(base, resp, listing.URL) {
			if seen[cand.URL] || !isDocLink(seg, cand) {
				continue
			}
			seen[cand.URL] = true
			out = append(out, filing{Segment: seg, URL: cand.URL, Text: cand.Text, PageURL: listing.URL})
		}
	}
	return out
}

// fetchListingPages resolves the listing API endpoint and returns up to
// maxListingPages record responses (page 1 first, newest records first).
func (c *Client) fetchListingPages(ctx context.Context, co models.Company, listingURL string, spec *listingPagination) [][]byte {
	endpoint, withCtx, first, ok := c.probeListingAPI(ctx, co, listingURL, spec)
	if !ok {
		return nil
	}

	limit := spec.Limit
	if limit < listingRecordLimit {
		limit = listingRecordLimit
	}
	out := [][]byte{first}
	for page := 2; page <= maxListingPages; page++ {
		if page > responseTotalPages(first) {
			break
		}
		body, err := c.postListingQuery(ctx, endpoint, spec, page, limit, withCtx)
		if err != nil || !isRecordList(body) {
			log.Printf("[ir] %s: listing api %s page %d: %v", co.Symbol, endpoint, page, err)
			break
		}
		out = append(out, body)
		if page >= responseTotalPages(body) {
			break
		}
	}
	return out
}

// probeListingAPI finds a working endpoint for spec by trying the common
// spellings of the API base (page origin, /api/ prefix, api.<domain> host),
// each first without and then with the CMS's default session context. It
// returns the endpoint, whether the session context was needed, and the page
// 1 response body.
func (c *Client) probeListingAPI(ctx context.Context, co models.Company, listingURL string, spec *listingPagination) (endpoint string, withCtx bool, first []byte, ok bool) {
	for _, ep := range listingEndpoints(listingURL, spec.Path) {
		for attempt := 0; attempt < 2; attempt++ {
			body, err := c.postListingQuery(ctx, ep, spec, 1, listingRecordLimit, attempt > 0)
			if err != nil {
				log.Printf("[ir] %s: listing api %s: %v", co.Symbol, ep, err)
				break
			}
			if isRecordList(body) {
				return ep, attempt > 0, body, true
			}
			if attempt == 0 && spec.Store == "" && spec.Locale == "" && needsStoreContext(body) {
				continue // retry the same endpoint with the default context
			}
			break // answered, but not a record list: wrong endpoint
		}
	}
	return "", false, nil, false
}

// postListingQuery POSTs one record-list query to the listing API and returns
// the response body regardless of status, so callers can tell a store/context
// demand (a retryable answer) from a wrong endpoint.
func (c *Client) postListingQuery(ctx context.Context, endpoint string, spec *listingPagination, page, limit int, withCtx bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(listingRequestBody(spec, page, limit, withCtx)))
	if err != nil {
		return nil, err
	}
	c.setBrowseHeaders(req, "")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("ngrok-skip-browser-warning", "1")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, maxPageBytes))
}

// listingRequestBody builds the record-list query from the page's own
// contract. The session context (store/locale) is only added on retry: the
// CMS behind these sites rejects anonymous queries, and its client bundle
// defaults every visitor to store "global", locale "en".
func listingRequestBody(spec *listingPagination, page, limit int, withCtx bool) string {
	b := fmt.Sprintf(`{"template_id":%q,"page":%d,"limit":%d,"tags":{},"conditions":%s,"tag_conditions":%s,"sort":%s`,
		spec.TemplateID, page, limit, rawOrDefault(spec.Conditions, "[]"), rawOrDefault(spec.TagConditions, "[]"), rawOrDefault(spec.Sort, "{}"))
	if spec.Store != "" {
		b += fmt.Sprintf(`,"store":%q`, spec.Store)
	}
	if spec.Locale != "" {
		b += fmt.Sprintf(`,"locale":%q,"lang":%q`, spec.Locale, spec.Locale)
	}
	if withCtx {
		b += `,"store":"global","locale":"en","lang":"en"`
	}
	return b + "}"
}

// rawOrDefault returns raw when it is a JSON container, else def.
func rawOrDefault(raw, def string) string {
	if raw != "" && (raw[0] == '{' || raw[0] == '[') {
		return raw
	}
	return def
}

// isRecordList reports whether a response is a record-list document.
func isRecordList(body []byte) bool {
	s := strings.TrimSpace(string(body))
	return strings.HasPrefix(s, "{") && strings.Contains(s, `"list":[`)
}

// needsStoreContext reports whether an API error demands a store/locale
// session context, which the retry then supplies.
func needsStoreContext(body []byte) bool {
	flat := jsonFlatten.Replace(string(body))
	return strings.Contains(flat, `"status":false`) &&
		strings.Contains(flat, "store") && strings.Contains(flat, "locale")
}

// listingEndpoints lists the candidate URLs for the listing API, derived from
// the page the contract was found on and the API path it advertises.
func listingEndpoints(listingURL, apiPath string) []string {
	u, err := url.Parse(listingURL)
	if err != nil || u.Host == "" || apiPath == "" {
		return nil
	}
	origin := u.Scheme + "://" + u.Host
	apiOrigin := ""
	if labels := strings.Split(u.Hostname(), "."); len(labels) >= 2 {
		apiHost := "api." + strings.Join(labels[len(labels)-2:], ".")
		if !strings.EqualFold(apiHost, u.Hostname()) {
			apiOrigin = u.Scheme + "://" + apiHost
		}
	}

	var out []string
	add := func(u string) {
		if normalizeURL(u) != "" {
			out = append(out, normalizeURL(u))
		}
	}
	if strings.HasPrefix(apiPath, "http://") || strings.HasPrefix(apiPath, "https://") {
		add(apiPath)
	} else if strings.HasPrefix(apiPath, "/") {
		add(origin + apiPath)
		add(origin + "/api" + apiPath)
		if apiOrigin != "" {
			add(apiOrigin + "/api" + apiPath)
			add(apiOrigin + apiPath)
		}
	} else {
		add(absoluteURL(u, apiPath))
	}
	return out
}
