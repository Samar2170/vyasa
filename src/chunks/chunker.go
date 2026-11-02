package chunks

import (
	"regexp"
	"strconv"
	"strings"
)

type Chunker interface {
	Name() string
	Chunk(text string) []Chunk
}
type FixedChunker struct {
	// Size is the target chunk length in runes; Overlap the shared tail.
	Size    int
	Overlap int
}

func (f FixedChunker) Name() string { return "fixed" }

func (f FixedChunker) Chunk(text string) []Chunk {
	size, overlap := f.Size, f.Overlap
	if size <= 0 {
		size = defaultChunkSize
	}
	if overlap <= 0 {
		overlap = defaultOverlap
	}
	return splitSized(text, size, overlap)
}

// Segments with structure-aware chunkers. These mirror the analysis segment
// ids used by the ir client (defined there; duplicated here so chunks does
// not depend on ir).
const (
	SegAnnualReport = "annual_report"
	SegPresentation = "investor_presentation"
)

// hardOverlap is the rune overlap used when a single oversized block is
// hard-split.
const hardOverlap = 200

// ForSegment returns the chunking strategy for an analysis segment. Unknown
// segments fall back to FixedChunker.
func ForSegment(segment string) Chunker {
	switch segment {
	case SegAnnualReport:
		return AnnualReportChunker{}
	case SegPresentation:
		return PresentationChunker{}
	default:
		return FixedChunker{}
	}
}

// docBlock is one paragraph-sized piece of a parsed document.
type docBlock struct {
	text   string
	offset int // rune offset of text within the source document
	page   int // page marker in effect; 0 when the document has none
}

// pageMarkerRE matches the [[page N]] lines emitted by PDF extraction.
var pageMarkerRE = regexp.MustCompile(`^\[\[page (\d+)\]\]$`)

// docBlocks splits a parsed document into paragraph blocks: blank-line
// separated paragraphs, with [[page N]] marker lines updating the page
// counter rather than becoming content.
func docBlocks(text string) []docBlock {
	runes := []rune(text)
	var out []docBlock
	page := 0
	lineStart := 0
	var para []rune
	paraOffset := 0

	flush := func() {
		if len(para) == 0 {
			return
		}
		if txt := strings.TrimSpace(string(para)); txt != "" {
			out = append(out, docBlock{text: txt, offset: paraOffset, page: page})
		}
		para = nil
	}

	for i := 0; i <= len(runes); i++ {
		if i < len(runes) && runes[i] != '\n' {
			continue
		}
		line := strings.TrimSpace(string(runes[lineStart:i]))
		switch {
		case pageMarkerRE.MatchString(line):
			flush()
			if m := pageMarkerRE.FindStringSubmatch(line); m != nil {
				page, _ = strconv.Atoi(m[1])
			}
		case line == "":
			flush()
		default:
			if len(para) == 0 {
				paraOffset = lineStart
			}
			para = append(para, runes[lineStart:i]...)
			para = append(para, '\n')
		}
		lineStart = i + 1
	}
	flush()
	return out
}
