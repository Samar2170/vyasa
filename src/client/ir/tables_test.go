package ir

import (
	"strings"
	"testing"

	"github.com/ledongthuc/pdf"
)

// chars renders s as per-character pdf.Text items at (x, y), the shape
// Page.Content() emits. Each glyph advances by half its font size.
func chars(s string, x, y, size float64) []pdf.Text {
	var out []pdf.Text
	for _, r := range s {
		w := 0.5 * size
		out = append(out, pdf.Text{S: string(r), X: x, Y: y, W: w, FontSize: size})
		x += w
	}
	return out
}

// spread lays out one visual line: label at x=50, numbers at 250 and 350 —
// column gaps wide enough to split, word gaps narrow enough to hold.
func spread(s string, cols []string, y, size float64) []pdf.Text {
	xs := []float64{50, 250, 350, 450}
	var out []pdf.Text
	parts := append([]string{s}, cols...)
	for i, p := range parts {
		out = append(out, chars(p, xs[i], y, size)...)
	}
	return out
}

func TestSplitCells(t *testing.T) {
	l := itemLine{
		y:    100,
		size: 8,
		items: append(
			chars("Revenue from operations", 50, 100, 8),
			chars("1,234.56", 250, 100, 8)...,
		),
	}
	lc := splitCells(l)
	if len(lc.cells) != 2 {
		t.Fatalf("cells = %v, want 2", lc.cells)
	}
	if lc.cells[0] != "Revenue from operations" {
		t.Errorf("cell 0 = %q", lc.cells[0])
	}
	if lc.cells[1] != "1,234.56" {
		t.Errorf("cell 1 = %q", lc.cells[1])
	}
	if lc.xs[0] != 50 || lc.xs[1] != 250 {
		t.Errorf("xs = %v, want [50 250]", lc.xs)
	}
}

func TestSplitCellsWordGapsHold(t *testing.T) {
	// "FY 2026  Revenue" — the space inside FY 2026 must not split, the
	// wide gap must.
	l := itemLine{
		y:    100,
		size: 8,
		items: append(
			chars("FY 2026", 50, 100, 8),
			chars("Revenue", 300, 100, 8)...,
		),
	}
	lc := splitCells(l)
	if len(lc.cells) != 2 || lc.cells[0] != "FY 2026" || lc.cells[1] != "Revenue" {
		t.Fatalf("cells = %v", lc.cells)
	}
}

func TestIsNumericCell(t *testing.T) {
	cases := []struct {
		cell string
		want bool
	}{
		{"1,234.56", true},
		{"(1,234)", true},
		{"12%", true},
		{"-500", true},
		{"₹1,000", true},
		{"-", true},
		{"NA", true},
		{"nil", true},
		{"Revenue", false},
		{"FY2025-26", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isNumericCell(c.cell); got != c.want {
			t.Errorf("isNumericCell(%q) = %v, want %v", c.cell, got, c.want)
		}
	}
}

func TestCaptionScore(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"Statement of Profit and Loss for the quarter", 5}, // strong(2) + weak(3): profit and loss, quarter... plus standalone? no
		{"Balance Sheet as at 31 March", 2},
		{"Cash Flow Statement (Ind AS)", 2},
		{"Notes to the financial statements", 0},
		{"Figures in Rs crore", 2}, // figures in + rs in
	}
	for _, c := range cases {
		if got := captionScore(c.text); got != c.want {
			t.Errorf("captionScore(%q) = %d, want %d", c.text, got, c.want)
		}
	}
}

// pageFixture builds a statement page: caption, header, three aligned
// numeric rows with a wrapped label inside, then a prose paragraph.
func pageFixture() []pdf.Text {
	var items []pdf.Text
	items = append(items, chars("Statement of Profit and Loss", 50, 120, 10)...)
	items = append(items, spread("Particulars", []string{"Q2", "Q1"}, 114, 8)...)
	items = append(items, spread("Revenue from operations", []string{"1,000", "900"}, 108, 8)...)
	items = append(items, chars("Depreciation and", 50, 102, 8)...) // wrapped label, single cell
	items = append(items, spread("amortization", []string{"500", "450"}, 96, 8)...)
	items = append(items, spread("Finance costs", []string{"120", "130"}, 90, 8)...)
	// prose paragraph: single-cell lines, too far apart to align
	items = append(items, chars("The company continues to focus on its core business during the year.", 50, 60, 8)...)
	return items
}

func TestTablesOnPage(t *testing.T) {
	tables := tablesOnPage(pageFixture(), 7)
	if len(tables) != 1 {
		t.Fatalf("got %d tables, want 1", len(tables))
	}
	tb := tables[0]

	if tb.Page != 7 {
		t.Errorf("page = %d, want 7", tb.Page)
	}
	if !strings.Contains(tb.Caption, "Profit and Loss") {
		t.Errorf("caption = %q", tb.Caption)
	}
	if len(tb.Header) != 3 || tb.Header[0] != "Particulars" || tb.Header[1] != "Q2" || tb.Header[2] != "Q1" {
		t.Errorf("header = %v", tb.Header)
	}
	if len(tb.Rows) != 3 {
		t.Fatalf("rows = %d, want 3: %v", len(tb.Rows), tb.Rows)
	}
	if tb.Rows[0][0] != "Revenue from operations" {
		t.Errorf("row 0 label = %q", tb.Rows[0][0])
	}
	if tb.Rows[1][0] != "Depreciation and amortization" {
		t.Errorf("wrapped label not merged: %q", tb.Rows[1][0])
	}
	if tb.Rows[1][1] != "500" || tb.Rows[1][2] != "450" {
		t.Errorf("row 1 numbers = %v", tb.Rows[1])
	}
	if tb.Rows[2][0] != "Finance costs" {
		t.Errorf("row 2 label = %q", tb.Rows[2][0])
	}
}

func TestBlocksOnPageTwoTables(t *testing.T) {
	// two aligned tables separated by a prose paragraph and different
	// column anchors stay separate blocks
	lc := []lineCells{
		{xs: []float64{50, 250, 350}, cells: []string{"Revenue", "1,000", "900"}},
		{xs: []float64{50, 250, 350}, cells: []string{"Costs", "500", "450"}},
		{xs: []float64{50, 250, 350}, cells: []string{"Profit", "500", "450"}},
		{xs: []float64{50}, cells: []string{"Segment information follows."}},
		{xs: []float64{50, 200, 300}, cells: []string{"Digital", "800", "700"}},
		{xs: []float64{50, 200, 300}, cells: []string{"Oil", "600", "650"}},
		{xs: []float64{50, 200, 300}, cells: []string{"Retail", "400", "380"}},
	}
	blocks := blocksOnPage(lc)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2", len(blocks))
	}
	if len(blocks[0].cells) != 3 || len(blocks[1].cells) != 3 {
		t.Errorf("block sizes = %d, %d, want 3, 3", len(blocks[0].cells), len(blocks[1].cells))
	}
}

func TestBlocksOnPageBlankLinesAreSafe(t *testing.T) {
	// whitespace-only lines (zero cells) occur inside and around tables;
	// they must neither panic nor break the block
	lc := []lineCells{
		{},
		{xs: []float64{50, 250, 350}, cells: []string{"Revenue", "1,000", "900"}},
		{},
		{xs: []float64{50}, cells: []string{"note"}},
		{},
		{xs: []float64{50, 250, 350}, cells: []string{"Costs", "500", "450"}},
		{xs: []float64{50, 250, 350}, cells: []string{"Profit", "500", "450"}},
		{},
	}
	blocks := blocksOnPage(lc)
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(blocks))
	}
	if len(blocks[0].cells) != 3 {
		t.Fatalf("rows = %d, want 3", len(blocks[0].cells))
	}
	// the stashed label must still fold into the next row after blanks
	if blocks[0].cells[1].cells[0] != "note Costs" {
		t.Errorf("label after blank line not merged: %q", blocks[0].cells[1].cells[0])
	}
}

func TestBlocksOnPageRejectsProse(t *testing.T) {
	// paragraphs: single-cell lines and two-cell lines never form blocks
	lc := []lineCells{
		{xs: []float64{50}, cells: []string{"The quick brown fox jumps over the lazy dog and keeps running."}},
		{xs: []float64{50}, cells: []string{"Another sentence follows with more words to fill the line width."}},
		{xs: []float64{50, 60}, cells: []string{"A short", "line"}},
	}
	if blocks := blocksOnPage(lc); len(blocks) != 0 {
		t.Fatalf("blocks = %d, want 0", len(blocks))
	}
}

func TestAlignsToleratesOneMiss(t *testing.T) {
	anchors := []float64{50, 250, 350}
	lc := lineCells{xs: []float64{50, 250, 355}, cells: []string{"a", "b", "c"}}
	if !lc.aligns(anchors) {
		t.Error("one 5pt miss should be tolerated")
	}
	lc = lineCells{xs: []float64{60, 260, 350}, cells: []string{"a", "b", "c"}}
	if lc.aligns(anchors) {
		t.Error("two misses should fail alignment")
	}
	lc = lineCells{xs: []float64{50, 250, 500}, cells: []string{"a", "b", "c"}}
	if lc.aligns(anchors) {
		t.Error("a far-off cell should fail alignment even alone")
	}
}

func TestCleanCellsDropsDotLeaders(t *testing.T) {
	got := cleanCells([]string{"Revenue ..........", "1,234", "  "})
	if got[0] != "Revenue" {
		t.Errorf("dot leader not stripped: %q", got[0])
	}
	if got[1] != "1,234" {
		t.Errorf("number mangled: %q", got[1])
	}
}
