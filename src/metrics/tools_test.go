package metrics

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"vyasa/src/models"
)

func TestNormalizeFY(t *testing.T) {
	cases := map[string]string{
		"FY2025":   "FY2025",
		"fy2025":   "FY2025",
		"FY 2025":  "FY2025",
		"fy 2025":  "FY2025",
		"2025":     "FY2025",
		"  2025  ": "FY2025",
		"FY25":     "",
		"2025-26":  "",
		"latest":   "",
		"":         "",
	}
	for in, want := range cases {
		if got := normalizeFY(in); got != want {
			t.Errorf("normalizeFY(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveMetricNames(t *testing.T) {
	// "all" expands to every metric column in order.
	names, unknown := resolveMetricNames([]string{"ALL"})
	if len(unknown) != 0 {
		t.Fatalf("unknown = %v", unknown)
	}
	if len(names) != len(models.TickerMetricColumns) {
		t.Fatalf("all expanded to %d names, want %d", len(names), len(models.TickerMetricColumns))
	}
	for i := range names {
		if names[i] != models.TickerMetricColumns[i] {
			t.Fatalf("order mismatch at %d: %s != %s", i, names[i], models.TickerMetricColumns[i])
		}
	}

	// Explicit lists are validated, deduped and case-normalized.
	names, unknown = resolveMetricNames([]string{"ROE", " roe ", "net_margin", "bogus"})
	if len(unknown) != 1 || unknown[0] != "bogus" {
		t.Fatalf("unknown = %v, want [bogus]", unknown)
	}
	if len(names) != 2 || names[0] != "roe" || names[1] != "net_margin" {
		t.Fatalf("names = %v, want [roe net_margin]", names)
	}

	// Empty input resolves to nothing (caller rejects).
	names, unknown = resolveMetricNames(nil)
	if len(names) != 0 || len(unknown) != 0 {
		t.Fatalf("names = %v, unknown = %v", names, unknown)
	}
}

func fixtureRows() []models.TickerMetrics {
	f := func(v float64) *float64 { return &v }
	end := func(y int) time.Time { return time.Date(y, 3, 31, 0, 0, 0, 0, time.UTC) }
	return []models.TickerMetrics{
		{Symbol: "TEST", FY: "FY2023", EndDate: end(2023), RevenueYoY: f(0.1), ROE: f(0.15),
			DataGaps: []string{"gross_profit: missing cost_of_revenue"}},
		{Symbol: "TEST", FY: "FY2024", EndDate: end(2024), RevenueYoY: f(0.2), ROE: f(0.16)},
		{Symbol: "TEST", FY: "FY2025", EndDate: end(2025), RevenueYoY: f(0.3), ROE: nil},
	}
}

func TestFilterYears(t *testing.T) {
	rows := fixtureRows()

	// Empty filter keeps everything.
	kept, unmatched := filterYears(rows, nil)
	if len(kept) != 3 || unmatched != nil {
		t.Fatalf("kept = %d, unmatched = %v", len(kept), unmatched)
	}

	// Mixed label formats; "latest" is not a series filter and is reported.
	kept, unmatched = filterYears(rows, []string{"fy 2023", "2025", "latest"})
	if len(kept) != 2 || kept[0].FY != "FY2023" || kept[1].FY != "FY2025" {
		t.Fatalf("kept = %v", keptFYs(kept))
	}
	if len(unmatched) != 1 || unmatched[0] != "latest" {
		t.Fatalf("unmatched = %v", unmatched)
	}

	// An FY that was never stored yields nothing.
	kept, unmatched = filterYears(rows, []string{"2019"})
	if len(kept) != 0 || len(unmatched) != 0 {
		t.Fatalf("kept = %v, unmatched = %v", keptFYs(kept), unmatched)
	}
}

func keptFYs(rows []models.TickerMetrics) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.FY
	}
	return out
}

func TestNewSeriesRow(t *testing.T) {
	rows := fixtureRows()
	row := newSeriesRow(rows[2], []string{"revenue_yoy", "roe"}) // roe is nil in FY2025

	if row.FY != "FY2025" || row.EndDate != "2025-03-31" {
		t.Fatalf("row = %+v", row)
	}
	if v := row.Values["revenue_yoy"]; v == nil || v.(float64) != 0.3 {
		t.Fatalf("revenue_yoy = %v", v)
	}
	if v, ok := row.Values["roe"]; !ok || v != nil {
		t.Fatalf("roe = %v, ok = %v (want explicit null)", v, ok)
	}

	// Marshals as expected.
	buf, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(buf, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["values"].(map[string]any); !ok {
		t.Fatalf("values key missing: %s", buf)
	}
}

func TestRowJSONShape(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	m := models.TickerMetrics{Symbol: "TEST", FY: "FY2024", ROE: f(0.1), NetMargin: f(0.2)}
	doc := rowJSON(m)

	// Column names are the JSON keys; company metadata keys are separate.
	if _, ok := doc["roe"]; !ok {
		t.Fatal("roe missing from rowJSON")
	}
	if _, ok := doc["net_margin"]; !ok {
		t.Fatal("net_margin missing from rowJSON")
	}
	if _, ok := doc["symbol"]; !ok {
		t.Fatal("symbol missing from rowJSON")
	}
	// Absent metrics are not keys (the tool layer emits explicit nulls).
	if _, ok := doc["revenue_yoy"]; ok {
		t.Fatal("nil metric unexpectedly present in rowJSON")
	}
}

func TestPickFY(t *testing.T) {
	rows := fixtureRows()

	got, err := pickFY(rows, "latest")
	if err != nil || got.FY != "FY2025" {
		t.Fatalf("latest = %v, %v", got.FY, err)
	}
	got, err = pickFY(rows, "fy 2024")
	if err != nil || got.FY != "FY2024" {
		t.Fatalf("fy 2024 = %v, %v", got.FY, err)
	}
	if _, err = pickFY(rows, "2019"); err == nil || !strings.Contains(err.Error(), "no metrics for FY2019") {
		t.Fatalf("err = %v", err)
	}
	if _, err = pickFY(rows, "junk"); err == nil || !strings.Contains(err.Error(), "invalid fy") {
		t.Fatalf("err = %v", err)
	}
}
