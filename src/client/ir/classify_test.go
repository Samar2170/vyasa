package ir

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"vyasa/src/models"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		text, url, want string
	}{
		{"Annual Report 2024-25", "https://ir.example.com/AR2025.pdf", SegAnnualReport},
		{"Investor Presentation Q1 FY26", "https://ir.example.com/presentation.pdf", SegPresentation},
		{"Earnings Call Transcript Q4", "https://ir.example.com/transcript.pdf", SegTranscript},
		{"", "https://ir.example.com/Financial-Results-31-03-2025.pdf", SegQuarterly},
		{"Investor Grievance Report", "https://ir.example.com/grievance.pdf", ""},
		{"Code of Conduct Policy", "https://ir.example.com/policy.pdf", ""},
		{"Click here", "https://example.com/annualreport2025.pdf", SegAnnualReport},
	}
	for _, tc := range cases {
		got := classify(candidate{Text: tc.text, URL: tc.url})
		if got != tc.want {
			t.Errorf("classify(%q, %q) = %q, want %q", tc.text, tc.url, got, tc.want)
		}
	}
}

func TestRecency(t *testing.T) {
	cases := []struct {
		in            string
		year, quarter int
	}{
		{"Annual Report FY25", 2025, 0},
		{"https://x.com/ar-2024-25.pdf", 2025, 0},
		{"Annual Report 2023", 2023, 0},
		{"Investor Presentation Q2 FY26", 2026, 2},
		{"https://x.com/Q1FY24.pdf", 2024, 1},
		{"https://x.com/RIL_IAR%202026.pdf", 2026, 0},
		{"Integrated Filing (Financial) for the quarter ended June 30, 2026", 2026, 2},
		{"quarter and year ended March 31, 2026", 2026, 1},
		{"https://x.com/files/2025-10/SE_Integrated_Filing.pdf", 2025, 0},
		{"no signal here", 0, 0},
		{"garbage 12345", 0, 0},
	}
	for _, tc := range cases {
		y, q := recency(tc.in)
		if y != tc.year || q != tc.quarter {
			t.Errorf("recency(%q) = (%d, %d), want (%d, %d)", tc.in, y, q, tc.year, tc.quarter)
		}
	}
}

func TestSelectFilingsPrefersLatest(t *testing.T) {
	cands := []candidate{
		{URL: "https://ir.x.com/AR-2022.pdf", Text: "Annual Report"},
		{URL: "https://ir.x.com/AR-2025.pdf", Text: "Annual Report"},
		{URL: "https://ir.x.com/AR-2024.pdf", Text: "Annual Report"},
		{URL: "https://ir.x.com/presentations.html", Text: "Presentations"},
		{URL: "https://ir.x.com/Investor-Presentation-Q3-FY25.pdf", Text: "Investor Presentation"},
	}
	picks := selectFilings(cands)
	if len(picks[SegAnnualReport]) != 3 {
		t.Fatalf("annual picks = %d, want 3", len(picks[SegAnnualReport]))
	}
	if !strings.Contains(picks[SegAnnualReport][0].URL, "2025") {
		t.Fatalf("first annual pick = %s, want the 2025 report", picks[SegAnnualReport][0].URL)
	}
	if len(picks[SegPresentation]) != 2 {
		t.Fatalf("presentation picks = %d, want 2 (pdf + listing page fallback)", len(picks[SegPresentation]))
	}
	if !strings.Contains(picks[SegPresentation][0].URL, "Q3-FY25") {
		t.Fatalf("first presentation pick = %s, want the Q3-FY25 pdf", picks[SegPresentation][0].URL)
	}
	if len(picks[SegTranscript]) != 0 {
		t.Fatalf("transcript picks = %d, want 0", len(picks[SegTranscript]))
	}
}

func TestMissingSegments(t *testing.T) {
	got := missingSegments(map[string][]filing{
		SegAnnualReport: {{Segment: SegAnnualReport}},
	})
	if len(got) != 3 || got[0] != SegPresentation {
		t.Fatalf("missingSegments = %v, want presentation/transcript/quarterly", got)
	}
}

func TestCollapseBlankLines(t *testing.T) {
	in := "a\r\n\r\nb\n\n\n\n\nc\nd  \n"
	want := "a\n\nb\n\nc\nd"
	if got := collapseBlankLines(in); got != want {
		t.Fatalf("collapseBlankLines = %q, want %q", got, want)
	}
}

func TestSanitizeNameAndDocName(t *testing.T) {
	if got := sanitizeName("M&M Ltd"); got != "M&M_Ltd" {
		t.Fatalf("sanitizeName = %q", got)
	}
	day := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	want := "M&M_Ltd_annual_report_2026-09-11.pdf"
	if got := docName("M&M Ltd", SegAnnualReport, "pdf", day); got != want {
		t.Fatalf("docName = %q, want %q", got, want)
	}
}

func TestBaseSite(t *testing.T) {
	cases := map[string]string{
		"https://ir.tatamotors.com/investors":      "https://tatamotors.com",
		"https://www.ril.com/investor-relations":   "https://www.ril.com",
		"https://investors.example.co.in/reports":  "https://example.co.in",
		"https://company.com/investors/index.html": "https://company.com",
		"not a url": "",
	}
	for in, want := range cases {
		if got := baseSite(in); got != want {
			t.Errorf("baseSite(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAbsoluteURL(t *testing.T) {
	base, _ := mustParse("https://ir.example.com/investors/index.html")
	cases := map[string]string{
		"/files/ar.pdf":                 "https://ir.example.com/files/ar.pdf",
		"sub/page.html":                 "https://ir.example.com/investors/sub/page.html",
		"https://other.com/x.pdf#page1": "https://other.com/x.pdf",
		"mailto:a@b.com":                "",
		"javascript:void(0)":            "",
		"":                              "",
	}
	for in, want := range cases {
		if got := absoluteURL(base, in); got != want {
			t.Errorf("absoluteURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSameSite(t *testing.T) {
	if !sameSite("ir.ril.com", "www.ril.com") {
		t.Fatal("ir.ril.com and www.ril.com should match")
	}
	if sameSite("ril.com", "example.com") {
		t.Fatal("ril.com and example.com should not match")
	}
}

func TestNormalizeURL(t *testing.T) {
	cases := map[string]string{
		"  https://Example.com/a#frag ": "https://Example.com/a",
		"//cdn.example.com/x.pdf":       "https://cdn.example.com/x.pdf",
		"example.com/x.pdf":             "",
		"":                              "",
	}
	for in, want := range cases {
		if got := normalizeURL(in); got != want {
			t.Errorf("normalizeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPickIndex(t *testing.T) {
	if got := pickIndex(float64(7)); got != 7 {
		t.Fatalf("pickIndex(7.0) = %d", got)
	}
	if got := pickIndex("12"); got != 12 {
		t.Fatalf("pickIndex(\"12\") = %d", got)
	}
	if got := pickIndex(nil); got != -1 {
		t.Fatalf("pickIndex(nil) = %d", got)
	}
}

func TestSelectOptions(t *testing.T) {
	body := []byte(`<html><body>
	<form><select name="field_report_category_target_id">
	  <option value="All" selected>- Any -</option>
	  <option value="16">Annual Report</option>
	  <option value="19">Investor Presentation</option>
	</select></form>
	<select name="sortbyselector"><option value="Latest">Latest</option></select>
	</body></html>`)
	opts := selectOptions(body)
	if len(opts) != 4 {
		t.Fatalf("selectOptions returned %d options, want 4: %+v", len(opts), opts)
	}
	if opts[1].Name != "field_report_category_target_id" || opts[1].Value != "16" || opts[1].Text != "Annual Report" {
		t.Fatalf("second option = %+v", opts[1])
	}
}

func TestFilterCandidates(t *testing.T) {
	body := []byte(`<select name="field_report_category_target_id">
	  <option value="16">Annual Report</option>
	  <option value="19">Investor Presentation</option>
	  <option value="18">Reports</option>
	  <option value="13">Shareholding Pattern</option>
	</select>`)
	base, _ := mustParse("https://www.mahindra.com/investor-relations/reports")

	cands := filterCandidates(base, body, base.String())
	if len(cands) != 2 {
		t.Fatalf("filterCandidates = %d, want 2 (annual + presentation): %+v", len(cands), cands)
	}
	got := map[string]string{}
	for _, c := range cands {
		got[c.Text] = c.URL
	}
	if u := got["Annual Report"]; !strings.Contains(u, "field_report_category_target_id=16") {
		t.Fatalf("annual candidate URL = %q", u)
	}
	if u := got["Investor Presentation"]; !strings.Contains(u, "field_report_category_target_id=19") {
		t.Fatalf("presentation candidate URL = %q", u)
	}

	// Non-index pages (shared filter, no filings listed) yield nothing.
	if n := len(filterCandidates(base, body, "https://www.mahindra.com/investor-relations/sustainability")); n != 0 {
		t.Fatalf("sustainability filterCandidates = %d, want 0", n)
	}
}

func TestIsDownloadLink(t *testing.T) {
	if !isDownloadLink(candidate{Text: "Download PDF", URL: "https://www.mahindra.com/print/pdf/node/7857"}) {
		t.Fatal("print/pdf download link not detected")
	}
	if isDownloadLink(candidate{Text: "Reports", URL: "https://www.mahindra.com/investor-relations/reports"}) {
		t.Fatal("plain page link misdetected as download")
	}
	if !isDocLink(SegQuarterly, candidate{Text: "Download PDF", URL: "https://www.mahindra.com/print/pdf/node/7857"}) {
		t.Fatal("download endpoint should be a doc link for a classified page")
	}
	if isDocLink(SegAnnualReport, candidate{Text: "Download", URL: "https://x.com/press-release.pdf"}) {
		t.Fatal("irrelevant PDF should not be a doc link")
	}
}

func TestJSONCandidates(t *testing.T) {
	// Next.js flight-data style payload: record JSON escaped inside
	// self.__next_f.push([1,"..."]) script strings, like waaree.com ships
	// (each quote carries one backslash in the raw HTML).
	body := []byte(`<script>self.__next_f.push([1,"...{\"id\":2902,\"template_name\":\"Investors\",\"name\":\"Q1 Investor Presentation 2026-27\",\"slug\":\"/q1-investor-presentation-2026-27\",\"meta\":{\"title\":null},\"template_fields\":{\"document\":{\"src\":\"/upload/media/investorpresentation_signed_1_1785396912.pdf\"}}},{\"id\":2961,\"name\":\"Q1 Transcript of Call 2026-27\",\"template_fields\":{\"document\":{\"src\":\"/upload/media/waaree_energies_ltd_q1_fy27_earnings_call_transcript_1786015347.pdf\"}}},{\"link\":{\"title\":\"Annual Report\",\"url_type\":\"internal\",\"url\":\"/investor/annual-report\"}}..."])</script>`)
	base, _ := mustParse("https://www.waaree.com/investor/")

	cands := jsonCandidates(base, body, "https://www.waaree.com/investor/")
	if len(cands) != 3 {
		t.Fatalf("jsonCandidates = %d items, want 3: %+v", len(cands), cands)
	}
	byURL := map[string]candidate{}
	for _, c := range cands {
		byURL[c.URL] = c
	}
	pres := byURL["https://www.waaree.com/upload/media/investorpresentation_signed_1_1785396912.pdf"]
	if pres.Text != "Q1 Investor Presentation 2026-27" {
		t.Fatalf("presentation text = %q, want its record name", pres.Text)
	}
	if got := classify(pres); got != SegPresentation {
		t.Fatalf("classify(presentation record) = %q, want %q", got, SegPresentation)
	}
	trans := byURL["https://www.waaree.com/upload/media/waaree_energies_ltd_q1_fy27_earnings_call_transcript_1786015347.pdf"]
	if got := classify(trans); got != SegTranscript {
		t.Fatalf("classify(transcript record) = %q, want %q", got, SegTranscript)
	}
	if y, q := recency(trans.URL + " " + trans.Text); y != 2027 || q != 1 {
		t.Fatalf("recency(transcript) = (%d, %d), want (2027, 1)", y, q)
	}
	if _, ok := byURL["https://www.waaree.com/investor/annual-report"]; !ok {
		t.Fatal("internal page reference not harvested")
	}
}

func TestJSONCandidatesIgnoresNoise(t *testing.T) {
	body := []byte(`{"a":"x.pdf not quoted","b":"https://facebook.com/share.pdf","c":"","url":"/policies","items":[{"name":"Shareholding Pattern","src":"/upload/media/shareholding.pdf"},{"name":"Code of Conduct Policy","src":"/upload/media/policy.pdf"}]}`)
	base, _ := mustParse("https://ir.example.com/")
	cands := jsonCandidates(base, body, "https://ir.example.com/")
	for _, c := range cands {
		if strings.Contains(c.URL, "facebook.com") {
			t.Fatalf("social URL leaked: %s", c.URL)
		}
		if classify(c) != "" {
			t.Fatalf("denylisted candidate leaked: %+v", c)
		}
	}
}

func TestPageCandidatesMergesAnchorsAndJSON(t *testing.T) {
	body := []byte(`<a href="/reports/ar.pdf">Annual Report</a>
		<script>self.__next_f.push([1,"{\"name\":\"Investor Presentation Q2\",\"template_fields\":{\"document\":{\"src\":\"/upload/media/ip.pdf\"}}}"])</script>`)
	base, _ := mustParse("https://ir.example.com/investors/")
	cands := pageCandidates(base, body, base.String())
	if len(cands) != 2 {
		t.Fatalf("pageCandidates = %d, want 2: %+v", len(cands), cands)
	}
	if cands[0].Text != "Annual Report" || cands[1].Text != "Investor Presentation Q2" {
		t.Fatalf("candidate texts = %q, %q", cands[0].Text, cands[1].Text)
	}
}

func mustParse(u string) (*url.URL, error) {
	return url.Parse(u)
}

// fakePDF returns byte counts that pass download's PDF sniffing.
func fakePDF() []byte {
	b := make([]byte, 2048)
	copy(b, "%PDF-1.4\n")
	return b
}

func TestFindListingPagination(t *testing.T) {
	// Flight-data style payload (quotes escaped) as waaree.com ships it.
	flat := jsonFlatten.Replace(`self.__next_f.push([1,"...{\"pages\":{\"filters\":{\"url\":\"/filter-data\",\"template_id\":\"20\",\"limit\":8},\"current_page\":1,\"total_pages\":4,\"limit\":8,\"total_items\":32},\"filter_list\":[{\"id\":32,\"name\":\"Year\",\"tags\":[]}],\"conditions\":[],\"tag_conditions\":[{\"id\":\"30\",\"value\":\"478\"}],\"sort\":{\"sort_by\":\"published_date\",\"sort_type\":\"desc\"}}..."])`)
	spec := findListingPagination(flat)
	if spec == nil {
		t.Fatal("findListingPagination = nil, want a spec")
	}
	if spec.Path != "/filter-data" || spec.TemplateID != "20" || spec.Limit != 8 || spec.TotalPages != 4 {
		t.Fatalf("spec = %+v", spec)
	}
	if spec.TagConditions != `[{"id":"30","value":"478"}]` {
		t.Fatalf("tag_conditions = %q", spec.TagConditions)
	}
	if spec.Conditions != `[]` || spec.Sort != `{"sort_by":"published_date","sort_type":"desc"}` {
		t.Fatalf("conditions/sort = %q / %q", spec.Conditions, spec.Sort)
	}

	// Single-page lists and non-API "pages" keys are ignored.
	if findListingPagination(`{"pages":{"filters":{"url":"/x","template_id":"1","limit":8},"total_pages":1,"total_items":8}}`) != nil {
		t.Fatal("single-page list should not yield a spec")
	}
	if findListingPagination(`{"pages":{"title":"Other"}}`) != nil {
		t.Fatal("unrelated pages object should not yield a spec")
	}
}

func TestListingRequestBodyAndEndpoints(t *testing.T) {
	spec := &listingPagination{
		Path: "/filter-data", TemplateID: "20", Limit: 8, TotalPages: 4,
		Conditions: `[]`, TagConditions: `[{"id":"30","value":"478"}]`,
		Sort: `{"sort_by":"published_date","sort_type":"desc"}`,
	}
	got := listingRequestBody(spec, 2, 100, false)
	want := `{"template_id":"20","page":2,"limit":100,"tags":{},"conditions":[],"tag_conditions":[{"id":"30","value":"478"}],"sort":{"sort_by":"published_date","sort_type":"desc"}}`
	if got != want {
		t.Fatalf("listingRequestBody = %s, want %s", got, want)
	}
	if withCtx := listingRequestBody(spec, 1, 8, true); !strings.Contains(withCtx, `"store":"global","locale":"en"`) {
		t.Fatalf("context request = %s", withCtx)
	}

	eps := listingEndpoints("https://www.waaree.com/investor/annual-report", "/filter-data")
	wantEps := []string{
		"https://www.waaree.com/filter-data",
		"https://www.waaree.com/api/filter-data",
		"https://api.waaree.com/api/filter-data",
		"https://api.waaree.com/filter-data",
	}
	if !slices.Equal(eps, wantEps) {
		t.Fatalf("listingEndpoints = %v, want %v", eps, wantEps)
	}
}

func TestPaginatedListingDocs(t *testing.T) {
	listingPage := `<html><body><a href="/upload/media/draft_mgt_7.pdf">Draft Annual Return FY 2025 - 26</a>
		<script>self.__next_f.push([1,"{\"pages\":{\"filters\":{\"url\":\"/filter-data\",\"template_id\":\"20\",\"limit\":8},\"current_page\":1,\"total_pages\":4,\"limit\":8,\"total_items\":32},\"conditions\":[],\"tag_conditions\":[{\"id\":\"30\",\"value\":\"478\"}],\"sort\":{\"sort_by\":\"published_date\",\"sort_type\":\"desc\"}}"])</script>
	</body></html>`
	var seenBodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/investor/annual-report" {
			fmt.Fprint(w, listingPage)
			return
		}
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		seenBodies = append(seenBodies, string(body))
		if !strings.Contains(string(body), `"store"`) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"errors":{"error":"Invalid or missing store\/locale context"},"status":false}`)
			return
		}
		fmt.Fprint(w, `{"list":[`+
			`{"name":"Annual Report FY 2025 - 26","template_fields":{"document":{"src":"\/upload\/media\/annualreport_25_26.pdf"}},"tag_data":{"type":[{"name":"Annual Report"}]}},`+
			`{"name":"Notice of AGM FY 2025 - 26","template_fields":{"document":{"src":"\/upload\/media\/notice_of_agm.pdf"}}},`+
			`{"name":"Annual Return (MGT-7) FY 2024-25","template_fields":{"document":{"src":"\/upload\/media\/mgt_7.pdf"}}}`+
			`],"pages":{"filters":{"url":"/filter-data","template_id":"20","limit":100},"current_page":1,"total_pages":1,"total_items":3}}`)
	}))
	defer srv.Close()

	c := &Client{hc: srv.Client()}
	co := models.Company{Symbol: "TEST"}
	listing := filing{Segment: SegAnnualReport, URL: srv.URL + "/investor/annual-report", PageURL: srv.URL}

	got := c.pdfsOnPage(context.Background(), co, SegAnnualReport, listing)
	if len(got) != 1 {
		t.Fatalf("pdfsOnPage = %+v, want the single annual report", got)
	}
	if want := srv.URL + "/upload/media/annualreport_25_26.pdf"; got[0].URL != want {
		t.Fatalf("pick = %s, want %s", got[0].URL, want)
	}
	if got[0].PageURL != listing.URL {
		t.Fatalf("pick page = %s, want %s", got[0].PageURL, listing.URL)
	}
	if len(seenBodies) != 2 {
		t.Fatalf("api calls = %d (%v), want 2 (bare request + context retry)", len(seenBodies), seenBodies)
	}
}

func TestFetchAndParseListingFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ar":
			fmt.Fprint(w, `<html><body><h1>Annual Reports</h1><a href="/real.pdf">Annual Report FY 2025 - 26</a></body></html>`)
		case "/real.pdf":
			w.Write(fakePDF())
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &Client{hc: srv.Client()}
	co := models.Company{Symbol: "TEST"}
	picks := map[string][]filing{
		SegAnnualReport: {{Segment: SegAnnualReport, URL: srv.URL + "/ar", PageURL: srv.URL}},
	}
	results := c.fetchAndParse(context.Background(), co, picks, map[string][]filing{}, &sync.Mutex{})
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if want := srv.URL + "/real.pdf"; results[0].SourceURL != want {
		t.Fatalf("source = %s, want %s (the pdf linked on the listing page)", results[0].SourceURL, want)
	}
	if len(results[0].PDF) != len(fakePDF()) {
		t.Fatalf("pdf bytes = %d, want %d", len(results[0].PDF), len(fakePDF()))
	}

	// A listing page with no documents at all yields no result.
	picks = map[string][]filing{
		SegAnnualReport: {{Segment: SegAnnualReport, URL: srv.URL + "/missing", PageURL: srv.URL}},
	}
	if results := c.fetchAndParse(context.Background(), co, picks, map[string][]filing{}, &sync.Mutex{}); len(results) != 0 {
		t.Fatalf("results = %d, want 0", len(results))
	}
}
