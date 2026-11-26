package prompts

import (
	"strings"
	"testing"

	"vyasa/src/models"
)

func TestSystemPromptsNonEmpty(t *testing.T) {
	for name, p := range map[string]string{
		"financial":       FinancialAnalyst,
		"annual":          AnnualReportAnalyst,
		"presentation":    PresentationAnalyst,
		"verifier":        Verifier,
		"synthesizer":     Synthesizer,
		"question_writer": QuestionWriter,
		"json_assistant":  JSONAssistant,
	} {
		if strings.TrimSpace(p) == "" {
			t.Errorf("%s prompt is empty", name)
		}
	}
}

func TestSpecialistTask(t *testing.T) {
	co := &models.Company{Name: "Tata Steel", Symbol: "TATASTEEL", Exchange: "NSE"}
	for _, role := range []string{"financial_analyst", "annual_report_analyst", "presentation_analyst", "unknown"} {
		got := SpecialistTask(role, co)
		if !strings.Contains(got, "Tata Steel") || !strings.Contains(got, "TATASTEEL") {
			t.Errorf("task %q missing company context: %s", role, got)
		}
	}
}

func TestSynthesisInput(t *testing.T) {
	co := &models.Company{Name: "Tata Steel", Symbol: "TATASTEEL", Exchange: "NSE"}
	got := SynthesisInput(co, []string{"a", "", "b"})
	for _, want := range []string{"Tata Steel", "### Annual report findings", "output ONLY the markdown report"} {
		if !strings.Contains(got, want) {
			t.Errorf("synthesis input missing %q:\n%s", want, got)
		}
	}
}

func TestBuilders(t *testing.T) {
	if got := QuestionUser("Tata Steel", "TATASTEEL", "annual_report", "FY24", "excerpt text"); !strings.Contains(got, "excerpt text") || !strings.Contains(got, "FY24") {
		t.Errorf("QuestionUser missing fields:\n%s", got)
	}
	if got := IRLinksUser("Tata Steel", "TATASTEEL"); !strings.Contains(got, "Tata Steel") || !strings.Contains(got, "website") {
		t.Errorf("IRLinksUser missing fields:\n%s", got)
	}
	if got := IRClassifyUser("Tata Steel", "TATASTEEL", "\n- annual_report: x", "1. link"); !strings.Contains(got, "1. link") || !strings.Contains(got, "annual_report: x") {
		t.Errorf("IRClassifyUser missing fields:\n%s", got)
	}
}
