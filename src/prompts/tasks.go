package prompts

import (
	"fmt"
	"strings"

	"vyasa/src/models"
)

// FindingLabels labels the three specialist findings, in pipeline order.
// Used by SynthesisInput and by the fallback report renderer.
var FindingLabels = []string{
	"Financial analysis (metrics)",
	"Annual report findings",
	"Investor presentation findings",
}

// SpecialistTask is the user message that kicks off one specialist agent:
// shared company header plus the role's task instruction.
func SpecialistTask(role string, co *models.Company) string {
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

// SynthesisInput assembles the labeled specialist findings into the
// synthesizer's user message.
func SynthesisInput(co *models.Company, findings []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Company: %s (%s, %s)\n\n", co.Name, co.Symbol, co.Exchange)
	for i, f := range findings {
		if i < len(FindingLabels) {
			fmt.Fprintf(&b, "---\n### %s\n\n%s\n\n", FindingLabels[i], strings.TrimSpace(f))
		}
	}
	b.WriteString("---\n\nSynthesize the findings above into the final equity research report. Reconcile contradictions and fill gaps: if something is unclear, incomplete or disputed, call ask_specialist with one specific question before writing that part. When ready, output ONLY the markdown report.")
	return b.String()
}
