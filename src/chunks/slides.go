package chunks

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	slideTarget = 1000 // preferred chunk length in runes
	slideMax    = 1600 // hard cap before a chunk is flushed
	// slideMergeMin keeps consecutive stub slides together instead of
	// emitting one tiny chunk per page.
	slideMergeMin = 200
)

// PresentationChunker chunks investor presentations slide by slide using
// the [[page N]] markers: one chunk per slide, tiny slides merged forward
// and oversized slides split, each chunk carrying its slide number as
// retrieval context. Documents without markers degrade to windowed
// splitting.
type PresentationChunker struct {
	// Target is the preferred chunk length in runes; Max the hard cap.
	Target int
	Max    int
}

func (c PresentationChunker) Name() string { return "investor_presentation" }

func (c PresentationChunker) limits() (target, max int) {
	target, max = c.Target, c.Max
	if target <= 0 {
		target = slideTarget
	}
	if max <= target {
		max = slideMax
	}
	return target, max
}

func (c PresentationChunker) Chunk(text string) []Chunk {
	target, max := c.limits()
	blocks := docBlocks(text)
	if len(blocks) == 0 {
		return nil
	}

	var out []Chunk
	var cur []docBlock
	size := 0

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
			Context: slideContext(cur),
		})
		cur, size = nil, 0
	}

	for _, blk := range blocks {
		n := utf8.RuneCountInString(blk.text)
		if n > max {
			flush()
			for _, part := range splitSized(blk.text, target, hardOverlap) {
				out = append(out, Chunk{
					Ordinal: len(out),
					Offset:  blk.offset + part.Offset,
					Text:    part.Text,
					Context: slideLabel(blk.page),
				})
			}
			continue
		}
		if len(cur) > 0 {
			newSlide := cur[0].page != blk.page
			if (newSlide && size >= slideMergeMin) || size+n+2 > max {
				flush()
			}
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

// slideContext describes the slides covered by a chunk.
func slideContext(cur []docBlock) string {
	first, last := cur[0].page, cur[len(cur)-1].page
	if first <= 0 {
		return ""
	}
	if first == last {
		return slideLabel(first)
	}
	return fmt.Sprintf("slides %d-%d", first, last)
}

func slideLabel(page int) string {
	if page <= 0 {
		return ""
	}
	return fmt.Sprintf("slide %d", page)
}
