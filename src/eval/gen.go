package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"

	"vyasa/src/llm"
	"vyasa/src/store"
)

// GenOptions controls golden-dataset generation.
type GenOptions struct {
	Symbols []string // restrict to these symbols; empty = every indexed symbol
	Total   int      // approximate question count before validation rejects
	Workers int      // concurrent LLM calls (1 = sequential)
	MinLen  int      // minimum chunk runes worth a question
	MaxLen  int      // maximum chunk runes worth a question
}

// chunkGroup is one (symbol, segment) bucket of question-worthy chunks.
type chunkGroup struct {
	Symbol  string
	Segment string
	Name    string // company display name for the prompt
	Chunks  []store.RagSample
}

// genJob is one sampled chunk queued for question writing.
type genJob struct {
	sample store.RagSample
	name   string // company display name
}

// questionReply is the JSON the question-writing model must return.
type questionReply struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
	Style    string `json:"style"`
}

var (
	// Boilerplate chunks never make good questions.
	boilerplate = regexp.MustCompile(`(?i)(forward[- ]looking statement|safe harbou?r|table of contents|disclaimer|this page is intentionally|intentionally left blank)`)

	// Questions that reference the source text are useless for retrieval.
	sourceReferring = regexp.MustCompile(`(?i)\b(excerpt|passage|chunk|paragraph|the document|the report|the filing|this text|given text|provided text|above)\b`)

	knownStyles = map[string]bool{"lookup": true, "numeric": true, "period": true, "qualitative": true}
)

// genSystemPrompt instructs the model to turn one chunk into one
// self-contained retrieval question plus its answer.
const genSystemPrompt = `You write evaluation questions for a financial-filings retrieval benchmark.
You are given one excerpt from a listed Indian company's investor filing.
Produce ONE question whose answer is contained in the excerpt.

Rules:
- Make the question self-contained: name the company, and include the fiscal period when the fact is period-specific.
- The question must be answerable from the excerpt alone; never require outside knowledge.
- Prefer concrete facts: revenue, margin, capex, capacity, orders, dividend, growth, plants, debt, headcount, segment split.
- Paraphrase; do not copy long phrases verbatim from the excerpt.
- Never refer to the excerpt, the document, the report, the filing or "the text".

Reply with strict JSON only, no commentary:
{"question": "...", "answer": "...", "style": "lookup"|"numeric"|"period"|"qualitative"}`

// Generate builds a draft golden dataset: it samples question-worthy chunks
// across (symbol, segment) groups, asks the model to write one grounded
// question per chunk, validates and dedupes the replies and writes the items
// as JSONL. Chunks the model could not turn into a usable question are
// skipped, so the returned count is approximate.
func Generate(ctx context.Context, st *store.Store, client llm.ModelClient, opts GenOptions, out string) ([]Item, error) {
	groups, err := sampleGroups(ctx, st, opts)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, fmt.Errorf("eval: no indexable chunks found (run: vyasa rag-index)")
	}

	jobs := selectChunks(groups, opts.Total)
	log.Printf("[rag-eval-gen] %d groups, %d candidate questions", len(groups), len(jobs))

	workers := opts.Workers
	if workers < 1 {
		workers = 1
	}
	type outcome struct {
		item Item
		err  error
	}
	results := make(chan outcome, len(jobs))
	jobsCh := make(chan genJob, len(jobs))
	for _, j := range jobs {
		jobsCh <- j
	}
	close(jobsCh)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobsCh {
				it, err := writeQuestion(ctx, client, j)
				if err != nil {
					// one retry: replies are occasionally truncated or
					// malformed; hygiene rejections sometimes improve on a
					// second draw
					it, err = writeQuestion(ctx, client, j)
				}
				results <- outcome{item: it, err: err}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	var items []Item
	seenQuestions := make(map[string]bool)
	done, rejected := 0, 0
	for res := range results {
		done++
		if res.err != nil {
			rejected++
			log.Printf("[rag-eval-gen] skip: %v", res.err)
			continue
		}
		key := normalizeQuestion(res.item.Question)
		if key == "" || seenQuestions[key] {
			rejected++
			continue
		}
		seenQuestions[key] = true
		items = append(items, res.item)
		log.Printf("[rag-eval-gen] %d/%d %s", done, len(jobs), res.item.ID)
	}

	sortItems(items)
	if err := SaveJSONL(out, items); err != nil {
		return nil, err
	}
	log.Printf("[rag-eval-gen] %d questions written to %s (%d rejected)", len(items), out, rejected)
	return items, nil
}

// sampleGroups loads every stored chunk, buckets them by (symbol, segment)
// and drops the chunks that would make bad question sources.
func sampleGroups(ctx context.Context, st *store.Store, opts GenOptions) ([]chunkGroup, error) {
	all, err := st.ListRAGChunks(ctx, "", "")
	if err != nil {
		return nil, fmt.Errorf("eval: list chunks: %w", err)
	}
	wanted := make(map[string]bool)
	for _, s := range opts.Symbols {
		wanted[strings.ToUpper(strings.TrimSpace(s))] = true
	}

	minLen := opts.MinLen
	if minLen <= 0 {
		minLen = 250
	}
	maxLen := opts.MaxLen
	if maxLen <= 0 {
		maxLen = 4000
	}

	var groups []chunkGroup
	index := make(map[string]int)
	for _, s := range all {
		if len(wanted) > 0 && !wanted[s.Symbol] {
			continue
		}
		text := strings.TrimSpace(s.Text)
		runes := len([]rune(text))
		if runes < minLen || runes > maxLen || boilerplate.MatchString(text) {
			continue
		}
		key := s.Symbol + "\x00" + s.Segment
		gi, ok := index[key]
		if !ok {
			co, err := st.GetCompany(ctx, s.Symbol)
			if err != nil {
				return nil, fmt.Errorf("eval: company %s: %w", s.Symbol, err)
			}
			if co == nil {
				return nil, fmt.Errorf("eval: company %s not seeded (run: vyasa seed)", s.Symbol)
			}
			groups = append(groups, chunkGroup{Symbol: s.Symbol, Segment: s.Segment, Name: co.Name})
			gi = len(groups) - 1
			index[key] = gi
		}
		groups[gi].Chunks = append(groups[gi].Chunks, s)
	}
	return groups, nil
}

// selectChunks spreads each group's quota evenly across its candidate list
// (deterministic stride) and returns the chosen chunks. Quotas are
// sqrt-weighted by group size so voluminous annual reports do not crowd out
// small segments like quarterly financials.
func selectChunks(groups []chunkGroup, total int) []genJob {
	if total <= 0 {
		total = 60
	}
	weight := 0.0
	for _, g := range groups {
		if len(g.Chunks) > 0 {
			weight += math.Sqrt(float64(len(g.Chunks)))
		}
	}
	if weight == 0 {
		return nil
	}

	var picked []genJob
	for _, g := range groups {
		n := len(g.Chunks)
		if n == 0 {
			continue
		}
		quota := int(math.Round(float64(total) * math.Sqrt(float64(n)) / weight))
		if quota < 2 {
			quota = 2
		}
		if quota > 20 {
			quota = 20
		}
		if quota > n {
			quota = n
		}
		for j := 0; j < quota; j++ {
			idx := (j*n + n/2) / quota
			picked = append(picked, genJob{sample: g.Chunks[idx], name: g.Name})
		}
	}
	return picked
}

// writeQuestion asks the model for one question-answer pair grounded in the
// chunk and validates the reply into an Item.
func writeQuestion(ctx context.Context, client llm.ModelClient, job genJob) (Item, error) {
	s := job.sample
	excerpt := []rune(strings.TrimSpace(s.Text))
	if len(excerpt) > 2500 {
		excerpt = excerpt[:2500]
	}
	user := fmt.Sprintf("Company: %s (symbol %s)\nSegment: %s\nPeriod: %s\nExcerpt:\n<<<\n%s\n>>>",
		orUnnamed(job.name), s.Symbol, s.Segment, orUnnamed(s.Period), string(excerpt))

	temp := 0.2
	maxTokens := 800
	resp, err := client.Complete(ctx, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Parts: []llm.Part{{Kind: llm.PartText, Text: genSystemPrompt}}},
			{Role: llm.RoleUser, Parts: []llm.Part{{Kind: llm.PartText, Text: user}}},
		},
		Temperature: &temp,
		MaxTokens:   &maxTokens,
	})
	if err != nil {
		return Item{}, fmt.Errorf("%s/%s#%d: llm: %w", s.Symbol, s.Segment, s.Ordinal, err)
	}
	if text := strings.TrimSpace(resp.Message.Text()); text == "" {
		return Item{}, fmt.Errorf("%s/%s#%d: empty llm reply", s.Symbol, s.Segment, s.Ordinal)
	}

	var reply questionReply
	if err := json.Unmarshal([]byte(stripFence(resp.Message.Text())), &reply); err != nil {
		return Item{}, fmt.Errorf("%s/%s#%d: decode reply: %w", s.Symbol, s.Segment, s.Ordinal, err)
	}
	q := strings.Join(strings.Fields(reply.Question), " ")
	if len([]rune(q)) < 15 || len([]rune(q)) > 300 {
		return Item{}, fmt.Errorf("%s/%s#%d: question length %d", s.Symbol, s.Segment, s.Ordinal, len([]rune(q)))
	}
	if !strings.HasSuffix(q, "?") {
		return Item{}, fmt.Errorf("%s/%s#%d: not a question: %q", s.Symbol, s.Segment, s.Ordinal, q)
	}
	if m := sourceReferring.FindString(q); m != "" {
		return Item{}, fmt.Errorf("%s/%s#%d: refers to source (%q)", s.Symbol, s.Segment, s.Ordinal, m)
	}
	style := reply.Style
	if !knownStyles[style] {
		style = "lookup"
	}
	return Item{
		ID:          fmt.Sprintf("%s-%s-%04d", slug(s.Symbol), slug(s.Segment), s.Ordinal),
		Symbol:      s.Symbol,
		Segment:     s.Segment,
		Period:      s.Period,
		Question:    q,
		Answer:      strings.TrimSpace(reply.Answer),
		Style:       style,
		DocumentURL: s.DocumentURL,
		Ordinals:    []int{s.Ordinal},
	}, nil
}

// sortItems orders the dataset by symbol, segment then ordinal for stable
// diffs between regeneration runs.
func sortItems(items []Item) {
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.Symbol != b.Symbol {
			return a.Symbol < b.Symbol
		}
		if a.Segment != b.Segment {
			return a.Segment < b.Segment
		}
		if a.DocumentURL != b.DocumentURL {
			return a.DocumentURL < b.DocumentURL
		}
		ao, bo := firstOrdinal(a), firstOrdinal(b)
		return ao < bo
	})
}

func firstOrdinal(it Item) int {
	if len(it.Ordinals) == 0 {
		return 0
	}
	return it.Ordinals[0]
}

// slug lowercases a string and maps every non-alphanumeric rune to '-'.
func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// stripFence removes a leading markdown code fence so JSON-only replies
// wrapped by the model still decode.
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}

func normalizeQuestion(q string) string { return strings.ToLower(strings.Join(strings.Fields(q), " ")) }

func orUnnamed(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
