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

func TestAnnualReportChunkerSections(t *testing.T) {
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

	var directors, mda bool
	for _, c := range got {
		if strings.Contains(c.Context, "Directors' Report") && strings.Contains(c.Context, "page 1") {
			directors = true
		}
		if strings.Contains(c.Context, "MANAGEMENT DISCUSSION") && strings.Contains(c.Context, "page 2") {
			mda = true
		}
		if !strings.Contains(markedDoc[:len(markedDoc)], "") && c.Offset < 0 {
			t.Errorf("bad offset %d", c.Offset)
		}
	}
	if !directors || !mda {
		t.Errorf("missing section contexts: %+v", got)
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
