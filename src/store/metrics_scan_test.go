package store

import (
	"testing"

	"vyasa/src/models"
)

// TestMetricFieldPtrsAlignment guards the GetMetrics scan order: the model
// field pointers must line up one-to-one with models.TickerMetricColumns,
// otherwise nullable values would silently scan into the wrong column.
func TestMetricFieldPtrsAlignment(t *testing.T) {
	m := &models.TickerMetrics{}
	fields := metricFieldPtrs(m)
	if len(fields) != len(models.TickerMetricColumns) {
		t.Fatalf("metricFieldPtrs has %d fields, TickerMetricColumns has %d",
			len(fields), len(models.TickerMetricColumns))
	}

	// Every destination must be a distinct non-nil double pointer.
	seen := make(map[any]bool, len(fields))
	for i, f := range fields {
		if f == nil {
			t.Fatalf("field %d (%s) is nil", i, models.TickerMetricColumns[i])
		}
		if seen[f] {
			t.Fatalf("field %d (%s) duplicates another destination", i, models.TickerMetricColumns[i])
		}
		seen[f] = true
	}

	// Spot-check positions against the struct layout.
	checks := map[string]any{
		"total_revenue":     &m.TotalRevenue,
		"roe":               &m.ROE,
		"dso_days":          &m.DSODays,
		"cfo_to_ebitda":     &m.CFOToEBITDA,
		"eps_cagr_5y":       &m.EPSCAGR5Y,
		"payables_pct_cogs": &m.PayablesPctCOGS,
	}
	for col, want := range checks {
		idx := indexOfColumn(col)
		if idx < 0 {
			t.Fatalf("column %s not in TickerMetricColumns", col)
		}
		if fields[idx] != want {
			t.Fatalf("column %s at index %d does not point at the matching model field", col, idx)
		}
	}
}

func indexOfColumn(col string) int {
	for i, c := range models.TickerMetricColumns {
		if c == col {
			return i
		}
	}
	return -1
}
