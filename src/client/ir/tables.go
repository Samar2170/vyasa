package ir

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ledongthuc/pdf"

	"vyasa/src/rag"
)

// Table extraction from filing PDFs. The pdf library exposes every drawn
// character with its position and advance width (Page.Content emits one
// Text per glyph), which is enough to rebuild the layout borderless PDFs
// rely on:
//
//  1. group characters into lines by baseline (Y tolerance),
//  2. split each line into cells at large horizontal gaps,
//  3. merge consecutive lines whose cell starts align into a block,
//  4. hoist an aligned header line above the block and search for a caption
//     line above that.
//
// Detection is tuned for financial statements (Particulars + numeric
// columns) and prefers precision over recall: a wrong table is worse than
// no table, since missed tables still surface through the prose chunks.

// ExtractedTables returns every table mined from the raw PDF bytes, in
// page order. It is the exported form for the rag-index backfill.
func ExtractedTables(data []byte) []rag.ExtractedTable {
	return extractPDFTables(data)
}

const (
	lineYTolerance  = 2.5 // same line when baselines are this close (pt)
	alignTolerance  = 6.0 // same column when cell starts are this close (pt)
	minTableLines   = 3   // data rows a block needs to count as a table
	maxLabelGap     = 2   // wrapped label lines tolerated inside a block
	headerHoistMax  = 2   // aligned header lines picked up above a block
	captionScanMax  = 4   // lines scanned above a table for its caption
	captionMinScore = 2   // keyword score a caption line needs
	maxTablesPage   = 8   // tables accepted per page
	maxTablesDoc    = 60  // tables accepted per document
	maxTableCols    = 24  // wider than any real statement: layout noise
	minCellGap      = 5.0 // never split cells at smaller gaps (pt)
	cellGapFactor   = 1.2 // cell gap threshold = factor x font size
)

var (
	numericCellRe = regexp.MustCompile(`^[-(]?[₹$]?\s*\d[\d,.]*\s*[%)]?$`)
	nilCellRe     = regexp.MustCompile(`(?i)^(?:[-–—.]+|n/?a\.?|nil)$`)
	dotRunRe      = regexp.MustCompile(`\s*\.{2,}\s*`)
)

// isNumericCell reports whether a cell holds a number, a percentage, a
// bracketed negative, or a nil placeholder (dash, NA) — the shapes financial
// statement bodies are made of.
func isNumericCell(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	return numericCellRe.MatchString(s) || nilCellRe.MatchString(s)
}

// caption keywords: statement titles are strong evidence, figure units and
// generic words are weak. A caption needs a minimum combined score.
var captionStrong = []string{
	"balance sheet", "profit & loss", "profit and loss", "profit or loss",
	"statement of profit", "cash flow", "cash-flow", "income statement",
	"segment results", "key ratios", "standalone financial",
	"consolidated financial",
}
var captionWeak = []string{
	"particulars", "revenue", "quarter", "year ended", "figures in",
	"rs in", "crore", "lakh", "million", "unaudited", "audited",
	"standalone", "consolidated", "summary", "results", "ratios",
}

// captionScore rates a candidate caption line.
func captionScore(s string) int {
	s = strings.ToLower(s)
	score := 0
	for _, k := range captionStrong {
		if strings.Contains(s, k) {
			score += 2
		}
	}
	for _, k := range captionWeak {
		if strings.Contains(s, k) {
			score++
		}
	}
	return score
}

// TablesJSON renders extracted tables as the archived JSON payload stored
// beside the other filing files.
func TablesJSON(segment, sourceURL string, tables []rag.ExtractedTable) ([]byte, error) {
	return json.Marshal(tablesPayload{
		Segment:     segment,
		SourceURL:   sourceURL,
		GeneratedAt: time.Now(),
		Tables:      tables,
	})
}

// extractPDFTables pulls table candidates out of the raw PDF bytes.
func extractPDFTables(data []byte) []rag.ExtractedTable {
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil
	}
	var out []rag.ExtractedTable
	for i := 1; i <= r.NumPage() && len(out) < maxTablesDoc; i++ {
		items := safePageContent(r.Page(i))
		if len(items) == 0 {
			continue
		}
		for _, t := range tablesOnPage(items, i) {
			out = append(out, t)
			if len(out) >= maxTablesDoc {
				break
			}
		}
	}
	return out
}

// safePageContent guards against panics inside the pdf parser on malformed
// pages.
func safePageContent(p pdf.Page) (items []pdf.Text) {
	defer func() {
		if recover() != nil {
			items = nil
		}
	}()
	return p.Content().Text
}

// itemLine is one visual line: the characters sharing a baseline.
type itemLine struct {
	y     float64
	size  float64
	items []pdf.Text
}

// lineCells is a line split into cells: each cell keeps the x where it
// starts (its column anchor) and its text.
type lineCells struct {
	xs    []float64
	cells []string
}

// pageLines groups the page's characters into lines, top to bottom. Space
// characters are kept: they are the separators between words inside a cell
// (some PDFs draw no space glyphs, in which case words jam — the same
// trade-off GetPlainText makes).
func pageLines(items []pdf.Text) []itemLine {
	clean := items[:0]
	for _, it := range items {
		if it.S != "" {
			clean = append(clean, it)
		}
	}
	items = clean
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Y != items[j].Y {
			return items[i].Y > items[j].Y // top first
		}
		return items[i].X < items[j].X
	})

	var out []itemLine
	for _, it := range items {
		if n := len(out); n > 0 && it.Y >= out[n-1].y-lineYTolerance {
			out[n-1].items = append(out[n-1].items, it)
			continue
		}
		out = append(out, itemLine{y: it.Y, size: it.FontSize, items: []pdf.Text{it}})
	}
	for i := range out {
		l := &out[i]
		sort.SliceStable(l.items, func(a, b int) bool { return l.items[a].X < l.items[b].X })
		for _, it := range l.items {
			if it.FontSize > l.size {
				l.size = it.FontSize
			}
		}
	}
	return out
}

// lineText joins the line's characters into plain text.
func lineText(l itemLine) string {
	var b strings.Builder
	for _, it := range l.items {
		b.WriteString(it.S)
	}
	return b.String()
}

// splitCells cuts a line into cells at horizontal gaps larger than the
// cell-gap threshold (a function of the line's font size). Character
// advance widths make inter-word gaps (a space: ~0.25 x size) far smaller
// than inter-column gaps (8pt and up on real statements).
func splitCells(l itemLine) lineCells {
	gap := l.size * cellGapFactor
	if gap < minCellGap {
		gap = minCellGap
	}
	lc := lineCells{}
	var b strings.Builder
	startX := 0.0
	prevEnd := -1e9
	inCell := false
	flush := func() {
		cell := strings.Join(strings.Fields(b.String()), " ")
		b.Reset()
		if cell != "" {
			lc.xs = append(lc.xs, startX)
			lc.cells = append(lc.cells, cell)
		}
		inCell = false
	}
	for _, it := range l.items {
		if inCell && it.X-prevEnd > gap {
			flush()
		}
		if !inCell {
			startX = it.X
			inCell = true
		}
		b.WriteString(it.S)
		prevEnd = it.X + it.W
	}
	if inCell {
		flush()
	}
	return lc
}

// aligns reports whether a line's cells sit on the column anchors. One
// slightly-off cell is tolerated (footnote markers, stray punctuation);
// anything further out than twice the tolerance is a different layout.
func (lc lineCells) aligns(anchors []float64) bool {
	if len(lc.xs) != len(anchors) {
		return false
	}
	misses := 0
	for i, x := range lc.xs {
		if d := abs(x - anchors[i]); d > alignTolerance {
			if d > 2*alignTolerance {
				return false
			}
			misses++
		}
	}
	return misses <= 1
}

// alignsLoose is the header-hoist test: the candidate header may carry one
// fewer or one more cell than the block, but its cells must sit on the
// anchors.
func (lc lineCells) alignsLoose(anchors []float64) bool {
	if len(lc.xs) == 0 || abs(float64(len(lc.xs)-len(anchors))) > 1 {
		return false
	}
	n := len(lc.xs)
	if n > len(anchors) {
		n = len(anchors)
	}
	for i := 0; i < n; i++ {
		if abs(lc.xs[i]-anchors[i]) > alignTolerance {
			return false
		}
	}
	return true
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// tableBlock is a detected table: the aligned lines of its body in
// document order.
type tableBlock struct {
	start int // index of the first body line on the page
	cells []lineCells
}

// isTableRow classifies a line as table body: at least three cells, at
// least one numeric. Header lines (no numbers) cannot start a block but
// join one through alignment.
func isTableRow(lc lineCells) bool {
	if len(lc.cells) < 3 {
		return false
	}
	for _, c := range lc.cells {
		if isNumericCell(c) {
			return true
		}
	}
	return false
}

// blocksOnPage merges consecutive aligned table rows into blocks, sliding
// over up to maxLabelGap wrapped label lines inside a block (their text
// folds into the next row's first cell).
func blocksOnPage(lcs []lineCells) []tableBlock {
	var blocks []tableBlock
	var pending []string

	addPending := func(cell string) {
		pending = append(pending, cell)
		if len(pending) > maxLabelGap {
			pending = nil
		}
	}

	for i, lc := range lcs {
		// a blank line (whitespace only) is neither row nor label: it must
		// not touch the pending label or the open block
		if len(lc.cells) == 0 {
			continue
		}
		row := isTableRow(lc)
		b := len(blocks) - 1

		// aligned with the open block: extend it (or stash the label)
		if b >= 0 && lc.aligns(blocks[b].cells[0].xs) {
			if row {
				if len(pending) > 0 {
					lc.cells[0] = strings.Join(pending, " ") + " " + lc.cells[0]
					pending = nil
				}
				blocks[b].cells = append(blocks[b].cells, lc)
			} else {
				addPending(lc.cells[0])
			}
			continue
		}

		if row {
			// misaligned row: close any open block, start a new one
			pending = nil
			blocks = append(blocks, tableBlock{start: i, cells: []lineCells{lc}})
			continue
		}
		if b >= 0 {
			addPending(lc.cells[0])
		}
	}
	return blocks
}

// tablesOnPage turns one page's blocks into ExtractedTables.
func tablesOnPage(items []pdf.Text, page int) []rag.ExtractedTable {
	lines := pageLines(items)
	lcs := make([]lineCells, len(lines))
	texts := make([]string, len(lines))
	for i, l := range lines {
		lcs[i] = splitCells(l)
		texts[i] = lineText(l)
	}

	var out []rag.ExtractedTable
	for _, b := range blocksOnPage(lcs) {
		cols := len(b.cells[0].xs)
		if cols < 2 || cols > maxTableCols || len(b.cells) < minTableLines {
			continue
		}
		anchors := b.cells[0].xs

		// hoist aligned header lines sitting directly above the body
		header := lineCells{}
		hoistFrom := b.start
		for i := b.start - 1; i >= 0 && b.start-i <= headerHoistMax; i-- {
			if !lcs[i].alignsLoose(anchors) {
				break
			}
			header = lcs[i]
			hoistFrom = i
		}

		// find the caption above the table
		caption := ""
		best := 0
		for i := hoistFrom - 1; i >= 0 && hoistFrom-i <= captionScanMax; i-- {
			sc := captionScore(texts[i])
			if sc > best && sc >= captionMinScore {
				best = sc
				caption = texts[i]
			}
		}

		rows := make([][]string, 0, len(b.cells))
		for _, lc := range b.cells {
			rows = append(rows, cleanCells(lc.cells))
		}
		out = append(out, rag.ExtractedTable{
			Page:    page,
			Caption: truncateCaption(caption),
			Header:  cleanCells(header.cells),
			Rows:    rows,
		})
		if len(out) >= maxTablesPage {
			break
		}
	}
	return out
}

// cleanCells applies the final cell cleanup: dot leaders collapse, padding
// whitespace goes away, decoration-only cells drop out.
func cleanCells(cells []string) []string {
	out := make([]string, len(cells))
	for i, c := range cells {
		c = dotRunRe.ReplaceAllString(c, " ")
		c = strings.TrimSpace(c)
		if strings.Trim(c, ".-–— ") == "" {
			c = strings.Trim(c, " ")
		}
		out[i] = c
	}
	return out
}

// truncateCaption caps a caption line at a readable length, cutting at a
// rune boundary (captions can carry ₹ and dashes).
func truncateCaption(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= 160 {
		return s
	}
	return strings.TrimRight(string(r[:160]), " ")
}
