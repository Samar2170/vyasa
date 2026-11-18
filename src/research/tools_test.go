// Tests for the research workflow's pure pieces: the segment-forcing RAG
// wrappers, the DuckDuckGo parsing/unwrap helpers, visible-text extraction,
// the Q&A recorder and the ask tool's argument validation.
package research

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"vyasa/src/llm"
	"vyasa/src/models"
	"vyasa/src/types"
)

type captureTool struct {
	gotArgs string
}

func (t *captureTool) Name() string                { return "inner" }
func (t *captureTool) Description() string         { return "inner" }
func (t *captureTool) Parameters() json.RawMessage { return json.RawMessage(`{}`) }
func (t *captureTool) Execute(_ context.Context, args string) types.ExecutionOutput {
	t.gotArgs = args
	return types.ExecutionOutput{Output: "ok"}
}

func TestScopedToolForcesSegment(t *testing.T) {
	inner := &captureTool{}
	scoped := &scopedTool{name: "scoped", desc: "d", segment: "annual_report", inner: inner}

	scoped.Execute(context.Background(), `{"query":"capex","symbol":"RELIANCE","segment":"investor_presentation"}`)

	var doc map[string]any
	if err := json.Unmarshal([]byte(inner.gotArgs), &doc); err != nil {
		t.Fatalf("inner args not JSON: %v", err)
	}
	if doc["segment"] != "annual_report" {
		t.Errorf("segment = %v, want annual_report (forced)", doc["segment"])
	}
	if doc["query"] != "capex" || doc["symbol"] != "RELIANCE" {
		t.Errorf("other args lost: %v", doc)
	}
}

func TestScopedToolPassthroughOnBadArgs(t *testing.T) {
	inner := &captureTool{}
	scoped := &scopedTool{name: "scoped", segment: "annual_report", inner: inner}
	scoped.Execute(context.Background(), `not json`)
	if inner.gotArgs != `not json` {
		t.Errorf("malformed args not passed through: %q", inner.gotArgs)
	}
}

func TestScopedRAGRegistry(t *testing.T) {
	reg := scopedRAGRegistry(Deps{}, segmentAnnual, "annual", "annual report")
	for _, name := range []string{"annual_rag_search", "annual_rag_table_search"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("registry missing tool %q (has %v)", name, reg)
		}
	}
}

func TestWebToolsNames(t *testing.T) {
	got := map[string]bool{}
	for _, tool := range WebTools() {
		got[tool.Name()] = true
	}
	if !got["web_search"] || !got["web_fetch"] {
		t.Errorf("WebTools() = %v, want web_search + web_fetch", got)
	}
}

func TestUnwrapDDGLink(t *testing.T) {
	cases := map[string]string{
		"/l/?uddg=https%3A%2F%2Fexample.com%2Fa":        "https://example.com/a",
		"//duckduckgo.com/l/?uddg=https%3A%2F%2Fx.test": "https://x.test",
		"https://direct.example.com":                    "https://direct.example.com",
		"":                                              "",
	}
	for in, want := range cases {
		if got := unwrapDDGLink(in); got != want {
			t.Errorf("unwrapDDGLink(%q) = %q, want %q", in, got, want)
		}
	}
}

func mustParseHTML(t *testing.T, s string) *html.Node {
	t.Helper()
	root, err := html.Parse(strings.NewReader(s))
	if err != nil {
		t.Fatalf("html.Parse: %v", err)
	}
	return root
}

// hasLine reports whether the exact line appears in s.
func hasLine(t *testing.T, s, line string) bool {
	t.Helper()
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) == strings.TrimSpace(line) {
			return true
		}
	}
	return false
}

func TestParseWebResultsHTMLLayout(t *testing.T) {
	root := mustParseHTML(t, `<html><body>
		<div class="result">
			<a class="result__a" href="/l/?uddg=https%3A%2F%2Fone.example%2Fa">First result</a>
			<a class="result__snippet">Snippet one</a>
		</div>
		<div class="result">
			<a class="result__a" href="https://two.example/b">Second result</a>
			<a class="result__snippet">Snippet two</a>
		</div>
	</body></html>`)
	got := parseWebResults(root)
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2: %+v", len(got), got)
	}
	if got[0].Title != "First result" || got[0].URL != "https://one.example/a" || got[0].Snippet != "Snippet one" {
		t.Errorf("result 0 wrong: %+v", got[0])
	}
	if got[1].Title != "Second result" || got[1].URL != "https://two.example/b" || got[1].Snippet != "Snippet two" {
		t.Errorf("result 1 wrong: %+v", got[1])
	}
}

func TestParseWebResultsLiteLayout(t *testing.T) {
	root := mustParseHTML(t, `<html><body><table>
		<tr><td><a class="result-link" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Flite.example%2Fx">Lite hit</a></td></tr>
		<tr><td class="result-snippet">Lite snippet</td></tr>
	</table></body></html>`)
	got := parseWebResults(root)
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1: %+v", len(got), got)
	}
	if got[0].Title != "Lite hit" || got[0].URL != "https://lite.example/x" || got[0].Snippet != "Lite snippet" {
		t.Errorf("lite result wrong: %+v", got[0])
	}
}

func TestExtractVisibleText(t *testing.T) {
	root := mustParseHTML(t, `<html><head><style>body{color:red}</style></head>
		<body><h1>Report</h1><p>Hello <b>world</b></p><script>hide()</script><noscript>no js</noscript></body></html>`)
	got := extractVisibleText(root)
	if !hasLine(t, got, "Report") || !hasLine(t, got, "Hello world") {
		t.Errorf("visible text missing expected lines: %q", got)
	}
	for _, unwanted := range []string{"hide()", "color:red", "no js"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("unwanted content %q leaked: %q", unwanted, got)
		}
	}
}

func TestCollapseBlank(t *testing.T) {
	if got := collapseBlank("  a \n\n\tb\t\n \n c "); got != "a\nb\nc" {
		t.Errorf("collapseBlank = %q, want %q", got, "a\nb\nc")
	}
}

func TestIsHTML(t *testing.T) {
	cases := []struct {
		ct, body string
		want     bool
	}{
		{"text/html; charset=utf-8", "", true},
		{"", "<!DOCTYPE html><html>", true},
		{"", "<HTML><body", true},
		{"application/json", `{"a":1}`, false},
		{"text/plain", "<html", false},
	}
	for _, c := range cases {
		if got := isHTML(c.ct, c.body); got != c.want {
			t.Errorf("isHTML(%q, %q) = %v, want %v", c.ct, c.body, got, c.want)
		}
	}
}

func TestSessionRecorder(t *testing.T) {
	rec := &sessionRecorder{}
	rec.AddQA("q1", "a1")
	rec.AddQA("q2", "a2")
	cost := 0.5
	rec.AddUsage(llm.Usage{InputTokens: 10, OutputTokens: 5, Cost: &cost}, 2)
	cost2 := 0.25
	rec.AddUsage(llm.Usage{InputTokens: 20, OutputTokens: 15, Cost: &cost2}, 3)

	qa, u, steps := rec.Snapshot()
	if len(qa) != 2 || qa[0].Question != "q1" || qa[1].Answer != "a2" {
		t.Fatalf("qa snapshot wrong: %+v", qa)
	}
	if steps != 5 {
		t.Errorf("steps = %d, want 5", steps)
	}
	if u.InputTokens != 30 || u.OutputTokens != 20 || u.Cost == nil || *u.Cost != 0.75 {
		t.Errorf("usage snapshot wrong: %+v", u)
	}
}

func TestAskToolArgValidation(t *testing.T) {
	ask := NewAskTool(nil, nil, &sessionRecorder{}, nil)

	out := ask.Execute(context.Background(), `not json`)
	if out.Error == "" {
		t.Error("malformed args should error")
	}
	out = ask.Execute(context.Background(), `{"question":"   "}`)
	if out.Error != "question is required" {
		t.Errorf("empty question error = %q, want 'question is required'", out.Error)
	}
}

// TestAskToolBudgetRefusal verifies the budget-exhausted path answers
// inline as a normal tool result. The verifier is nil on purpose: if the
// tool tried to run it, the test would panic instead of returning.
func TestAskToolBudgetRefusal(t *testing.T) {
	budget := llm.NewCostBudget(0.01)
	budget.Add(0.01)
	ask := NewAskTool(nil, nil, &sessionRecorder{}, budget)

	out := ask.Execute(context.Background(), `{"question":"What is FY2025 ROE?"}`)
	if out.Error != "" {
		t.Fatalf("budget refusal returned as error: %q", out.Error)
	}
	if !strings.Contains(out.Output, "cost budget exhausted") {
		t.Errorf("refusal output missing the budget note: %q", out.Output)
	}

	qa, _, _ := ask.recr.Snapshot()
	if len(qa) != 0 {
		t.Errorf("refused question should not be recorded: %+v", qa)
	}
}

func TestAskToolUnlimitedBudgetRunsVerifier(t *testing.T) {
	// nil budget = unlimited; with valid args the tool must attempt the
	// verifier run (nil verifier → panic proves the call path was reached).
	defer func() {
		if recover() == nil {
			t.Error("expected the verifier run to be attempted (panic on nil verifier)")
		}
	}()
	ask := NewAskTool(nil, nil, &sessionRecorder{}, nil)
	ask.Execute(context.Background(), `{"question":"What is FY2025 ROE?"}`)
}

func TestFallbackReport(t *testing.T) {
	co := &models.Company{Symbol: "RELIANCE", Name: "Reliance Industries", Exchange: "NSE"}
	got := fallbackReport(co, []string{"Revenue grew 8% FY2024 to FY2025.", "", "Guidance raised."})

	for _, want := range []string{
		"# Reliance Industries (RELIANCE) — Equity Research",
		"cost budget ran out before synthesis",
		"Revenue grew 8% FY2024 to FY2025.",
		"Not produced — the cost budget was exhausted",
		"Guidance raised.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("fallback report missing %q:\n%s", want, got)
		}
	}
}

func TestPromptsNonEmpty(t *testing.T) {
	for name, p := range map[string]string{
		"financial":    financialAnalystPrompt,
		"annual":       annualReportPrompt,
		"presentation": presentationPrompt,
		"verifier":     verifierPrompt,
		"synthesizer":  synthesizerPrompt,
	} {
		if strings.TrimSpace(p) == "" {
			t.Errorf("%s prompt is empty", name)
		}
	}
}
