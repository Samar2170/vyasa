package metrics

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"vyasa/src/models"
)

// stmtRow builds one statement row with the tickertape metadata keys plus
// the given values.
func stmtRow(period, end string, vals map[string]any) map[string]any {
	row := map[string]any{
		"displayPeriod": period,
		"endDate":       end,
		"reporting":     "consolidated",
	}
	for k, v := range vals {
		row[k] = v
	}
	return row
}

// statementJSON encodes rows into a statement snapshot payload.
func statementJSON(t *testing.T, rows ...map[string]any) json.RawMessage {
	t.Helper()
	buf, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("encode statement: %v", err)
	}
	return buf
}

func fyLabel(fy int) string { return fmt.Sprintf("FY %d", fy) }
func endDate(fy int) string { return fmt.Sprintf("%d-03-31T00:00:00.000Z", fy) }

// fixture builds a 6-FY (2020..2025) company with clean 10% revenue growth,
// 20% ebitda / 15% ebit margins and a 25% effective tax rate. Cost of
// revenue is present only in FY2022 (derived gross profit) and FY2024
// (stated gross profit), exercising both gross-profit paths and the gaps.
func fixture(t *testing.T) (income, balance, cashflow json.RawMessage) {
	t.Helper()
	revenue := []float64{100, 110, 121, 133.1, 146.41, 161.05}
	cogs := map[int]float64{2022: 60, 2024: 56.41}
	gross := map[int]float64{2024: 90}

	var inc, bal, cf []map[string]any
	for _, fy := range []int{2020, 2021, 2022, 2023, 2024, 2025} {
		rev := revenue[fy-2020]
		ebitda := 0.2 * rev
		ebit := 0.15 * rev
		interest := 2.0
		pbt := ebit - interest
		taxes := 0.25 * pbt
		ni := pbt - taxes
		row := map[string]any{
			srcRevenue:   rev,
			srcEBITDA:    ebitda,
			srcEBIT:      ebit,
			srcInterest:  interest,
			srcPBT:       pbt,
			srcTaxes:     taxes,
			srcNetIncome: ni,
			srcEPS:       ni / 10,
		}
		if v, ok := cogs[fy]; ok {
			row[srcCOGS] = v
		}
		if v, ok := gross[fy]; ok {
			row[srcGrossProfit] = v
		}
		inc = append(inc, stmtRow(fyLabel(fy), endDate(fy), row))

		bal = append(bal, stmtRow(fyLabel(fy), endDate(fy), map[string]any{
			srcEquity:        200.0,
			srcMinority:      50.0,
			srcTotalDebt:     80.0,
			srcCashSTI:       30.0,
			srcTotalAssets:   400.0,
			srcCurrentAssets: 150.0,
			srcCurrentLiab:   100.0,
			srcReceivables:   20.0,
			srcInventory:     10.0,
			srcPayables:      5.0,
			srcSharesOut:     10.0,
		}))

		cf = append(cf, stmtRow(fyLabel(fy), endDate(fy), map[string]any{
			srcCFO:   30.0,
			srcCapex: 10.0,
			srcFCF:   20.0,
		}))
	}
	// A TTM row must be ignored.
	inc = append(inc, stmtRow("TTM", endDate(2026), map[string]any{srcRevenue: 999.0}))

	return statementJSON(t, inc...), statementJSON(t, bal...), statementJSON(t, cf...)
}

// almost reports whether a is within delta of b.
func almost(a, b, delta float64) bool { return math.Abs(a-b) <= delta }

// want checks a metric value against an expected float.
func want(t *testing.T, m models.TickerMetrics, name string, get func() *float64, expect float64) {
	t.Helper()
	v := get()
	if v == nil {
		t.Errorf("%s %s: %s = nil, want %v (gaps: %v)", m.Symbol, m.FY, name, expect, m.DataGaps)
		return
	}
	if !almost(*v, expect, 1e-9) {
		t.Errorf("%s %s: %s = %v, want %v", m.Symbol, m.FY, name, *v, expect)
	}
}

// wantNil checks that a metric is absent.
func wantNil(t *testing.T, m models.TickerMetrics, name string, get func() *float64) {
	t.Helper()
	if v := get(); v != nil {
		t.Errorf("%s %s: %s = %v, want nil (gaps: %v)", m.Symbol, m.FY, name, *v, m.DataGaps)
	}
}

// hasGap reports whether the row records a gap containing substr.
func hasGap(m models.TickerMetrics, substr string) bool {
	for _, g := range m.DataGaps {
		if strings.Contains(g, substr) {
			return true
		}
	}
	return false
}

func computeFixture(t *testing.T) []models.TickerMetrics {
	t.Helper()
	income, balance, cashflow := fixture(t)
	rows, err := ComputeStatements(models.Company{BaseModel: models.BaseModel{ID: "co1"}, Symbol: "TEST"}, income, balance, cashflow)
	if err != nil {
		t.Fatalf("ComputeStatements: %v", err)
	}
	if len(rows) != 6 {
		t.Fatalf("got %d rows, want 6", len(rows))
	}
	for i, m := range rows {
		if expect := fmt.Sprintf("FY%d", 2020+i); m.FY != expect {
			t.Errorf("row %d: fy = %q, want %q", i, m.FY, expect)
		}
	}
	return rows
}

func TestRawCopiesAndMargins(t *testing.T) {
	rows := computeFixture(t)
	fy22 := rows[2] // FY2022

	want(t, fy22, "total_revenue", func() *float64 { return fy22.TotalRevenue }, 121)
	want(t, fy22, "ebitda", func() *float64 { return fy22.EBITDA }, 24.2)
	want(t, fy22, "ebit", func() *float64 { return fy22.EBIT }, 18.15)
	want(t, fy22, "pbt", func() *float64 { return fy22.PBT }, 16.15)
	want(t, fy22, "net_income", func() *float64 { return fy22.NetIncome }, 12.1125)
	want(t, fy22, "eps", func() *float64 { return fy22.EPS }, 1.21125)
	want(t, fy22, "cfo", func() *float64 { return fy22.CFO }, 30)
	want(t, fy22, "capex", func() *float64 { return fy22.Capex }, 10)
	want(t, fy22, "fcf", func() *float64 { return fy22.FCF }, 20)
	want(t, fy22, "total_equity", func() *float64 { return fy22.TotalEquity }, 200)
	want(t, fy22, "minority_interest", func() *float64 { return fy22.MinorityInterest }, 50)
	want(t, fy22, "net_worth", func() *float64 { return fy22.NetWorth }, 150)
	want(t, fy22, "total_debt", func() *float64 { return fy22.TotalDebt }, 80)
	want(t, fy22, "cash_and_sti", func() *float64 { return fy22.CashAndSTI }, 30)
	want(t, fy22, "shares_outstanding", func() *float64 { return fy22.SharesOutstanding }, 10)

	want(t, fy22, "gross_profit", func() *float64 { return fy22.GrossProfit }, 61) // 121 - 60
	want(t, fy22, "ebitda_margin", func() *float64 { return fy22.EBITDAMargin }, 0.2)
	want(t, fy22, "ebit_margin", func() *float64 { return fy22.EBITMargin }, 0.15)
	want(t, fy22, "net_margin", func() *float64 { return fy22.NetMargin }, 12.1125/121)

	want(t, fy22, "net_debt", func() *float64 { return fy22.NetDebt }, 50)
	want(t, fy22, "net_debt_to_ebitda", func() *float64 { return fy22.NetDebtToEBITDA }, 50/24.2)
	want(t, fy22, "interest_coverage", func() *float64 { return fy22.InterestCoverage }, 18.15/2)
	want(t, fy22, "debt_to_equity", func() *float64 { return fy22.DebtToEquity }, 0.4)
	want(t, fy22, "current_ratio", func() *float64 { return fy22.CurrentRatio }, 1.5)

	want(t, fy22, "dso_days", func() *float64 { return fy22.DSODays }, 20.0*365.0/121.0)
	want(t, fy22, "dio_days", func() *float64 { return fy22.DIODays }, 10.0*365.0/60.0)
	want(t, fy22, "dpo_days", func() *float64 { return fy22.DPODays }, 5.0*365.0/60.0)
	want(t, fy22, "wc_cycle_days", func() *float64 { return fy22.WCCycleDays },
		20.0*365.0/121.0+10.0*365.0/60.0-5.0*365.0/60.0)

	want(t, fy22, "fcf_margin", func() *float64 { return fy22.FCFMargin }, 20.0/121.0)
	want(t, fy22, "cfo_to_pat", func() *float64 { return fy22.CFOToPAT }, 30/12.1125)
	want(t, fy22, "cfo_to_ebitda", func() *float64 { return fy22.CFOToEBITDA }, 30/24.2)

	// TTM row must not appear as an FY.
	for _, m := range rows {
		if m.FY == "FYTTM" || m.FY == "TTM" {
			t.Errorf("TTM row leaked into output: %s", m.FY)
		}
	}
}

func TestGrowthAndCAGR(t *testing.T) {
	rows := computeFixture(t)
	fy23 := rows[3] // FY2023

	want(t, fy23, "revenue_yoy", func() *float64 { return fy23.RevenueYoY }, 0.1)
	want(t, fy23, "ebitda_yoy", func() *float64 { return fy23.EBITDAYoY }, 0.1)
	want(t, fy23, "ebit_yoy", func() *float64 { return fy23.EBITYoY }, 0.1)
	want(t, fy23, "pat_yoy", func() *float64 { return fy23.PATYoY }, (13.47375-12.1125)/12.1125)
	want(t, fy23, "eps_yoy", func() *float64 { return fy23.EPSYoY }, (13.47375-12.1125)/12.1125)

	want(t, fy23, "revenue_cagr_3y", func() *float64 { return fy23.RevenueCAGR3Y }, 0.1)
	want(t, fy23, "pat_cagr_3y", func() *float64 { return fy23.PATCAGR3Y }, math.Pow(13.47375/9.75, 1.0/3.0)-1)

	// CAGR5 is only defined from FY2025 (needs FY2020).
	wantNil(t, fy23, "revenue_cagr_5y", func() *float64 { return fy23.RevenueCAGR5Y })
	fy25 := rows[5]
	want(t, fy25, "revenue_cagr_5y", func() *float64 { return fy25.RevenueCAGR5Y }, math.Pow(161.05/100.0, 1.0/5.0)-1)

	// The oldest year has no growth metrics.
	fy20 := rows[0]
	wantNil(t, fy20, "revenue_yoy", func() *float64 { return fy20.RevenueYoY })
	wantNil(t, fy20, "eps_cagr_3y", func() *float64 { return fy20.EPSCAGR3Y })
}

func TestDOL(t *testing.T) {
	rows := computeFixture(t)
	fy21 := rows[1]
	// Flat margins: profit moves exactly with revenue.
	want(t, fy21, "dol_ebit", func() *float64 { return fy21.DOLEBIT }, 1.0)
	want(t, fy21, "dol_ebitda", func() *float64 { return fy21.DOLEBITDA }, 1.0)
}

func TestReturnsAveragesAndFallback(t *testing.T) {
	rows := computeFixture(t)

	// Oldest year: year-end balances (average falls back to the single value).
	fy20 := rows[0]
	want(t, fy20, "roe", func() *float64 { return fy20.ROE }, 9.75/200)
	want(t, fy20, "ronw", func() *float64 { return fy20.RONW }, 9.75/150)
	want(t, fy20, "roce", func() *float64 { return fy20.ROCE }, 15.0/300.0)
	want(t, fy20, "roic", func() *float64 { return fy20.ROIC }, 15*0.75/230)

	// Later years: constant balances, so the average equals the year-end value.
	fy22 := rows[2]
	want(t, fy22, "roe", func() *float64 { return fy22.ROE }, 12.1125/200)
	want(t, fy22, "roic", func() *float64 { return fy22.ROIC }, 18.15*0.75/230)
}

func TestGrossProfitAndCOGSGaps(t *testing.T) {
	rows := computeFixture(t)

	// FY2021 has no cost of revenue: gross profit and COGS-based metrics gap.
	fy21 := rows[1]
	wantNil(t, fy21, "gross_profit", func() *float64 { return fy21.GrossProfit })
	wantNil(t, fy21, "gross_margin", func() *float64 { return fy21.GrossMargin })
	wantNil(t, fy21, "dio_days", func() *float64 { return fy21.DIODays })
	wantNil(t, fy21, "dpo_days", func() *float64 { return fy21.DPODays })
	wantNil(t, fy21, "payables_pct_cogs", func() *float64 { return fy21.PayablesPctCOGS })
	wantNil(t, fy21, "wc_cycle_days", func() *float64 { return fy21.WCCycleDays })
	if !hasGap(fy21, "gross_profit: missing cost_of_revenue") {
		t.Errorf("FY2021 gaps missing gross_profit entry: %v", fy21.DataGaps)
	}

	// FY2024 uses the stated Gross Profit rather than revenue - COGS.
	fy24 := rows[4]
	want(t, fy24, "gross_profit", func() *float64 { return fy24.GrossProfit }, 90)
}

func TestMissingStatementsRecordGaps(t *testing.T) {
	income, _, _ := fixture(t)
	rows, err := ComputeStatements(models.Company{Symbol: "TEST"}, income, nil, nil)
	if err != nil {
		t.Fatalf("ComputeStatements: %v", err)
	}
	if len(rows) != 6 {
		t.Fatalf("got %d rows, want 6", len(rows))
	}
	for _, m := range rows {
		if !hasGap(m, "balancesheet: no data") || !hasGap(m, "cashflow: no data") {
			t.Errorf("%s: missing statement gaps not recorded: %v", m.FY, m.DataGaps)
		}
		if m.TotalEquity != nil || m.CFO != nil {
			t.Errorf("%s: balance/cashflow values leaked despite missing statements", m.FY)
		}
		if m.ROE != nil {
			t.Errorf("%s: roe computed without equity", m.FY)
		}
	}
}

func TestZeroDenominatorGaps(t *testing.T) {
	income := statementJSON(t,
		stmtRow(fyLabel(2023), endDate(2023), map[string]any{
			srcRevenue:   100.0,
			srcEBITDA:    10.0,
			srcEBIT:      8.0,
			srcInterest:  0.0, // zero interest: coverage undefined
			srcPBT:       6.0,
			srcTaxes:     1.5,
			srcNetIncome: 4.5,
			srcEPS:       0.45,
		}),
	)
	balance := statementJSON(t,
		stmtRow(fyLabel(2023), endDate(2023), map[string]any{
			srcEquity:        0.0, // zero equity: debt_to_equity, roe undefined
			srcTotalDebt:     10.0,
			srcCurrentAssets: 10.0,
			srcCurrentLiab:   0.0, // zero CL: current ratio undefined
			srcTotalAssets:   50.0,
		}),
	)
	rows, err := ComputeStatements(models.Company{Symbol: "TEST"}, income, balance, nil)
	if err != nil {
		t.Fatalf("ComputeStatements: %v", err)
	}
	m := rows[0]
	wantNil(t, m, "interest_coverage", func() *float64 { return m.InterestCoverage })
	wantNil(t, m, "debt_to_equity", func() *float64 { return m.DebtToEquity })
	wantNil(t, m, "roe", func() *float64 { return m.ROE })
	wantNil(t, m, "current_ratio", func() *float64 { return m.CurrentRatio })
	wantNil(t, m, "net_worth", func() *float64 { return m.NetWorth }) // minority missing
	if !hasGap(m, "interest_coverage: undefined") {
		t.Errorf("gaps: %v", m.DataGaps)
	}
	if !hasGap(m, "cashflow: no data") {
		t.Errorf("gaps: %v", m.DataGaps)
	}
}

func TestNullValuesAreMissing(t *testing.T) {
	// Tickertape emits explicit JSON nulls for unreported fields (e.g. M&M's
	// cost of revenue); those must be treated as missing, not as zero.
	income := statementJSON(t,
		stmtRow(fyLabel(2023), endDate(2023), map[string]any{
			srcRevenue:     100.0,
			srcCOGS:        nil, // explicit JSON null
			srcGrossProfit: nil,
			srcEBITDA:      20.0,
			srcEBIT:        15.0,
			srcNetIncome:   10.0,
		}),
	)
	rows, err := ComputeStatements(models.Company{Symbol: "TEST"}, income, nil, nil)
	if err != nil {
		t.Fatalf("ComputeStatements: %v", err)
	}
	m := rows[0]
	wantNil(t, m, "gross_profit", func() *float64 { return m.GrossProfit })
	wantNil(t, m, "gross_margin", func() *float64 { return m.GrossMargin })
	wantNil(t, m, "dio_days", func() *float64 { return m.DIODays })
	if !hasGap(m, "gross_profit: missing cost_of_revenue") {
		t.Errorf("gaps: %v", m.DataGaps)
	}
}

func TestMissingPriorYearBalance(t *testing.T) {
	// Balance sheet starts one year later than the income statement: FY2021
	// gets no returns, FY2022 falls back to the year-end balance.
	income := statementJSON(t,
		stmtRow(fyLabel(2021), endDate(2021), map[string]any{
			srcRevenue:   100.0,
			srcEBIT:      20.0,
			srcPBT:       18.0,
			srcTaxes:     4.5,
			srcNetIncome: 13.5,
		}),
		stmtRow(fyLabel(2022), endDate(2022), map[string]any{
			srcRevenue:   120.0,
			srcEBIT:      24.0,
			srcPBT:       21.0,
			srcTaxes:     5.25,
			srcNetIncome: 15.75,
		}),
	)
	balance := statementJSON(t,
		stmtRow(fyLabel(2022), endDate(2022), map[string]any{
			srcEquity:    100.0,
			srcMinority:  0.0,
			srcTotalDebt: 50.0,
		}),
	)
	rows, err := ComputeStatements(models.Company{Symbol: "TEST"}, income, balance, nil)
	if err != nil {
		t.Fatalf("ComputeStatements: %v", err)
	}
	fy21 := rows[0]
	wantNil(t, fy21, "roe", func() *float64 { return fy21.ROE }) // no equity data at all
	fy22 := rows[1]
	want(t, fy22, "roe", func() *float64 { return fy22.ROE }, 15.75/100) // year-end fallback
	want(t, fy22, "ronw", func() *float64 { return fy22.RONW }, 15.75/100)
	want(t, fy22, "roic", func() *float64 { return fy22.ROIC }, 24*0.75/150)
}
