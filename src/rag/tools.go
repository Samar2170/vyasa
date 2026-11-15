package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"vyasa/src/store"
	"vyasa/src/types"
)

// tableHitRunes caps how much of a table's markdown a hit may print.
const tableHitRunes = 1600

// SearchTool exposes hybrid retrieval over indexed filings to the agent.
type SearchTool struct {
	st *store.Store
	ix *Indexer
}

var _ types.Tool = (*SearchTool)(nil)

// NewSearchTool builds the rag_search agent tool.
func NewSearchTool(st *store.Store, ix *Indexer) *SearchTool {
	return &SearchTool{st: st, ix: ix}
}

func (t *SearchTool) Name() string { return "rag_search" }

func (t *SearchTool) Description() string {
	return "Search a company's archived investor-relations filings (annual reports, " +
		"investor presentations, earnings call transcripts, quarterly financials) with " +
		"hybrid semantic + keyword retrieval. Returns the most relevant prose passages " +
		"with their source document, section and score. For financial statement tables " +
		"and structured numbers use rag_table_search instead."
}

func (t *SearchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query":   {"type": "string", "description": "what to look for"},
			"symbol":  {"type": "string", "description": "company symbol, e.g. RELIANCE"},
			"segment": {"type": "string", "description": "optional filter: annual_report, investor_presentation, earnings_call_transcript or quarterly_financials"},
			"top_k":   {"type": "integer", "description": "max hits to return (default 8)"}
		},
		"required": ["query", "symbol"]
	}`)
}

func (t *SearchTool) Execute(ctx context.Context, args string) types.ExecutionOutput {
	start := time.Now()
	var req struct {
		Query   string `json:"query"`
		Symbol  string `json:"symbol"`
		Segment string `json:"segment"`
		TopK    int    `json:"top_k"`
	}
	if err := json.Unmarshal([]byte(args), &req); err != nil {
		return types.ExecutionOutput{
			Error: fmt.Sprintf("decode args: %v", err), Code: 400, Duration: time.Since(start),
		}
	}
	if req.Query == "" || req.Symbol == "" {
		return types.ExecutionOutput{
			Error: "query and symbol are required", Code: 400, Duration: time.Since(start),
		}
	}

	co, err := t.st.GetCompany(ctx, req.Symbol)
	if err != nil {
		return types.ExecutionOutput{Error: err.Error(), Code: 500, Duration: time.Since(start)}
	}
	if co == nil {
		return types.ExecutionOutput{
			Error: fmt.Sprintf("symbol %q not found", req.Symbol), Code: 404, Duration: time.Since(start),
		}
	}

	hits, err := t.ix.Search(ctx, co.ID, req.Segment, req.Query, req.TopK)
	if err != nil {
		return types.ExecutionOutput{Error: err.Error(), Code: 500, Duration: time.Since(start)}
	}
	if len(hits) == 0 {
		return types.ExecutionOutput{
			Output: "no matching passages found", Duration: time.Since(start),
		}
	}

	var b strings.Builder
	for i, h := range hits {
		label := h.Segment
		if h.Period != "" {
			label += " " + h.Period
		}
		if h.IsQuarterly {
			label += " [quarterly]"
		}
		section := ""
		if h.Section != "" {
			section = " — " + h.Section
		}
		score := fmt.Sprintf("score %.4f", h.Score)
		if h.Reranked {
			score = fmt.Sprintf("rerank %.4f (rrf %.4f)", h.RerankScore, h.Score)
		}
		fmt.Fprintf(&b, "[%d] %s — %s %s%s\n%s\nsource: %s\n\n",
			i+1, score, h.Symbol, label, section, h.Text, h.DocumentURL)
	}
	return types.ExecutionOutput{
		Output:   strings.TrimSpace(b.String()),
		Duration: time.Since(start),
	}
}

// TableSearchTool exposes retrieval over the extracted financial tables to
// the agent. Tables are a separate store and a separate tool so numeric
// questions get table-shaped answers (caption, page, rendered grid) instead
// of prose fragments.
type TableSearchTool struct {
	st *store.Store
	ix *Indexer
}

var _ types.Tool = (*TableSearchTool)(nil)

// NewTableSearchTool builds the rag_table_search agent tool.
func NewTableSearchTool(st *store.Store, ix *Indexer) *TableSearchTool {
	return &TableSearchTool{st: st, ix: ix}
}

func (t *TableSearchTool) Name() string { return "rag_table_search" }

func (t *TableSearchTool) Description() string {
	return "Search the financial tables extracted from a company's archived " +
		"investor-relations filings (profit & loss, balance sheet, cash flow, segment " +
		"results and similar statement grids). Returns whole tables as markdown with " +
		"their caption, page and fiscal period. Use this for numeric questions about " +
		"revenue, margins, assets, ratios and other statement figures; use rag_search " +
		"for narrative text."
}

func (t *TableSearchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query":   {"type": "string", "description": "what to look for, e.g. 'consolidated revenue by quarter'"},
			"symbol":  {"type": "string", "description": "company symbol, e.g. RELIANCE"},
			"segment": {"type": "string", "description": "optional filter: annual_report, investor_presentation, earnings_call_transcript or quarterly_financials"},
			"top_k":   {"type": "integer", "description": "max tables to return (default 8)"}
		},
		"required": ["query", "symbol"]
	}`)
}

func (t *TableSearchTool) Execute(ctx context.Context, args string) types.ExecutionOutput {
	start := time.Now()
	var req struct {
		Query   string `json:"query"`
		Symbol  string `json:"symbol"`
		Segment string `json:"segment"`
		TopK    int    `json:"top_k"`
	}
	if err := json.Unmarshal([]byte(args), &req); err != nil {
		return types.ExecutionOutput{
			Error: fmt.Sprintf("decode args: %v", err), Code: 400, Duration: time.Since(start),
		}
	}
	if req.Query == "" || req.Symbol == "" {
		return types.ExecutionOutput{
			Error: "query and symbol are required", Code: 400, Duration: time.Since(start),
		}
	}

	co, err := t.st.GetCompany(ctx, req.Symbol)
	if err != nil {
		return types.ExecutionOutput{Error: err.Error(), Code: 500, Duration: time.Since(start)}
	}
	if co == nil {
		return types.ExecutionOutput{
			Error: fmt.Sprintf("symbol %q not found", req.Symbol), Code: 404, Duration: time.Since(start),
		}
	}

	hits, err := t.ix.SearchTables(ctx, co.ID, req.Segment, req.Query, req.TopK)
	if err != nil {
		return types.ExecutionOutput{Error: err.Error(), Code: 500, Duration: time.Since(start)}
	}
	if len(hits) == 0 {
		return types.ExecutionOutput{
			Output: "no matching tables found", Duration: time.Since(start),
		}
	}

	var b strings.Builder
	for i, h := range hits {
		label := h.Segment
		if h.Period != "" {
			label += " " + h.Period
		}
		if h.IsQuarterly {
			label += " [quarterly]"
		}
		caption := h.Section
		if caption == "" {
			caption = "untitled table"
		}
		score := fmt.Sprintf("score %.4f", h.Score)
		if h.Reranked {
			score = fmt.Sprintf("rerank %.4f (rrf %.4f)", h.RerankScore, h.Score)
		}
		fmt.Fprintf(&b, "[%d] %s — %s %s — table %q, page %d\n%s\nsource: %s\n\n",
			i+1, score, h.Symbol, label, caption, h.Offset, truncateRunes(h.Text, tableHitRunes), h.DocumentURL)
	}
	return types.ExecutionOutput{
		Output:   strings.TrimSpace(b.String()),
		Duration: time.Since(start),
	}
}
