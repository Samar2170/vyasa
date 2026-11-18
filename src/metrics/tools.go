// Agent tools over the ticker_metrics table: time series of selected metrics
// across years, and full-fy snapshots. Both follow the rag_search tool
// contract (types.Tool) and return compact JSON.
package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"vyasa/src/models"
	"vyasa/src/store"
	"vyasa/src/types"
)

// NewTools returns the metrics agent tools for registration in a tool
// registry.
func NewTools(st *store.Store) []types.Tool {
	return []types.Tool{NewSeriesTool(st), NewSnapshotTool(st)}
}

// SeriesTool exposes multi-metric, multi-year metric series (metrics_series).
type SeriesTool struct {
	st *store.Store
}

var _ types.Tool = (*SeriesTool)(nil)

// NewSeriesTool builds the metrics_series agent tool.
func NewSeriesTool(st *store.Store) *SeriesTool {
	return &SeriesTool{st: st}
}

func (t *SeriesTool) Name() string { return "metrics_series" }

func (t *SeriesTool) Description() string {
	return "Get annual financial metrics for a company across multiple years. " +
		"Pass one or more metric names (e.g. roe, revenue_yoy, ebitda_margin, net_debt) " +
		"and receive a per-FY series with the requested values; omit years for the full history. " +
		"A null value means the metric was not computable for that year."
}

func (t *SeriesTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"symbol":  {"type": "string", "description": "company symbol, e.g. RELIANCE"},
			"metrics": {"type": "array", "items": {"type": "string"}, "description": "metric names to return, e.g. [\"roe\", \"revenue_yoy\"], or [\"all\"] for every metric"},
			"years":   {"type": "array", "items": {"type": "string"}, "description": "optional FY labels to restrict the series to, e.g. [\"FY2023\", \"FY2024\", \"2025\"] (default: every stored FY)"}
		},
		"required": ["symbol", "metrics"]
	}`)
}

func (t *SeriesTool) Execute(ctx context.Context, args string) types.ExecutionOutput {
	start := time.Now()
	var req struct {
		Symbol  string   `json:"symbol"`
		Metrics []string `json:"metrics"`
		Years   []string `json:"years"`
	}
	if err := json.Unmarshal([]byte(args), &req); err != nil {
		return errOut(fmt.Sprintf("decode args: %v", err), 400, start)
	}
	if req.Symbol == "" {
		return errOut("symbol is required", 400, start)
	}
	if len(req.Metrics) == 0 {
		return errOut("metrics is required (e.g. [\"roe\"] or [\"all\"])", 400, start)
	}

	rows, err := t.st.GetMetrics(ctx, req.Symbol)
	if err != nil {
		return errOut(err.Error(), 500, start)
	}
	if len(rows) == 0 {
		return errOut(fmt.Sprintf("no metrics stored for %q (run: vyasa metrics)", req.Symbol), 404, start)
	}

	names, unknown := resolveMetricNames(req.Metrics)
	if len(unknown) > 0 {
		return errOut(fmt.Sprintf("unknown metric(s) %s — valid names: %s",
			strings.Join(unknown, ", "), strings.Join(models.TickerMetricColumns, ", ")), 400, start)
	}

	series, unmatched := filterYears(rows, req.Years)
	if len(series) == 0 {
		return errOut(fmt.Sprintf("no metrics for the requested year(s) %s — stored FYs: %s",
			strings.Join(req.Years, ", "), fyList(rows)), 404, start)
	}

	out := struct {
		Symbol    string      `json:"symbol"`
		Metrics   []string    `json:"metrics"`
		Series    []seriesRow `json:"series"`
		SkippedFY []string    `json:"skipped_years,omitempty"`
	}{Symbol: req.Symbol, Metrics: names, SkippedFY: unmatched}
	for _, r := range series {
		out.Series = append(out.Series, newSeriesRow(r, names))
	}
	return jsonOut(out, start)
}

// seriesRow is one FY slice of the requested metric series.
type seriesRow struct {
	FY      string         `json:"fy"`
	EndDate string         `json:"end_date"`
	Values  map[string]any `json:"values"`
}

// newSeriesRow projects a metrics row onto the requested metric names; absent
// metrics are emitted as null.
func newSeriesRow(m models.TickerMetrics, names []string) seriesRow {
	doc := rowJSON(m)
	values := make(map[string]any, len(names))
	for _, name := range names {
		v, ok := doc[name]
		if !ok {
			v = nil
		}
		values[name] = v
	}
	return seriesRow{FY: m.FY, EndDate: m.EndDate.Format("2006-01-02"), Values: values}
}

// SnapshotTool exposes every metric of a single FY (metrics_snapshot).
type SnapshotTool struct {
	st *store.Store
}

var _ types.Tool = (*SnapshotTool)(nil)

// NewSnapshotTool builds the metrics_snapshot agent tool.
func NewSnapshotTool(st *store.Store) *SnapshotTool {
	return &SnapshotTool{st: st}
}

func (t *SnapshotTool) Name() string { return "metrics_snapshot" }

func (t *SnapshotTool) Description() string {
	return "Get all computed financial metrics for a company for a single financial year " +
		"(raw figures, growth, margins, returns, solvency, working capital, cash-flow ratios), " +
		"plus data_gaps listing which values were not computable and why. " +
		"Use fy=\"latest\" for the most recent year."
}

func (t *SnapshotTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"symbol":  {"type": "string", "description": "company symbol, e.g. RELIANCE"},
			"fy":      {"type": "string", "description": "financial year, e.g. \"FY2025\", \"2025\" or \"latest\""},
			"metrics": {"type": "array", "items": {"type": "string"}, "description": "optional subset of metric names (default: all)"}
		},
		"required": ["symbol", "fy"]
	}`)
}

func (t *SnapshotTool) Execute(ctx context.Context, args string) types.ExecutionOutput {
	start := time.Now()
	var req struct {
		Symbol  string   `json:"symbol"`
		FY      string   `json:"fy"`
		Metrics []string `json:"metrics"`
	}
	if err := json.Unmarshal([]byte(args), &req); err != nil {
		return errOut(fmt.Sprintf("decode args: %v", err), 400, start)
	}
	if req.Symbol == "" || req.FY == "" {
		return errOut("symbol and fy are required", 400, start)
	}

	rows, err := t.st.GetMetrics(ctx, req.Symbol)
	if err != nil {
		return errOut(err.Error(), 500, start)
	}
	if len(rows) == 0 {
		return errOut(fmt.Sprintf("no metrics stored for %q (run: vyasa metrics)", req.Symbol), 404, start)
	}

	row, err := pickFY(rows, req.FY)
	if err != nil {
		return errOut(fmt.Sprintf("%v — stored FYs: %s", err, fyList(rows)), 404, start)
	}

	// Default to every metric when no subset is requested.
	if len(req.Metrics) == 0 {
		req.Metrics = []string{"all"}
	}
	names, unknown := resolveMetricNames(req.Metrics)
	if len(unknown) > 0 {
		return errOut(fmt.Sprintf("unknown metric(s) %s — valid names: %s",
			strings.Join(unknown, ", "), strings.Join(models.TickerMetricColumns, ", ")), 400, start)
	}

	doc := rowJSON(*row)
	values := make(map[string]any, len(names))
	for _, name := range names {
		v, ok := doc[name]
		if !ok {
			v = nil
		}
		values[name] = v
	}

	gaps := row.DataGaps
	if gaps == nil {
		gaps = []string{} // keep data_gaps an array, not null
	}
	out := struct {
		Symbol   string         `json:"symbol"`
		FY       string         `json:"fy"`
		EndDate  string         `json:"end_date"`
		DataGaps []string       `json:"data_gaps"`
		Metrics  map[string]any `json:"metrics"`
	}{
		Symbol:   row.Symbol,
		FY:       row.FY,
		EndDate:  row.EndDate.Format("2006-01-02"),
		DataGaps: gaps,
		Metrics:  values,
	}
	return jsonOut(out, start)
}

// rowJSON marshals a metrics row into a generic map keyed by column name
// (the model's JSON tags equal the SQL columns).
func rowJSON(m models.TickerMetrics) map[string]any {
	buf, err := json.Marshal(m)
	if err != nil {
		return map[string]any{}
	}
	var doc map[string]any
	if err := json.Unmarshal(buf, &doc); err != nil {
		return map[string]any{}
	}
	return doc
}

// resolveMetricNames expands "all" (case-insensitive) to every metric column
// and validates the requested names; unknown ones are returned for the error
// message.
func resolveMetricNames(requested []string) (names, unknown []string) {
	valid := make(map[string]bool, len(models.TickerMetricColumns))
	for _, c := range models.TickerMetricColumns {
		valid[c] = true
	}
	seen := make(map[string]bool)
	for _, r := range requested {
		r = strings.ToLower(strings.TrimSpace(r))
		if r == "" {
			continue
		}
		if r == "all" {
			for _, c := range models.TickerMetricColumns {
				if !seen[c] {
					seen[c] = true
					names = append(names, c)
				}
			}
			continue
		}
		if !valid[r] {
			unknown = append(unknown, r)
			continue
		}
		if !seen[r] {
			seen[r] = true
			names = append(names, r)
		}
	}
	return names, unknown
}

var fyLabelPattern = regexp.MustCompile(`^(?:FY)?\s*(\d{4})$`)

// normalizeFY accepts "FY2025", "fy 2025" and "2025" and returns the stored
// "FY2025" form, or "" when the label is not a valid FY.
func normalizeFY(s string) string {
	m := fyLabelPattern.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(s)))
	if m == nil {
		return ""
	}
	return "FY" + m[1]
}

// pickFY finds the row for an FY label; "latest" selects the newest row.
func pickFY(rows []models.TickerMetrics, label string) (*models.TickerMetrics, error) {
	if strings.EqualFold(strings.TrimSpace(label), "latest") {
		return &rows[len(rows)-1], nil
	}
	fy := normalizeFY(label)
	if fy == "" {
		return nil, fmt.Errorf("invalid fy %q (want e.g. FY2025 or latest)", label)
	}
	for i := range rows {
		if rows[i].FY == fy {
			return &rows[i], nil
		}
	}
	return nil, fmt.Errorf("no metrics for %s", fy)
}

// filterYears restricts rows to the requested FY labels (normalized; empty
// list keeps everything). Labels matching no stored row are returned so the
// caller can report them.
func filterYears(rows []models.TickerMetrics, years []string) (kept []models.TickerMetrics, unmatched []string) {
	if len(years) == 0 {
		return rows, nil
	}
	want := make(map[string]bool, len(years))
	for _, y := range years {
		if fy := normalizeFY(y); fy != "" {
			want[fy] = true
		} else {
			unmatched = append(unmatched, y)
		}
	}
	for _, r := range rows {
		if want[r.FY] {
			kept = append(kept, r)
		}
	}
	return kept, unmatched
}

// fyList renders the stored FY labels of a series for error messages.
func fyList(rows []models.TickerMetrics) string {
	labels := make([]string, len(rows))
	for i, r := range rows {
		labels[i] = r.FY
	}
	return strings.Join(labels, ", ")
}

// jsonOut marshals the payload as the tool's output.
func jsonOut(payload any, start time.Time) types.ExecutionOutput {
	buf, err := json.Marshal(payload)
	if err != nil {
		return errOut(fmt.Sprintf("encode output: %v", err), 500, start)
	}
	return types.ExecutionOutput{Output: string(buf), Duration: time.Since(start)}
}

// errOut builds a failed execution output.
func errOut(msg string, code int, start time.Time) types.ExecutionOutput {
	return types.ExecutionOutput{Error: msg, Code: code, Duration: time.Since(start)}
}
