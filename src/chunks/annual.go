package chunks

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	annualTarget = 1200 // preferred chunk length in runes
	annualMax    = 1800 // hard cap before a chunk is flushed
)

// AnnualReportChunker chunks annual reports along their section structure:
// every detected section heading starts a new chunk, paragraphs are packed
// up to the target size and each chunk carries its section and page as
// retrieval context.
type AnnualReportChunker struct {
	// Target is the preferred chunk length in runes; Max the hard cap.
	Target int
	Max    int
}

func (c AnnualReportChunker) Name() string { return "annual_report" }

func (c AnnualReportChunker) limits() (target, max int) {
	target, max = c.Target, c.Max
	if target <= 0 {
		target = annualTarget
	}
	if max <= target {
		max = annualMax
	}
	return target, max
}

func (c AnnualReportChunker) Chunk(text string) []Chunk {
	target, max := c.limits()
	blocks := docBlocks(text)
	if len(blocks) == 0 {
		return nil
	}

	var out []Chunk
	var cur []docBlock
	size := 0
	section := ""

	context := func(b docBlock) string {
		switch {
		case section != "" && b.page > 0:
			return fmt.Sprintf("%s — page %d", section, b.page)
		case section != "":
			return section
		case b.page > 0:
			return fmt.Sprintf("page %d", b.page)
		default:
			return ""
		}
	}
	flush := func() {
		if len(cur) == 0 {
			return
		}
		var b strings.Builder
		for i, blk := range cur {
			if i > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(blk.text)
		}
		out = append(out, Chunk{
			Ordinal: len(out),
			Offset:  cur[0].offset,
			Text:    b.String(),
			Context: context(cur[0]),
		})
		cur, size = nil, 0
	}

	for _, blk := range blocks {
		if isAnnualHeading(blk.text) {
			flush()
			section = headingTitle(blk.text)
		}
		n := utf8.RuneCountInString(blk.text)
		if n > max {
			// an oversized paragraph (dense tables collapse into one line)
			// is hard-split instead of dropped
			flush()
			for _, part := range splitSized(blk.text, target, hardOverlap) {
				out = append(out, Chunk{
					Ordinal: len(out),
					Offset:  blk.offset + part.Offset,
					Text:    part.Text,
					Context: context(blk),
				})
			}
			continue
		}
		if len(cur) > 0 && size+n+2 > max {
			flush()
		}
		cur = append(cur, blk)
		size += n + 2
		if size >= target {
			flush()
		}
	}
	flush()
	return out
}

// annualHeadings are the standard sections of an Indian annual report,
// matched case-insensitively against short standalone lines.
var annualHeadings = []string{
	"board's report", "board’s report",
	"directors' report", "directors’ report", "directors report",
	"chairman's letter", "chairman’s letter",
	"management discussion",
	"corporate governance",
	"auditor's report", "auditor’s report", "independent auditor",
	"financial statements", "notes to the", "annexure",
	"key managerial", "risk management", "business responsibility",
	"corporate information", "ten year", "notice",
}

// isAnnualHeading reports whether a paragraph looks like an annual report
// section heading: a short standalone line that is either shouted in
// capitals or names a standard report section.
func isAnnualHeading(text string) bool {
	s := strings.TrimSpace(text)
	if s == "" || strings.Contains(s, "\n") || utf8.RuneCountInString(s) > 80 {
		return false
	}
	var letters, upper int
	for _, r := range s {
		if unicode.IsLetter(r) {
			letters++
			if unicode.IsUpper(r) {
				upper++
			}
		}
	}
	if letters >= 3 && upper*5 >= letters*4 { // >=80% capitals
		return true
	}
	lower := strings.ToLower(s)
	for _, kw := range annualHeadings {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// headingTitle normalises a heading for use as chunk context.
func headingTitle(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
