package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"

	"vyasa/src/models"
	"vyasa/src/store"
)

// RunOptions controls one evaluation pass.
type RunOptions struct {
	Ks      []int           // cutoffs, e.g. 1,3,5,8; must be sorted ascending
	Window  int             // ±ordinal tolerance for relevant chunks (0 = strict)
	Symbols map[string]bool // optional symbol filter
	Segment string          // optional segment filter
}

// ItemResult records one dataset item's retrieval outcome.
type ItemResult struct {
	Item    Item               `json:"item"`
	Hits    int                `json:"hits"`            // chunks retrieved
	Ranks   []int              `json:"ranks,omitempty"` // 1-based ranks of relevant hits
	Metrics map[string]Metrics `json:"metrics"`         // keyed by k
	Err     string             `json:"error,omitempty"` // non-empty = item failed
}

// Report aggregates the per-item outcomes.
type Report struct {
	Items          int
	Ks             []int
	Window         int
	Results        []ItemResult
	Failed         int // items skipped because the symbol is not indexed/seeded
	Overall        map[int]Metrics
	BySymbol       map[string]map[int]Metrics
	BySegment      map[string]map[int]Metrics
	CountBySymbol  map[string]int
	CountBySegment map[string]int
}

// Searcher is the retrieval surface the runner exercises. Production passes
// the rag.Indexer; tests can pass fakes.
type Searcher interface {
	Search(ctx context.Context, companyID, segment, query string, topK int) ([]models.RAGHit, error)
}

// Run replays every dataset item through the searcher and scores retrieval.
// Each item costs exactly one hybrid search (topK = max k, whose candidate
// pool is >= 50), so every requested cutoff is scored from the same ranking.
func Run(ctx context.Context, ix Searcher, st *store.Store, items []Item, opts RunOptions) (*Report, error) {
	rep := &Report{
		Ks:             opts.Ks,
		Window:         opts.Window,
		Overall:        make(map[int]Metrics),
		BySymbol:       make(map[string]map[int]Metrics),
		BySegment:      make(map[string]map[int]Metrics),
		CountBySymbol:  make(map[string]int),
		CountBySegment: make(map[string]int),
	}
	maxK := 0
	for _, k := range opts.Ks {
		if k > maxK {
			maxK = k
		}
	}
	if maxK <= 0 {
		return rep, fmt.Errorf("eval: no valid k cutoffs")
	}

	companyIDs := make(map[string]string) // symbol -> company id
	for _, it := range items {
		if len(opts.Symbols) > 0 && !opts.Symbols[it.Symbol] {
			continue
		}
		if opts.Segment != "" && it.Segment != opts.Segment {
			continue
		}
		rep.Items++

		res := ItemResult{Item: it, Metrics: make(map[string]Metrics)}

		id, ok := companyIDs[it.Symbol]
		if !ok {
			co, err := st.GetCompany(ctx, it.Symbol)
			if err != nil {
				return rep, fmt.Errorf("eval: company %s: %w", it.Symbol, err)
			}
			if co == nil {
				rep.Failed++
				res.Err = "symbol not seeded"
				rep.Results = append(rep.Results, res)
				continue
			}
			id, companyIDs[it.Symbol] = co.ID, co.ID
		}

		hits, err := ix.Search(ctx, id, it.Segment, it.Question, maxK)
		if err != nil {
			return rep, fmt.Errorf("eval: search %s: %w", it.ID, err)
		}
		res.Hits = len(hits)

		relevant := it.RelevantKeys(opts.Window)
		rel := make([]bool, len(hits))
		for i, h := range hits {
			if _, ok := relevant[ChunkKey{DocumentURL: h.DocumentURL, Ordinal: h.Ordinal}]; ok {
				rel[i] = true
				res.Ranks = append(res.Ranks, i+1)
			}
		}
		// each item counts exactly once towards its groups
		symAgg := ensureGroup(rep.BySymbol, rep.CountBySymbol, it.Symbol)
		segAgg := ensureGroup(rep.BySegment, rep.CountBySegment, it.Segment)
		for _, k := range opts.Ks {
			m := Score(rel, len(it.Ordinals), k)
			res.Metrics[strconv.Itoa(k)] = m
			acc(rep.Overall, k, m)
			acc(symAgg, k, m)
			acc(segAgg, k, m)
		}
		rep.Results = append(rep.Results, res)
	}
	finalize(rep)
	return rep, nil
}

// acc accumulates one item's metrics into a group total (means are taken in
// finalize).
func acc(dst map[int]Metrics, k int, m Metrics) {
	cur := dst[k]
	cur.Hit += m.Hit
	cur.Recall += m.Recall
	cur.MRR += m.MRR
	cur.NDCG += m.NDCG
	dst[k] = cur
}

// ensureGroup creates the aggregation bucket for one item's group and
// counts the item once.
func ensureGroup(groups map[string]map[int]Metrics, counts map[string]int, name string) map[int]Metrics {
	if groups[name] == nil {
		groups[name] = make(map[int]Metrics)
		counts[name] = 0
	}
	counts[name]++
	return groups[name]
}

// finalize turns the accumulated sums into means.
func finalize(rep *Report) {
	avg := func(src map[int]Metrics, n int, dst map[int]Metrics) {
		if n == 0 {
			return
		}
		for k, m := range src {
			dst[k] = Metrics{
				Hit:    m.Hit / float64(n),
				Recall: m.Recall / float64(n),
				MRR:    m.MRR / float64(n),
				NDCG:   m.NDCG / float64(n),
			}
		}
	}
	avg(rep.Overall, rep.Items, rep.Overall)
	for name, src := range rep.BySymbol {
		avg(src, rep.CountBySymbol[name], src)
	}
	for name, src := range rep.BySegment {
		avg(src, rep.CountBySegment[name], src)
	}
}

// groupRow is one printable report line: a group name, its item count and
// its aggregated metrics.
type groupRow struct {
	name  string
	count int
	agg   map[int]Metrics
}

// Print writes the aggregate report: overall, then per segment, then per
// symbol, one table row each with Hit@k, Recall@k per cutoff plus MRR and
// nDCG at the largest cutoff.
func (rep *Report) Print(w io.Writer) {
	rows := []groupRow{{name: "overall", count: rep.Items, agg: rep.Overall}}
	for _, name := range sortedKeys(rep.BySegment) {
		rows = append(rows, groupRow{"  " + name, rep.CountBySegment[name], rep.BySegment[name]})
	}
	for _, name := range sortedKeys(rep.BySymbol) {
		rows = append(rows, groupRow{"  " + name, rep.CountBySymbol[name], rep.BySymbol[name]})
	}

	tol := "strict"
	if rep.Window > 0 {
		tol = fmt.Sprintf("±%d ordinal", rep.Window)
	}
	fmt.Fprintf(w, "[rag-eval] %d items, tolerance %s\n\n", rep.Items, tol)

	maxK := rep.Ks[len(rep.Ks)-1]
	header := fmt.Sprintf("%-28s %3s", "group", "n")
	for _, k := range rep.Ks {
		header += fmt.Sprintf(" hit@%-3d rec@%-3d", k, k)
	}
	header += fmt.Sprintf("    mrr  ndcg@%d", maxK)
	fmt.Fprintln(w, header)

	for _, row := range rows {
		fmt.Fprintf(w, "%-28s %3d", row.name, row.count)
		for _, k := range rep.Ks {
			m := row.agg[k]
			fmt.Fprintf(w, "  %5.2f  %6.2f", m.Hit, m.Recall)
		}
		fmt.Fprintf(w, "  %5.2f  %6.2f\n", row.agg[maxK].MRR, row.agg[maxK].NDCG)
	}
}

// SaveResults writes the per-item outcomes as JSONL for diffing across runs.
// An empty path skips the write.
func (rep *Report) SaveResults(path string) error {
	if path == "" {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("eval: create results file: %w", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range rep.Results {
		if err := enc.Encode(r); err != nil {
			return fmt.Errorf("eval: encode result: %w", err)
		}
	}
	return f.Sync()
}

// sortedKeys returns the group names of an aggregation map sorted
// alphabetically.
func sortedKeys(m map[string]map[int]Metrics) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
