package rag

import (
	"strings"
	"testing"

	"vyasa/src/models"
)

func TestExtractedTableMarkdown(t *testing.T) {
	tt := ExtractedTable{
		Caption: "Balance Sheet",
		Header:  []string{"Particulars", "Q2", "Q1"},
		Rows: [][]string{
			{"Revenue", "1,000", "900"},
			{"Other  income ", "10"}, // ragged + doubled spaces
			{"Profit", "500", "450"},
		},
	}
	md := tt.Markdown()
	lines := strings.Split(md, "\n")
	if len(lines) != 5 {
		t.Fatalf("markdown has %d lines, want 5:\n%s", len(lines), md)
	}
	if lines[0] != "| Particulars | Q2 | Q1 |" {
		t.Errorf("header line = %q", lines[0])
	}
	if lines[1] != "| --- | --- | --- |" {
		t.Errorf("separator = %q", lines[1])
	}
	if lines[2] != "| Revenue | 1,000 | 900 |" {
		t.Errorf("row = %q", lines[2])
	}
	if lines[3] != "| Other income | 10 |  |" {
		t.Errorf("ragged row not padded: %q", lines[3])
	}
}

func TestExtractedTableMarkdownEmpty(t *testing.T) {
	if got := (ExtractedTable{}).Markdown(); got != "" {
		t.Errorf("empty table markdown = %q", got)
	}
}

func TestSplitTableRows(t *testing.T) {
	tt := ExtractedTable{Page: 3, Caption: "P&L", Header: []string{"a", "b"}}
	for i := 0; i < 50; i++ {
		tt.Rows = append(tt.Rows, []string{"r", "1"})
	}
	parts := splitTableRows(tt)
	if len(parts) != 3 {
		t.Fatalf("parts = %d, want 3", len(parts))
	}
	for i, p := range parts {
		if len(p.Header) != 2 || p.Header[0] != "a" {
			t.Errorf("part %d header = %v, want repeated", i, p.Header)
		}
		if p.Page != 3 || p.Caption != "P&L" {
			t.Errorf("part %d lost page/caption: %d %q", i, p.Page, p.Caption)
		}
	}
	if len(parts[0].Rows) != tableSplitRows || len(parts[1].Rows) != tableSplitRows || len(parts[2].Rows) != 2 {
		t.Errorf("part row counts = %d, %d, %d", len(parts[0].Rows), len(parts[1].Rows), len(parts[2].Rows))
	}
}

func TestSplitTableRowsShort(t *testing.T) {
	tt := ExtractedTable{Rows: [][]string{{"a", "1"}}}
	parts := splitTableRows(tt)
	if len(parts) != 1 || len(parts[0].Rows) != 1 {
		t.Errorf("short table should pass through: %+v", parts)
	}
}

func TestTableChunksCarryKind(t *testing.T) {
	tables := []models.RAGTable{{
		BaseModel:   models.BaseModel{ID: "t1"},
		Symbol:      "RELIANCE",
		Segment:     "annual_report",
		DocumentURL: "https://example.com/ar.pdf",
		Ordinal:     2,
		Page:        41,
		Caption:     "Balance Sheet",
		Text:        "| a | b |",
		Period:       "FY2025-26",
		Source:       "investor_relations",
	}}
	chunks := tableChunks(tables)
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(chunks))
	}
	c := chunks[0]
	if c.Kind != models.HitKindTable {
		t.Errorf("kind = %q, want table", c.Kind)
	}
	if c.Section != "Balance Sheet" || c.Offset != 41 || c.Ordinal != 2 {
		t.Errorf("mapping lost table identity: %+v", c)
	}
	if c.Text != "| a | b |" || c.Period != "FY2025-26" {
		t.Errorf("mapping lost text/period: %+v", c)
	}
}
