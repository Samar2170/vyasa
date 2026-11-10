package eval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidate(t *testing.T) {
	ok := Item{ID: "a", Symbol: "RELIANCE", Segment: "annual_report",
		Question: "What was net debt?", DocumentURL: "https://x/y.pdf", Ordinals: []int{4}}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid item rejected: %v", err)
	}

	bad := ok
	bad.Question = "   "
	if err := bad.Validate(); err == nil {
		t.Error("blank question accepted")
	}

	bad = ok
	bad.Ordinals = nil
	if err := bad.Validate(); err == nil {
		t.Error("no ordinals accepted")
	}

	bad = ok
	bad.Ordinals = []int{-1}
	if err := bad.Validate(); err == nil {
		t.Error("negative ordinal accepted")
	}
}

func TestRelevantKeys(t *testing.T) {
	it := Item{DocumentURL: "d.pdf", Ordinals: []int{5, 10}}

	strict := it.RelevantKeys(0)
	if len(strict) != 2 {
		t.Fatalf("strict keys = %d, want 2", len(strict))
	}

	tol := it.RelevantKeys(1)
	// 5±1 and 10±1: {4,5,6} ∪ {9,10,11}
	for _, want := range []int{4, 5, 6, 9, 10, 11} {
		if _, ok := tol[ChunkKey{"d.pdf", want}]; !ok {
			t.Errorf("missing neighbour ordinal %d", want)
		}
	}
	if _, ok := tol[ChunkKey{"other.pdf", 5}]; ok {
		t.Error("key leaked across documents")
	}

	// negative ordinals must not appear
	it2 := Item{DocumentURL: "d.pdf", Ordinals: []int{0}}
	if _, ok := it2.RelevantKeys(1)[ChunkKey{"d.pdf", -1}]; ok {
		t.Error("negative ordinal produced")
	}
}

func TestJSONLRoundTrip(t *testing.T) {
	items := []Item{
		{ID: "a-1", Symbol: "M&M", Segment: "annual_report", Period: "FY2025-26",
			Question: "What was revenue?", Answer: "₹1,00,000 crore", Style: "numeric",
			DocumentURL: "https://x/a.pdf", Ordinals: []int{3}},
		{ID: "a-2", Symbol: "JSL", Segment: "investor_presentation",
			Question: "How many plants?", DocumentURL: "https://x/b.pdf", Ordinals: []int{0}},
	}
	path := filepath.Join(t.TempDir(), "ds.jsonl")
	if err := SaveJSONL(path, items); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := LoadJSONL(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 2 || got[0].ID != "a-1" || got[0].Ordinals[0] != 3 || got[1].Symbol != "JSL" {
		t.Errorf("round trip mismatch: %+v", got)
	}
}

func TestLoadJSONLErrors(t *testing.T) {
	dir := t.TempDir()

	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	valid := Item{ID: "a", Symbol: "S", Segment: "annual_report", Question: "q?",
		DocumentURL: "u", Ordinals: []int{0}}
	full := `{"id":"a","symbol":"S","segment":"annual_report","question":"q?","document_url":"u","ordinals":[0]}`

	t.Run("duplicate id", func(t *testing.T) {
		if _, err := LoadJSONL(write("dup.jsonl", full+"\n"+full)); err == nil {
			t.Error("duplicate id accepted")
		}
	})
	t.Run("malformed json", func(t *testing.T) {
		if _, err := LoadJSONL(write("bad.jsonl", "{oops")); err == nil {
			t.Error("malformed line accepted")
		}
	})
	t.Run("invalid item", func(t *testing.T) {
		if _, err := LoadJSONL(write("inv.jsonl", `{"id":"","question":"q?"}`)); err == nil {
			t.Error("incomplete item accepted")
		}
	})
	t.Run("blank lines and comments skipped", func(t *testing.T) {
		got, err := LoadJSONL(write("ok.jsonl", "\n// note\n"+full+"\n"))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(got) != 1 || got[0].ID != valid.ID {
			t.Errorf("unexpected items: %+v", got)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		if _, err := LoadJSONL(filepath.Join(dir, "nope.jsonl")); err == nil {
			t.Error("missing file accepted")
		}
	})
}
