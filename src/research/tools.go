package research

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"vyasa/src/rag"
	"vyasa/src/types"

	"golang.org/x/net/html"
)

const (
	segmentAnnual       = "annual_report"
	segmentPresentation = "investor_presentation"
	webUserAgent        = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36"
)

type scopedTool struct {
	name    string
	desc    string
	segment string
	inner   types.Tool
}

var _ types.Tool = (*scopedTool)(nil)

func (t *scopedTool) Name() string        { return t.name }
func (t *scopedTool) Description() string { return t.desc }
func (t *scopedTool) Parameters() json.RawMessage {
	// The inner parameters already carry a segment field; it is overridden
	// server-side on every call.
	return t.inner.Parameters()
}

func (t *scopedTool) Execute(ctx context.Context, args string) types.ExecutionOutput {
	var doc map[string]any
	if err := json.Unmarshal([]byte(args), &doc); err != nil {
		return t.inner.Execute(ctx, args) // the inner tool reports malformed args
	}
	doc["segment"] = t.segment
	buf, err := json.Marshal(doc)
	if err != nil {
		return t.inner.Execute(ctx, args)
	}
	return t.inner.Execute(ctx, string(buf))

}

func scopedRAGRegistry(d Deps, segment, prefix, label string) *types.Registry {
	reg := types.NewRegistry()
	reg.Register(&scopedTool{
		name: prefix + "_rag_search",
		desc: "Search the company's archived " + label + " for narrative passages " +
			"(commentary, strategy, risks, business description). Returns relevant " +
			"passages with section and fiscal period. For statement tables use " +
			prefix + "_rag_table_search instead.",
		segment: segment,
		inner:   rag.NewSearchTool(d.St, d.Indexer),
	})
	reg.Register(&scopedTool{
		name: prefix + "_rag_table_search",
		desc: "Search the financial tables extracted from the company's archived " + label +
			" (P&L, balance sheet, cash flow, segment grids). Returns whole tables as " +
			"markdown with caption, page and period. For narrative use " + prefix +
			"_rag_search instead.",
		segment: segment,
		inner:   rag.NewTableSearchTool(d.St, d.Indexer),
	})
	return reg
}

// WebTools returns the open-web agent tools for the verifier specialist:
// web_search (DuckDuckGo, no API key) and web_fetch (URL text extraction).
func WebTools() []types.Tool {
	hc := &http.Client{Timeout: 30 * time.Second}
	return []types.Tool{&webSearchTool{client: hc}, &webFetchTool{client: hc}}
}

type webSearchTool struct {
	client *http.Client
}

var _ types.Tool = (*webSearchTool)(nil)

func (t *webSearchTool) Name() string { return "web_search" }

func (t *webSearchTool) Description() string {
	return "Search the open web (DuckDuckGo) and return the top result titles, " +
		"snippets and URLs. Use for recent news, prices and anything the " +
		"archived filings do not cover."
}

func (t *webSearchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query":       {"type": "string", "description": "web search query"},
			"max_results": {"type": "integer", "description": "max results to return (default 6, cap 10)"}
		},
		"required": ["query"]
	}`)
}

type webResult struct {
	Title, URL, Snippet string
}

func (t *webSearchTool) Execute(ctx context.Context, args string) types.ExecutionOutput {
	start := time.Now()
	var req struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	if err := json.Unmarshal([]byte(args), &req); err != nil {
		return errOut(fmt.Sprintf("decode args: %v", err), start)
	}
	req.Query = strings.TrimSpace(req.Query)
	if req.Query == "" {
		return errOut("query is required", start)
	}
	if req.MaxResults <= 0 {
		req.MaxResults = 6
	}
	if req.MaxResults > 10 {
		req.MaxResults = 10
	}

	// The HTML endpoint first; the lite endpoint is the fallback when the
	// primary returns a challenge page or no results.
	results, err := t.search("https://html.duckduckgo.com/html/", req.Query)
	if err != nil || len(results) == 0 {
		if alt, aerr := t.search("https://lite.duckduckgo.com/lite/", req.Query); aerr == nil && len(alt) > 0 {
			results = alt
			err = nil
		}
	}
	if err != nil {
		return errOut("web search failed: "+err.Error(), start)
	}
	if len(results) == 0 {
		return types.ExecutionOutput{Output: "no results found for query: " + req.Query, Duration: time.Since(start)}
	}
	if len(results) > req.MaxResults {
		results = results[:req.MaxResults]
	}

	var b strings.Builder
	for i, r := range results {
		fmt.Fprintf(&b, "[%d] %s\n%s\n%s\n\n", i+1, r.Title, r.Snippet, r.URL)
	}
	return types.ExecutionOutput{Output: strings.TrimSpace(b.String()), Duration: time.Since(start)}
}

func (t *webSearchTool) search(endpoint, query string) ([]webResult, error) {
	form := url.Values{"q": {query}}
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", webUserAgent)
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", endpoint, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	root, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	return parseWebResults(root), nil
}

// parseWebResults walks the DuckDuckGo HTML/Lite result page in document
// order, pairing each result link with the snippet that follows it. Covers
// both layouts: result__a/result__snippet (HTML) and
// result-link/result-snippet (Lite).
func parseWebResults(root *html.Node) []webResult {
	var out []webResult
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			cls := nodeAttr(n, "class")
			switch {
			case n.Data == "a" && (strings.Contains(cls, "result__a") || strings.Contains(cls, "result-link")):
				if title := strings.TrimSpace(nodeText(n)); title != "" {
					out = append(out, webResult{Title: title, URL: unwrapDDGLink(nodeAttr(n, "href"))})
				}
			// The snippet element differs per layout: <a class="result__snippet">
			// on the HTML endpoint, <td class="result-snippet"> on Lite.
			case strings.Contains(cls, "result__snippet"), strings.Contains(cls, "result-snippet"):
				if len(out) > 0 && out[len(out)-1].Snippet == "" {
					out[len(out)-1].Snippet = strings.TrimSpace(nodeText(n))
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return out
}

// unwrapDDGLink resolves DuckDuckGo's redirect links (/l/?uddg=...) to the
// real target URL and upgrades protocol-relative hrefs.
func unwrapDDGLink(href string) string {
	if href == "" {
		return ""
	}
	if strings.Contains(href, "uddg=") {
		if u, err := url.Parse(href); err == nil {
			if d := u.Query().Get("uddg"); d != "" {
				return d
			}
		}
	}
	if strings.HasPrefix(href, "//") {
		return "https:" + href
	}
	return href
}

// --- web_fetch ----------------------------------------------------------

type webFetchTool struct {
	client *http.Client
}

var _ types.Tool = (*webFetchTool)(nil)

func (t *webFetchTool) Name() string { return "web_fetch" }

func (t *webFetchTool) Description() string {
	return "Download a web page (or plain-text file) by URL and return its " +
		"readable text with HTML stripped. Use after web_search to read a " +
		"promising result, or on a known document URL."
}

func (t *webFetchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"url":       {"type": "string", "description": "http(s) URL to fetch"},
			"max_chars": {"type": "integer", "description": "cap on returned text length (default 6000)"}
		},
		"required": ["url"]
	}`)
}

func (t *webFetchTool) Execute(ctx context.Context, args string) types.ExecutionOutput {
	start := time.Now()
	var req struct {
		URL      string `json:"url"`
		MaxChars int    `json:"max_chars"`
	}
	if err := json.Unmarshal([]byte(args), &req); err != nil {
		return errOut(fmt.Sprintf("decode args: %v", err), start)
	}
	u, err := url.Parse(strings.TrimSpace(req.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errOut("url must be an absolute http(s) URL", start)
	}
	if req.MaxChars <= 0 {
		req.MaxChars = 6000
	}
	if req.MaxChars > 20000 {
		req.MaxChars = 20000
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
	if err != nil {
		return errOut("build request: "+err.Error(), start)
	}
	httpReq.Header.Set("User-Agent", webUserAgent)
	resp, err := t.client.Do(httpReq)
	if err != nil {
		return errOut("fetch failed: "+err.Error(), start)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errOut(fmt.Sprintf("fetch %s: HTTP %d", req.URL, resp.StatusCode), start)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return errOut("read body: "+err.Error(), start)
	}

	text := string(body)
	if isHTML(resp.Header.Get("Content-Type"), text) {
		root, perr := html.Parse(bytes.NewReader(body))
		if perr != nil {
			return errOut("parse HTML: "+perr.Error(), start)
		}
		text = extractVisibleText(root)
	}
	if len(text) > req.MaxChars {
		text = text[:req.MaxChars] + "\n...[truncated]"
	}
	return types.ExecutionOutput{
		Output:   "URL: " + req.URL + "\n\n" + strings.TrimSpace(text),
		Duration: time.Since(start),
	}
}

func isHTML(contentType, body string) bool {
	if strings.Contains(contentType, "html") {
		return true
	}
	if contentType == "" {
		head := body
		if len(head) > 512 {
			head = head[:512]
		}
		head = strings.ToLower(head)
		return strings.Contains(head, "<!doctype html") || strings.Contains(head, "<html")
	}
	return false
}

var (
	blockTags = map[string]bool{
		"p": true, "div": true, "br": true, "li": true, "tr": true, "table": true,
		"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
		"section": true, "article": true, "header": true, "footer": true,
		"ul": true, "ol": true, "blockquote": true,
	}
	skipTags = map[string]bool{
		"script": true, "style": true, "noscript": true,
		"template": true, "svg": true, "iframe": true,
	}
)

// extractVisibleText renders an HTML document's visible text, one trimmed
// line per block element.
func extractVisibleText(root *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if skipTags[n.Data] {
				return
			}
			if blockTags[n.Data] {
				b.WriteString("\n")
			}
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			b.WriteString(" ")
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && blockTags[n.Data] {
			b.WriteString("\n")
		}
	}
	walk(root)
	return collapseBlank(b.String())
}

// collapseBlank trims every line, collapses internal whitespace runs and
// drops empty ones.
func collapseBlank(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		ln = strings.Join(strings.Fields(ln), " ")
		if ln != "" {
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}

// nodeAttr returns the value of the first attribute with the given name.
func nodeAttr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

// nodeText concatenates the text nodes beneath n.
func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// errOut builds a failed execution output (shared by the web tools).
func errOut(msg string, start time.Time) types.ExecutionOutput {
	return types.ExecutionOutput{Error: msg, Duration: time.Since(start)}
}
