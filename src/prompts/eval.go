package prompts

import "fmt"

// QuestionWriter instructs the model to turn one chunk into one
// self-contained retrieval question plus its answer.
const QuestionWriter = `You write evaluation questions for a financial-filings retrieval benchmark.
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

// QuestionUser is the user message for the golden-dataset question writer.
// excerpt must already be trimmed to the desired length.
func QuestionUser(name, symbol, segment, period, excerpt string) string {
	return fmt.Sprintf("Company: %s (symbol %s)\nSegment: %s\nPeriod: %s\nExcerpt:\n<<<\n%s\n>>>",
		name, symbol, segment, period, excerpt)
}
