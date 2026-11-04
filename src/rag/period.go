package rag

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Indian filings spell their fiscal period in several shapes: "FY 2025 - 26",
// "FY26", "Q1 FY27", "annualreport_25_26", "quarter ended June 30, 2026".
// DerivePeriod extracts one normalized label — "FY2025-26" or "Q1 FY2026-27" —
// from the anchor text, the document URL and the head of the parsed text, in
// that order. An empty string means the period could not be determined;
// callers must treat unknown as unknown rather than guessing.
//
// Convention note: bare "FYnn"/"FYnnnn" labels follow the Indian convention
// of naming the fiscal year by the March it ENDS in, so FY27 expands to the
// year starting April 2026 ("FY2026-27").
func DerivePeriod(anchor, url, textHead string) string {
	for _, s := range []string{anchor, documentURLText(url), textHead} {
		if p := periodFrom(s); p != "" {
			return p
		}
	}
	return ""
}

// TextHead returns the first n runes of s, for cheap content sniffing.
func TextHead(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

// documentURLText makes a stored URL matchable: percent-escapes decoded and
// underscores turned into spaces, since '_' is a word character and would
// break the quarter/label patterns ("q1_fy27" → "q1 fy27").
func documentURLText(u string) string {
	if u == "" {
		return ""
	}
	if dec, err := url.PathUnescape(u); err == nil {
		u = dec
	}
	return strings.ReplaceAll(u, "_", " ")
}

var (
	// quarterRE has no trailing word boundary: filings write "Q1FY2027"
	// without a separator between the quarter and the fiscal label.
	quarterRE = regexp.MustCompile(`(?i)\b(?:q([1-4])|quarter\s*([1-4]))\b`)
	// quarterFYRE matches the glued label in one token ("Q1FY2027",
	// "q1 fy27", "Q1 F27") where a word boundary cannot precede the FY.
	quarterFYRE    = regexp.MustCompile(`(?i)\b(?:q([1-4])|quarter\s*([1-4]))\s*f(?:y)?\.?\s*(\d{2,4})\b`)
	fyRangeRE      = regexp.MustCompile(`(?i)\bf(?:y)?\.?\s*(\d{2,4})\s*[-–—]?\s*(\d{2,4})\b`)
	yearRangeRE    = regexp.MustCompile(`\b(\d{2,4})\s*[-–—_]?\s*(\d{2,4})\b`)
	fyYearRE       = regexp.MustCompile(`(?i)\bf(?:y)?\.?\s*(\d{2,4})\b`)
	quarterEndedRE = regexp.MustCompile(`(?i)quarter\s+ended\s+(?:on\s+)?([a-z]{3,9})\s+(\d{1,2}),?\s+(\d{4})`)
	yearEndedRE    = regexp.MustCompile(`(?i)\byear\s+ended\s+(?:on\s+)?([a-z]{3,9})\s+(\d{1,2}),?\s+(\d{4})`)
)

// periodFrom returns the first derivable period label in s.
func periodFrom(s string) string {
	if m := quarterFYRE.FindStringSubmatch(s); m != nil {
		if q, end, ok := gluedPeriod(m); ok {
			return fmt.Sprintf("Q%d %s", q, fyPeriod(end-1))
		}
	}
	if q, ok := findQuarter(s); ok {
		if start, ok := findFYStart(s); ok {
			return fmt.Sprintf("Q%d %s", q, fyPeriod(start))
		}
	}
	if start, ok := findFYStart(s); ok {
		return fyPeriod(start)
	}
	if p := endedPeriod(s); p != "" {
		return p
	}
	return ""
}

// gluedPeriod extracts the quarter and ending year from a quarterFYRE match.
func gluedPeriod(m []string) (int, int, bool) {
	qs := m[1]
	if qs == "" {
		qs = m[2]
	}
	q, err := strconv.Atoi(qs)
	if err != nil {
		return 0, 0, false
	}
	end, ok := year(m[3])
	if !ok {
		return 0, 0, false
	}
	return q, end, true
}

// findQuarter returns a quarter number appearing anywhere in s.
func findQuarter(s string) (int, bool) {
	m := quarterRE.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	for _, g := range m[1:] {
		if g != "" {
			q, _ := strconv.Atoi(g)
			return q, true
		}
	}
	return 0, false
}

// findFYStart resolves the April-start year of the fiscal period described
// by s. A "FY2025-26"-style range wins over a bare "FY26" label, and a bare
// label follows the ending-year convention (FY26 → April 2025). The leftmost
// match is not always the valid one ("2026-08" path dates), so every match
// is considered until one passes the range validation.
func findFYStart(s string) (int, bool) {
	for _, m := range fyRangeRE.FindAllStringSubmatch(s, -1) {
		if start, _, ok := fyYears(m[1], m[2]); ok {
			return start, true
		}
	}
	for _, m := range yearRangeRE.FindAllStringSubmatch(s, -1) {
		if start, _, ok := fyYears(m[1], m[2]); ok {
			return start, true
		}
	}
	if m := fyYearRE.FindStringSubmatch(s); m != nil {
		end, ok := year(m[1])
		if !ok {
			return 0, false
		}
		return end - 1, true
	}
	return 0, false
}

// fyYears normalises both halves of a written range ("2025-26", "25_26",
// "2025-2026") to full years and validates that the range spans consecutive
// fiscal years. Non-consecutive ranges (URL path dates like "2026-08") are
// rejected so retrieval metadata never inherits garbage.
func fyYears(a, b string) (int, int, bool) {
	start, ok := year(a)
	if !ok {
		return 0, 0, false
	}
	end, ok := year(b)
	if !ok {
		return 0, 0, false
	}
	if end != start+1 {
		return 0, 0, false
	}
	return start, end, true
}

// year expands a 2- or 4-digit year label to full form; 2-digit labels are
// fiscal years of the 2000s.
func year(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	switch len(s) {
	case 4:
		if n < 1990 || n > 2100 {
			return 0, false
		}
		return n, true
	case 2:
		if n < 20 {
			return 0, false
		}
		return 2000 + n, true
	default:
		return 0, false
	}
}

// fyPeriod renders the fiscal year starting April of startYear.
func fyPeriod(startYear int) string {
	return fmt.Sprintf("FY%d-%02d", startYear, (startYear+1)%100)
}

var months = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

// endedPeriod turns "quarter ended June 30, 2026" / "year ended March 31,
// 2026" statements into period labels using the Indian April–March fiscal
// calendar.
func endedPeriod(s string) string {
	if m := quarterEndedRE.FindStringSubmatch(s); m != nil {
		month, ok := monthOf(m[1])
		if !ok {
			return ""
		}
		y, _ := strconv.Atoi(m[3])
		return fmt.Sprintf("Q%d %s", quarterOf(month), fyPeriod(fyStartYear(month, y)))
	}
	if m := yearEndedRE.FindStringSubmatch(s); m != nil {
		month, ok := monthOf(m[1])
		if !ok {
			return ""
		}
		y, _ := strconv.Atoi(m[3])
		return fyPeriod(fyStartYear(month, y))
	}
	return ""
}

func monthOf(name string) (int, bool) {
	if len(name) > 3 {
		name = name[:3]
	}
	m, ok := months[strings.ToLower(name)]
	return m, ok
}

// quarterOf maps a calendar month to the Indian fiscal quarter: Apr–Jun is
// Q1, Jul–Sep Q2, Oct–Dec Q3, Jan–Mar Q4.
func quarterOf(month int) int {
	return (month+8)%12/3 + 1
}

// fyStartYear returns the April-start year of the fiscal year containing
// the given month.
func fyStartYear(month, year int) int {
	if month >= 4 {
		return year
	}
	return year - 1
}