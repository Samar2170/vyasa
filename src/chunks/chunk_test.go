package chunks

import (
	"strings"
	"testing"
)

const markedDoc = `The board submits its report for FY2024.

[[page 1]]

DIRECTORS' REPORT
The board is pleased to present the annual report.
Performance was strong across all segments.

[[page 2]]

MANAGEMENT DISCUSSION AND ANALYSIS
Demand remained healthy through the year.
`

// TestAnnualReportChunkerPages pins the context contract: chunks carry only
// their page number, never a parsed section title.
func TestAnnualReportChunkerPages(t *testing.T) {
	got := AnnualReportChunker{}.Chunk(markedDoc)
	if len(got) < 3 {
		t.Fatalf("got %d chunks, want >=3: %+v", len(got), got)
	}

	if got[0].Text != "The board submits its report for FY2024." || got[0].Offset != 0 {
		t.Errorf("first chunk = %+v", got[0])
	}
	if got[0].Context != "" {
		t.Errorf("intro chunk context = %q, want empty", got[0].Context)
	}

	var page1, page2 bool
	for _, c := range got {
		switch c.Context {
		case "page 1":
			page1 = true
		case "page 2":
			page2 = true
		}
		if c.Offset < 0 {
			t.Errorf("bad offset %d", c.Offset)
		}
	}
	if !page1 || !page2 {
		t.Errorf("missing page contexts: %+v", got)
	}
}

func TestAnnualReportChunkerHeadingStartsNewChunk(t *testing.T) {
	got := AnnualReportChunker{}.Chunk(markedDoc)
	for _, c := range got {
		if strings.Contains(c.Text, "DIRECTORS' REPORT") &&
			strings.Contains(c.Text, "The board submits") {
			t.Errorf("heading did not start a new chunk: %q", c.Text)
		}
	}
}

func TestPresentationChunkerMergesStubSlides(t *testing.T) {
	doc := `[[page 1]]

Revenue at a glance

[[page 2]]

EBITDA margin up 120 bps

[[page 3]]

` + strings.Repeat("Growth outlook detail line. ", 200) + `
`
	got := PresentationChunker{}.Chunk(doc)
	if len(got) < 2 {
		t.Fatalf("got %d chunks, want >=2", len(got))
	}
	if got[0].Context != "slides 1-2" {
		t.Errorf("first context = %q, want slides 1-2", got[0].Context)
	}
	if !strings.Contains(got[0].Text, "Revenue at a glance") ||
		!strings.Contains(got[0].Text, "EBITDA margin") {
		t.Errorf("stub slides not merged: %q", got[0].Text)
	}
	if got[len(got)-1].Context != "slide 3" {
		t.Errorf("last context = %q, want slide 3", got[len(got)-1].Context)
	}
}

// Junk lines observed in production rag_chunks data must never be picked
// up as section headings — a wrong heading sticks to hundreds of chunks.
func TestAnnualHeadingJunkRejected(t *testing.T) {
	junk := []string{
		"SGS",
		"BRSR",
		"NOTES:1.",
		"G13.Risk ManagementCritical (Tier I)-",
		"INTEGRATED ANNUALREPORT 2025-26MAHINDRA & MAHINDRA LIMITED",
		"Risk ManagementCritical (Tier I)-",
	}
	for _, s := range junk {
		if isAnnualHeading(s) {
			t.Errorf("junk accepted as heading: %q", s)
		}
	}
}

func TestAnnualHeadingRealSectionsAccepted(t *testing.T) {
	headings := []string{
		"DIRECTORS' REPORT",
		"MANAGEMENT DISCUSSION AND ANALYSIS",
		"Notes to the Financial Statements",
		"INDEPENDENT AUDITOR'S REPORT",
		"REPORT OF THE AUDITOR",
		"III. CORPORATE GOVERNANCE REPORT",
		"Financial Highlights",
	}
	for _, s := range headings {
		if !isAnnualHeading(s) {
			t.Errorf("heading rejected: %q", s)
		}
	}
}

func TestForSegment(t *testing.T) {
	if ForSegment(SegAnnualReport).Name() != "annual_report" {
		t.Error("annual_report should map to AnnualReportChunker")
	}
	if ForSegment(SegPresentation).Name() != "investor_presentation" {
		t.Error("investor_presentation should map to PresentationChunker")
	}
	if ForSegment("earnings_call_transcript").Name() != "fixed" {
		t.Error("unknown segment should fall back to fixed")
	}
}
