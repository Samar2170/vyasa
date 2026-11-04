package rag

import "testing"

func TestDerivePeriod(t *testing.T) {
	cases := []struct {
		name       string
		anchor     string
		url        string
		textHead   string
		want       string
	}{
		// real anchor/URL pairs from company_research_resources
		{"waaree annual anchor", "Annual Report FY 2025 - 26", "https://www.waaree.com/upload/media/annualreport_25_26_1787813718.pdf", "", "FY2025-26"},
		{"waaree transcript anchor", "Q1 Transcript of Call 2026-27", "https://www.waaree.com/upload/media/waaree_energies_ltd_q1_fy27_earnings_call_transcript_1786015347.pdf", "", "Q1 FY2026-27"},
		{"waaree presentation anchor", "Q2 Investor Presentation 2026-27", "https://www.waaree.com/upload/media/investorpresentation_signed_1793181437.pdf", "", "Q2 FY2026-27"},
		{"jsl presentation anchor", "Earnings Presentation Q1 FY27", "https://www.jindalstainless.com/wp-content/uploads/2026/08/JSL-Q1-FY27-Earnings-Presentation.pdf", "", "Q1 FY2026-27"},
		{"jsl quarterly anchor", "Unaudited financial results for the quarter ended June 30, 2026", "https://www.jindalstainless.com/wp-content/uploads/2026/08/Financials.pdf", "", "Q1 FY2026-27"},
		{"ril transcript url only", "", "https://rilstaticasset.akamaized.net/sites/default/files/2026-07/Transcript-Q1FY2027.pdf", "", "Q1 FY2026-27"},
		{"iff quarterly anchor", "Integrated Filing (Financial) for the quarter ended June 30, 2026", "https://rilstaticasset.akamaized.net/sites/default/files/2026-07/SE_IFF.pdf", "", "Q1 FY2026-27"},
		{"mm annual url only", "Download", "https://www.mahindra.com/sites/default/files/annual-report-FY26/MM-Annual-Report-2025-26.pdf", "", "FY2025-26"},
		{"mm transcript url only", "Download", "https://www.mahindra.com/sites/default/files/2026-08/M%26M%20Q1%20F27%20Analyst%20Meet%20-%20Transcript.pdf", "", "Q1 FY2026-27"},
		{"ril annual url only", "", "https://www.ril.com/ar2025-26/RIL_IAR%202026.pdf", "Integrated Annual Report 2025-26", "FY2025-26"},

		// derived shapes
		{"two digit fy expands to ending year", "", "", "Results for Q1 FY27", "Q1 FY2026-27"},
		{"year ended march", "", "", "for the year ended March 31, 2026", "FY2025-26"},
		{"quarter ended with on", "", "", "quarter ended on December 31, 2025", "Q3 FY2025-26"},
		{"underscored url range", "", "https://example.com/ar_fy25_26.pdf", "", "FY2025-26"},
		{"four digit range end", "", "", "Annual Report 2025-2026", "FY2025-26"},

		// guards: better empty than wrong
		{"url path month date rejected", "", "https://example.com/uploads/2026-08/doc.pdf", "", ""},
		{"calendar year alone rejected", "", "https://example.com/RIL_IAR_2026.pdf", "", ""},
		{"bare quarter rejected", "Q2 Investor Presentation", "https://example.com/pres.pdf", "", ""},
		{"non consecutive range rejected", "", "", "targets 2024-30 outlined", ""},
		{"nothing derivable", "", "https://example.com/print/pdf/node/7857", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DerivePeriod(tc.anchor, tc.url, TextHead(tc.textHead, 4000))
			if got != tc.want {
				t.Errorf("DerivePeriod(%q, %q, %q) = %q, want %q", tc.anchor, tc.url, tc.textHead, got, tc.want)
			}
		})
	}
}

func TestTextHead(t *testing.T) {
	if got := TextHead("abcde", 3); got != "abc" {
		t.Errorf("TextHead short = %q", got)
	}
	if got := TextHead("ab", 10); got != "ab" {
		t.Errorf("TextHead long n = %q", got)
	}
}
