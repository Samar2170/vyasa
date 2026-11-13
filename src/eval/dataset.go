package eval

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Item is one golden-dataset question. Its ground truth points at the
// chunks that answer the question through (DocumentURL, Ordinals) — the
// stable identity of a chunk across re-indexing, since ordinals come from
// the deterministic chunkers while database ids are recreated on every run.
type Item struct {
	ID          string `json:"id"`
	Symbol      string `json:"symbol"`
	Segment     string `json:"segment"`
	Period      string `json:"period,omitempty"`
	Question    string `json:"question"`
	Answer      string `json:"answer,omitempty"`
	Style       string `json:"style,omitempty"` // lookup | numeric | period | qualitative
	DocumentURL string `json:"document_url"`
	Ordinals    []int  `json:"ordinals"`
}

// ChunkKey identifies one chunk independently of the database.
type ChunkKey struct {
	DocumentURL string
	Ordinal     int
}

// Validate reports whether the item carries everything the runner needs.
func (it Item) Validate() error {
	var missing []string
	for name, val := range map[string]string{
		"id": it.ID, "symbol": it.Symbol, "segment": it.Segment,
		"question": it.Question, "document_url": it.DocumentURL,
	} {
		if strings.TrimSpace(val) == "" {
			missing = append(missing, name)
		}
	}
	if len(it.Ordinals) == 0 {
		missing = append(missing, "ordinals")
	}
	for _, o := range it.Ordinals {
		if o < 0 {
			missing = append(missing, "negative ordinal")
			break
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("item %q: missing %s", it.ID, strings.Join(missing, ", "))
	}
	return nil
}

// RelevantKeys maps the chunks that count as an answer for this item.
// window widens every target ordinal by ±window neighbours, tolerating
// hard-split chunk boundaries that straddle one fact; 0 means strict.
func (it Item) RelevantKeys(window int) map[ChunkKey]struct{} {
	keys := make(map[ChunkKey]struct{}, len(it.Ordinals)*(2*window+1))
	for _, o := range it.Ordinals {
		for d := -window; d <= window; d++ {
			n := o + d
			if n < 0 {
				continue
			}
			keys[ChunkKey{DocumentURL: it.DocumentURL, Ordinal: n}] = struct{}{}
		}
	}
	return keys
}

// LoadJSONL reads one JSON object per line, skipping blanks and returning an
// error naming the offending line on malformed input or duplicate ids.
func LoadJSONL(path string) ([]Item, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("eval: open dataset: %w", err)
	}
	defer f.Close()

	var items []Item
	seen := make(map[string]bool)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		row := strings.TrimSpace(sc.Text())
		if row == "" || strings.HasPrefix(row, "//") {
			continue
		}
		var it Item
		if err := json.Unmarshal([]byte(row), &it); err != nil {
			return nil, fmt.Errorf("eval: %s:%d: %w", path, line, err)
		}
		if err := it.Validate(); err != nil {
			return nil, fmt.Errorf("eval: %s:%d: %w", path, line, err)
		}
		if seen[it.ID] {
			return nil, fmt.Errorf("eval: %s:%d: duplicate id %q", path, line, it.ID)
		}
		seen[it.ID] = true
		items = append(items, it)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("eval: read dataset: %w", err)
	}
	return items, nil
}

// SaveJSONL writes the items one per line, creating the parent directory
// when it does not exist.
func SaveJSONL(path string, items []Item) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("eval: create dataset dir: %w", err)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("eval: create dataset: %w", err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, it := range items {
		row, err := json.Marshal(it)
		if err != nil {
			return fmt.Errorf("eval: encode item %q: %w", it.ID, err)
		}
		row = append(row, '\n')
		if _, err := w.Write(row); err != nil {
			return fmt.Errorf("eval: write dataset: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("eval: flush dataset: %w", err)
	}
	return nil
}
