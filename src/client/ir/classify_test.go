package ir

import (
	"net/url"
	"strings"
	"testing"
	"time"
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

func mustParse(u string) (*url.URL, error) {
	return url.Parse(u)
}
