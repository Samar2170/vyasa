package chunks

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	annualTarget = 1200 // preferred chunk length in runes
	annualMax    = 1800 // hard cap before a chunk is flushed
)

// AnnualReportChunker chunks annual reports along their section structure:
// every detected section heading starts a new chunk and paragraphs are packed
// up to the target size. Chunks carry only their page number as context —
// section titles are not recorded, because heading detection on jumbled PDF
// extraction proved unreliable and a wrong label sticks to hundreds of
// chunks.
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

	context := func(b docBlock) string {
		if b.page > 0 {
			return fmt.Sprintf("page %d", b.page)
		}
		return ""
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
		if _, _, ok := splitHeading(blk.text); ok {
			// the heading line stays part of the chunk text so lexical
			// search keeps matching on it; it is no longer recorded as the
			// chunk's section
			flush()
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
// matched case-insensitively as line prefixes (after stripping numbering).
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
	"report of the", "statement of", "financial highlights",
}

// headingPrefixRE strips the numbering annual report sections carry
// ("G13.", "2.1", "IV.", "Annexure I") before keyword matching.
var headingPrefixRE = regexp.MustCompile(
	`(?i)^(?:annexure\s*[a-z0-9]*|[ivxlcdm]+\.?|\d+(?:\.\d+)*|[a-z]\d*)[\s.):\-]+`)

// isAnnualHeading reports whether a paragraph opens with a section-heading
// line.
func isAnnualHeading(text string) bool {
	first, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return isHeadingLine(first)
}

// splitHeading separates the leading section-heading line of a paragraph
// from its body. PDF extraction rarely leaves a blank line under a heading,
// so "DIRECTORS' REPORT\nThe board presents…" is one paragraph whose first
// line is the heading.
func splitHeading(text string) (head, body string, ok bool) {
	first, rest, _ := strings.Cut(strings.TrimSpace(text), "\n")
	if !isHeadingLine(first) {
		return "", "", false
	}
	return first, rest, true
}

// isHeadingLine applies the heading rules to a single line. They are
// deliberately strict — jumbled PDF text glues page furniture to titles,
// and a wrong heading sticks to hundreds of chunks, so an unsectioned chunk
// is always preferred over a mis-sectioned one:
// no digits anywhere (kills "NOTES:1.", "G13.Risk Management…", cover-page
// year ranges), 2–12 words in at most 60 runes, no dangling punctuation,
// and keywords must prefix the line rather than appear anywhere in it.
func isHeadingLine(line string) bool {
	s := strings.TrimSpace(line)
	if s == "" || utf8.RuneCountInString(s) > 60 {
		return false
	}
	if strings.ContainsAny(s, "0123456789") {
		return false
	}
	words := strings.Fields(s)
	if len(words) < 2 || len(words) > 12 {
		return false
	}
	switch s[len(s)-1] {
	case '-', ':', ',':
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

	cleaned := headingPrefixRE.ReplaceAllString(strings.ToLower(s), "")
	if cleaned == "" {
		return false
	}
	for _, kw := range annualHeadings {
		if strings.HasPrefix(cleaned, kw) {
			return true
		}
	}
	return false
}
