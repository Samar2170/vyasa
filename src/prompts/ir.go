package prompts

import "fmt"

// JSONAssistant is the shared system prompt for the small JSON-assist calls
// (link discovery, link classification).
const JSONAssistant = "You are a precise research assistant. Reply with strict JSON only, no commentary."

// IRLinksUser asks the model for a company's official website and IR page.
func IRLinksUser(name, symbol string) string {
	return fmt.Sprintf(`What are the official website and the investor relations page of the Indian listed company %q (BSE symbol %s)?
Reply with JSON: {"website": "https://... or null", "investor_relations": "https://... or null"}
Only give URLs you are confident about. The investor relations page is the section hosting annual reports, investor presentations and quarterly results.`, name, symbol)
}

// IRClassifyUser asks the model to pick the best link index per filing
// segment. segDesc and links are pre-rendered by the caller.
func IRClassifyUser(name, symbol, segDesc, links string) string {
	return fmt.Sprintf(`Company %q (BSE symbol %s). Below are links from its investor relations pages. Pick the index (the number before the dot) of the best link for each of these segments:%s

Links:
%s

Reply with JSON mapping each segment to the chosen index or null, e.g. {"annual_report": 3, "investor_presentation": null, "earnings_call_transcript": null, "quarterly_financials": 7}. Only use indexes from the list. Prefer the most recent document.`, name, symbol, segDesc, links)
}
