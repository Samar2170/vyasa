package chunks

import (
	"strings"
	"testing"
)

func TestSplitShortText(t *testing.T) {
	parts := Split("hello world")
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want 1", len(parts))
	}
	if parts[0].Offset != 0 || parts[0].Text != "hello world" {
		t.Fatalf("unexpected chunk: %+v", parts[0])
	}
}

func TestSplitEmpty(t *testing.T) {
	if parts := Split(""); parts != nil {
		t.Fatalf("Split(\"\") = %v, want nil", parts)
	}
}

func TestSplitOverlapAndCoverage(t *testing.T) {
	text := strings.Repeat("a", 5000)
	parts := Split(text)
	if len(parts) != 4 {
		t.Fatalf("parts = %d, want 4", len(parts))
	}
	wantOffsets := []int{0, 1300, 2600, 3900}
	for i, p := range parts {
		if p.Offset != wantOffsets[i] {
			t.Errorf("chunk %d offset = %d, want %d", i, p.Offset, wantOffsets[i])
		}
		if p.Ordinal != i {
			t.Errorf("chunk %d ordinal = %d", i, p.Ordinal)
		}
	}

	// Adjacent chunks must overlap by defaultOverlap runes.
	r0, r1 := []rune(parts[0].Text), []rune(parts[1].Text)
	if string(r0[defaultChunkSize-defaultOverlap:]) != string(r1[:defaultOverlap]) {
		t.Fatalf("expected %d-rune overlap between chunks 0 and 1", defaultOverlap)
	}

	// The chunks together must cover the whole document.
	last := parts[len(parts)-1]
	if last.Offset+len([]rune(last.Text)) != 5000 {
		t.Fatalf("last chunk ends at %d, want 5000", last.Offset+len([]rune(last.Text)))
	}
}

func TestCollapseBlankLines(t *testing.T) {
	if got := CollapseBlankLines("a\n\n\n\nb  \n"); got != "a\n\nb" {
		t.Errorf("CollapseBlankLines = %q", got)
	}
	if got := CollapseBlankLines(""); got != "" {
		t.Errorf("CollapseBlankLines(\"\") = %q", got)
	}
}
