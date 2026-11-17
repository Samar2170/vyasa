package research

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"vyasa/src/agent"
	"vyasa/src/client"
	"vyasa/src/client/archivus"
	"vyasa/src/llm"
	"vyasa/src/metrics"
	"vyasa/src/models"
	"vyasa/src/rag"
	"vyasa/src/store"
	"vyasa/src/types"
)

type Deps struct {
	St           *store.Store
	Client       llm.ModelClient
	Indexer      *rag.Indexer
	Storage      client.StorageClient // optional: nil skips the Archivus copy
	Model        string               // LLM model id for every agent
	ParentFolder string               // Archivus root folder (e.g. financial_data)
	StepLimit    int                  // optional per-run step cap (0 = agent defaults)
	Stream       bool                 // stream text deltas through the event sink
	Sink         agent.EventSink      // optional event sink (set Stream to print deltas)
	// CostBudgetUSD caps the aggregate provider-reported dollar cost of the
	// whole workflow (0 = unlimited). When it runs out, in-flight phases
	// finish, remaining verification is skipped and a partial report is
	// still synthesized and persisted.
	CostBudgetUSD float64
}

type Result struct {
	Report   models.ResearchReport
	FilePath string
	Findings []string
	Usage    llm.Usage
	Steps    int
	Finish   agent.FinishStatus

	BudgetSpent     float64
	BudgetLimit     float64 // <= 0 = unlimited
	BudgetExhausted bool
	Warnings        []string
}

func Run(ctx context.Context, d Deps, symbol string) (*Result, error) {
	co, err := d.St.GetCompany(ctx, symbol)
	if err != nil {
		return nil, fmt.Errorf("research: %w", err)
	}
	if co == nil {
		return nil, fmt.Errorf("research: symbol %q not seeded (run: vyasa seed)", symbol)
	}
	facts, err := companyFacts(ctx, d.St, co)
	if err != nil {
		return nil, fmt.Errorf("research: %w", err)
	}

	// The cost budget is enforced at the client layer and shared through
	// the run context: specialists, synthesizer and the ask tool's verifier
	// all refuse further LLM calls once the limit is spent.
	budget := llm.NewCostBudget(d.CostBudgetUSD)
	client := llm.NewCostBudgetClient(d.Client)
	runCtx := llm.WithCostBudget(ctx, budget)

	rec := &sessionRecorder{}
	verifier := newVerifierAgent(d, facts)
	ask := NewAskTool(verifier, client, rec, budget)

	var warnings []string

	specs := []struct {
		name  string
		task  string
		agent *agent.Agent
	}{
		{"financial_analyst", specialistTask("financial_analyst", co), newFinancialAnalyst(d, facts)},
		{"annual_report_analyst", specialistTask("annual_report_analyst", co), newAnnualReportAnalyst(d, facts)},
		{"presentation_analyst", specialistTask("presentation_analyst", co), newPresentationAnalyst(d, facts)},
	}

	findings := make([]string, len(specs))
	perAgent := map[string]any{}
	total := llm.Usage{}
	steps := 0
	var mu sync.Mutex

	var wg sync.WaitGroup
	ferrs := make([]error, len(specs))
	for i, spec := range specs {
		wg.Add(1)
		go func(i int, spec struct {
			name  string
			task  string
			agent *agent.Agent
		}) {
			defer wg.Done()
			res, err := spec.agent.Run(runCtx, client, "", spec.task, runOpts(d)...)
			if err != nil {
				// A specialist stopped by the budget is a warning, not a
				// failure: its partial (or missing) finding is carried on
				// and the report is still synthesized.
				if errors.Is(err, llm.ErrCostBudgetExhausted) {
					mu.Lock()
					warnings = append(warnings, fmt.Sprintf("%s: stopped by cost budget: %v", spec.name, err))
					perAgent[spec.name] = map[string]any{"finish": agent.FinishError, "budget_exhausted": true}
					mu.Unlock()
					return
				}
				ferrs[i] = fmt.Errorf("research: %s: %w", spec.name, err)
				return
			}
			mu.Lock()
			findings[i] = res.FinalText
			perAgent[spec.name] = agentUsage(res)
			total = addUsage(total, res.Usage)
			steps += res.Steps
			mu.Unlock()
		}(i, spec)
	}
	wg.Wait()
	for _, err := range ferrs {
		if err != nil {
			return nil, err
		}
	}

	synth := newSynthesizer(d, ask, facts)
	res, err := synth.Run(runCtx, client, "", synthesisInput(co, findings), runOpts(d)...)
	if err != nil && errors.Is(err, llm.ErrCostBudgetExhausted) {
		// The budget ran out before synthesis could run: persist the raw
		// findings as a partial report instead of losing the spend.
		warnings = append(warnings, fmt.Sprintf("synthesizer: stopped by cost budget: %v", err))
		res = &agent.Result{FinalText: fallbackReport(co, findings), Finish: agent.FinishCompleted}
	} else if err != nil {
		return nil, fmt.Errorf("research: synthesizer: %w", err)
	}
	mu.Lock()
	perAgent["synthesizer"] = agentUsage(res)
	qaAll, vu, vsteps := rec.Snapshot()
	perAgent["research_verifier"] = verifierUsage(vu, vsteps)
	total = addUsage(total, res.Usage)
	total = addUsage(total, vu)
	steps += res.Steps + vsteps
	mu.Unlock()

	now := time.Now()
	spent, limit := budget.Snapshot()
	report := models.ResearchReport{
		CompanyID: co.ID,
		Symbol:    co.Symbol,
		Title:     fmt.Sprintf("%s (%s) — Equity Research — %s", co.Name, co.Symbol, now.Format("2006-01-02")),
		ReportMD:  res.FinalText,
		Questions: qaAll,
		Usage: map[string]any{
			"model":  d.Model,
			"agents": perAgent,
			"cost_budget": map[string]any{
				"limit":     limit,
				"spent":     spent,
				"exhausted": budget.Exhausted(),
			},
		},
	}
	filePath, err := writeMarkdown(outDir(), co.Symbol, res.FinalText, now)
	if err != nil {
		return nil, fmt.Errorf("research: write markdown: %w", err)
	}
	if d.Storage != nil {
		if archived, aerr := archiveMarkdown(ctx, d, co, res.FinalText, filepath.Base(filePath)); aerr != nil {
			// Non-fatal: the local file and the DB row still land.
			perAgent["archive_warning"] = aerr.Error()
		} else {
			report.ArchivedPath = archived
		}
	}
	if err := d.St.InsertResearchReport(ctx, &report); err != nil {
		return nil, fmt.Errorf("research: save report: %w", err)
	}

	return &Result{
		Report:   report,
		FilePath: filePath,
		Findings: findings,
		Usage:    total,
		Steps:    steps,
		Finish:   res.Finish,

		BudgetSpent:     spent,
		BudgetLimit:     limit,
		BudgetExhausted: budget.Exhausted(),
		Warnings:        warnings,
	}, nil
}

func outDir() string { return "reports" }

// runOpts builds the per-run options applied to every top-level run.
func runOpts(d Deps) []agent.RunOption {
	var opts []agent.RunOption
	if d.StepLimit > 0 {
		opts = append(opts, agent.WithStepLimit(d.StepLimit))
	}
	if d.Sink != nil {
		opts = append(opts, agent.WithEvents(d.Sink))
		if d.Stream {
			opts = append(opts, agent.WithStream())
		}
	}
	return opts
}

func newAnnualReportAnalyst(d Deps, facts []string) *agent.Agent {
	return &agent.Agent{
		Name:         "annual_report_analyst",
		Description:  "Reads the company's archived annual report: business, reported statements, MD&A, risks.",
		SystemPrompt: annualReportPrompt,
		Facts:        facts,
		Model:        d.Model,
		Tools:        scopedRAGRegistry(d, segmentAnnual, "annual", "annual report"),
		MaxSteps:     8,
	}
}

func agentUsage(res *agent.Result) map[string]any {
	doc := map[string]any{
		"input_tokens":  res.Usage.InputTokens,
		"output_tokens": res.Usage.OutputTokens,
		"cached_tokens": res.Usage.CachedTokens,
		"steps":         res.Steps,
		"finish":        res.Finish,
	}
	if res.Usage.Cost != nil {
		doc["cost"] = *res.Usage.Cost
	}
	return doc
}

func verifierUsage(u llm.Usage, steps int) map[string]any {
	doc := map[string]any{
		"input_tokens":  u.InputTokens,
		"output_tokens": u.OutputTokens,
		"cached_tokens": u.CachedTokens,
		"steps":         steps,
	}
	if u.Cost != nil {
		doc["cost"] = *u.Cost
	}
	return doc
}

func addUsage(dst, src llm.Usage) llm.Usage {
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	dst.CachedTokens += src.CachedTokens
	if src.Cost != nil {
		base := 0.0
		if dst.Cost != nil {
			base = *dst.Cost
		}
		total := base + *src.Cost
		dst.Cost = &total
	}
	return dst
}

func newFinancialAnalyst(d Deps, facts []string) *agent.Agent {
	reg := types.NewRegistry()
	for _, t := range metrics.NewTools(d.St) {
		reg.Register(t)
	}
	return &agent.Agent{
		Name:         "financial_analyst",
		Description:  "Analyzes computed financial metrics by fiscal year (growth, margins, returns, solvency, cash flow).",
		SystemPrompt: financialAnalystPrompt,
		Facts:        facts,
		Model:        d.Model,
		Tools:        reg,
		MaxSteps:     8,
	}
}

func newPresentationAnalyst(d Deps, facts []string) *agent.Agent {
	return &agent.Agent{
		Name:         "presentation_analyst",
		Description:  "Reads the company's archived investor presentations: headline claims, guidance, capex plans.",
		SystemPrompt: presentationPrompt,
		Facts:        facts,
		Model:        d.Model,
		Tools:        scopedRAGRegistry(d, segmentPresentation, "presentation", "investor presentation"),
		MaxSteps:     8,
	}
}

func newVerifierAgent(d Deps, facts []string) *agent.Agent {
	reg := types.NewRegistry()
	for _, t := range metrics.NewTools(d.St) {
		reg.Register(t)
	}
	reg.Register(rag.NewSearchTool(d.St, d.Indexer))
	reg.Register(rag.NewTableSearchTool(d.St, d.Indexer))
	for _, t := range WebTools() {
		reg.Register(t)
	}
	return &agent.Agent{
		Name:         "research_verifier",
		Description:  "Answers one specific research question using metrics, filings retrieval and the open web.",
		SystemPrompt: verifierPrompt,
		Facts:        facts,
		Model:        d.Model,
		Tools:        reg,
		MaxSteps:     10,
	}
}

func newSynthesizer(d Deps, ask *AskTool, facts []string) *agent.Agent {
	reg := types.NewRegistry()
	reg.Register(ask)
	return &agent.Agent{
		Name:         "synthesizer",
		Description:  "Synthesizes specialist findings into the final equity research report.",
		SystemPrompt: synthesizerPrompt,
		Facts:        facts,
		Model:        d.Model,
		Tools:        reg,
		MaxSteps:     12,
	}

}

// --- prompts & persistence helpers --------------------------------------

func specialistTask(role string, co *models.Company) string {
	base := fmt.Sprintf("Company under review: %s (%s, listed on %s).\n\n", co.Name, co.Symbol, co.Exchange)
	switch role {
	case "financial_analyst":
		return base + "Produce the quantitative findings: run the metrics tools, analyze growth, profitability, returns, solvency, working capital and cash conversion across the available FYs, and flag every data gap you hit."
	case "annual_report_analyst":
		return base + "Produce the annual report findings: map the business, pull the reported numbers from the statement tables, and summarize management's commentary, strategy, capex plans and risks as stated in the report."
	case "presentation_analyst":
		return base + "Produce the investor presentation findings: identify the period covered, extract the headline claims and their supporting numbers, and separate reported results from forward-looking guidance."
	}
	return base
}

// synthesisInput assembles the labeled specialist findings into the
// synthesizer's task.
func synthesisInput(co *models.Company, findings []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Company: %s (%s, %s)\n\n", co.Name, co.Symbol, co.Exchange)
	labels := []string{"Financial analysis (metrics)", "Annual report findings", "Investor presentation findings"}
	for i, f := range findings {
		if i < len(labels) {
			fmt.Fprintf(&b, "---\n### %s\n\n%s\n\n", labels[i], strings.TrimSpace(f))
		}
	}
	b.WriteString("---\n\nSynthesize the findings above into the final equity research report. Reconcile contradictions and fill gaps: if something is unclear, incomplete or disputed, call ask_specialist with one specific question before writing that part. When ready, output ONLY the markdown report.")
	return b.String()
}

// fallbackReport renders the specialist findings as a partial report when
// the cost budget ran out before synthesis could run. Empty findings are
// marked as not produced rather than invented.
func fallbackReport(co *models.Company, findings []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s (%s) — Equity Research\n\n", co.Name, co.Symbol)
	b.WriteString("> Partial report: the cost budget ran out before synthesis could run. ")
	b.WriteString("The specialist findings below are included unedited.\n\n")
	labels := []string{"Financial analysis (metrics)", "Annual report findings", "Investor presentation findings"}
	for i, f := range findings {
		label := "Specialist finding"
		if i < len(labels) {
			label = labels[i]
		}
		fmt.Fprintf(&b, "## %s\n\n", label)
		if text := strings.TrimSpace(f); text != "" {
			b.WriteString(text)
			b.WriteString("\n\n")
		} else {
			b.WriteString("Not produced — the cost budget was exhausted before this specialist finished.\n\n")
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// companyFacts renders the shared context every agent's system prompt gets.
func companyFacts(ctx context.Context, st *store.Store, co *models.Company) ([]string, error) {
	facts := []string{
		fmt.Sprintf("Company: %s (%s on %s)", co.Name, co.Symbol, co.Exchange),
	}
	if co.Industry != "" {
		facts = append(facts, "Industry: "+co.Industry)
	}
	links, err := st.CompanyLinks(ctx, co.ID)
	if err != nil {
		return nil, err
	}
	if links != nil {
		if links.OfficialWebsite != "" {
			facts = append(facts, "Official website: "+links.OfficialWebsite)
		}
		if links.InvestorRelationsLink != "" {
			facts = append(facts, "Investor relations page: "+links.InvestorRelationsLink)
		}
	}
	rows, err := st.GetMetrics(ctx, co.Symbol)
	if err != nil {
		return nil, err
	}
	if len(rows) > 0 {
		fys := make([]string, 0, len(rows))
		for _, r := range rows {
			fys = append(fys, r.FY)
		}
		facts = append(facts, "Financial metrics available for: "+strings.Join(fys, ", "))
	} else {
		facts = append(facts, "No precomputed metrics are stored for this company; the metrics tools will return errors.")
	}
	return facts, nil
}

// writeMarkdown saves the report locally as reports/<SYMBOL>_<timestamp>.md.
func writeMarkdown(dir, symbol, md string, now time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, fmt.Sprintf("%s_%s.md", symbol, now.Format("20060102_150405")))
	if err := os.WriteFile(p, []byte(md), 0o644); err != nil {
		return "", err
	}
	return p, nil
}

// archiveMarkdown uploads the report to Archivus under
// <parent>/research_reports/<SYMBOL>/ and records it in saved_files.
func archiveMarkdown(ctx context.Context, d Deps, co *models.Company, md, name string) (string, error) {
	arch, ok := d.Storage.(*archivus.ArchivusClient)
	if !ok {
		return "", nil // storage client without file operations: skip silently
	}
	folder := path.Join(d.ParentFolder, "research_reports", co.Symbol)
	resp, err := arch.UploadFile(folder, []*archivus.UploadFile{{Name: name, Content: []byte(md)}})
	if err != nil {
		return "", fmt.Errorf("archivus upload: %w", err)
	}
	if err := client.SaveUploadedPaths(ctx, d.St, *co, resp); err != nil {
		// Recorded or not, the upload itself landed; keep going.
		_ = err
	}
	stored := folder + "/" + name
	if len(resp) > 0 && !resp[0].IsError && resp[0].FolderPath != "" {
		stored = path.Join(resp[0].FolderPath, resp[0].Name)
	}
	return stored, nil
}
