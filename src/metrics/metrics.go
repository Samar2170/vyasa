// Package metrics derives per-FY key metrics from the annual tickertape
// statement tables (income, balancesheet, cashflow). Each statement snapshot
// is a JSON array of rows keyed by readable labels (see the tickertape key
// mappings); rows are joined on their "FY YYYY" displayPeriod, the raw
// figures are copied and the derived metrics — growth, margins, returns,
// solvency, working capital and cash-flow quality — are computed against the
// neighbouring years of the series.
//
// Conventions: metric values are fractions (0.15 = 15%) except the day-based
// working-capital metrics; a metric is NULL when any of its inputs is
// missing or the computation is undefined (zero denominator, non-positive
// CAGR base), and every such skip is itemised in the row's data_gaps.
package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"vyasa/src/models"
	"vyasa/src/store"
)

// Source field labels exactly as they are stored in the statement tables
// (the tickertape client renames the raw keys to these labels before saving).
const (
	srcRevenue     = "Total Revenue"
	srcCOGS        = "Cost Of Revenue"
	srcGrossProfit = "Gross Profit"
	srcEBITDA      = "EBITDA"
	srcEBIT        = "EBIT"
	srcInterest    = "Interest"
	srcPBT         = "PBT"
	srcTaxes       = "Taxes & Other Items"
	srcNetIncome   = "Net Income"
	srcEPS         = "EPS"

	srcEquity        = "Total Equity"
	srcMinority      = "Minority Interest"
	srcTotalDebt     = "Total Debt"
	srcCashSTI       = "Cash and Short Term Investments"
	srcTotalAssets   = "Total Assets"
	srcCurrentAssets = "Total Current Assets"
	srcCurrentLiab   = "Total Current Liabilities"
	srcReceivables   = "Total Recievables"
	srcInventory     = "Total Inventory"
	srcPayables      = "Accounts Payable"
	srcSharesOut     = "Total Common Stock Outstanding"

	srcCFO   = "Cash Flow from Operating Activities"
	srcCapex = "Capital Expenditure"
	srcFCF   = "Free Cash Flow"
)

// fyPattern matches the displayPeriod of annual statement rows ("FY 2017");
// TTM and interim rows are excluded.
var fyPattern = regexp.MustCompile(`^FY (\d{4})$`)

// Compute loads the latest annual statement snapshots for co from the store
// and derives the per-FY metrics rows, ordered oldest to newest FY. It
// returns nil (no error) when the company has no statement data at all.
func Compute(ctx context.Context, st *store.Store, co models.Company) ([]models.TickerMetrics, error) {
	income, okIncome, err := st.LatestStatement(ctx, models.TableTickerIncomeAnnual, co.Symbol)
	if err != nil {
		return nil, fmt.Errorf("metrics: %s: load income: %w", co.Symbol, err)
	}
	balance, okBalance, err := st.LatestStatement(ctx, models.TableTickerBalancesheetAnnual, co.Symbol)
	if err != nil {
		return nil, fmt.Errorf("metrics: %s: load balancesheet: %w", co.Symbol, err)
	}
	cashflow, okCash, err := st.LatestStatement(ctx, models.TableTickerCashflowAnnual, co.Symbol)
	if err != nil {
		return nil, fmt.Errorf("metrics: %s: load cashflow: %w", co.Symbol, err)
	}
	if !okIncome && !okBalance && !okCash {
		return nil, nil
	}
	if !okIncome {
		income = nil
	}
	if !okBalance {
		balance = nil
	}
	if !okCash {
		cashflow = nil
	}
	rows, err := ComputeStatements(co, income, balance, cashflow)
	if err != nil {
		return nil, fmt.Errorf("metrics: %s: %w", co.Symbol, err)
	}
	return rows, nil
}

// ComputeStatements derives the per-FY metrics from three statement JSON
// arrays; a nil argument marks that statement as missing for the company.
func ComputeStatements(co models.Company, income, balance, cashflow json.RawMessage) ([]models.TickerMetrics, error) {
	stmtIncome, err := parseStatement(income)
	if err != nil {
		return nil, fmt.Errorf("parse income statement: %w", err)
	}
	stmtBalance, err := parseStatement(balance)
	if err != nil {
		return nil, fmt.Errorf("parse balancesheet: %w", err)
	}
	stmtCash, err := parseStatement(cashflow)
	if err != nil {
		return nil, fmt.Errorf("parse cashflow: %w", err)
	}

	// Union of the FYs across the statements; each year gets one merged view
	// of its source values plus an accumulator for the metric columns.
	years := make(map[int]bool)
	for _, s := range []statement{stmtIncome, stmtBalance, stmtCash} {
		for y := range s {
			years[y] = true
		}
	}
	accs := make(map[int]*acc, len(years))
	for y := range years {
		a := &acc{
			src:    make(map[string]*float64),
			out:    make(map[string]*float64),
			gapSet: make(map[string]bool),
		}
		if row, ok := stmtIncome[y]; ok {
			a.label, a.endDate = row.label, row.endDate
			for k, v := range row.vals {
				a.src[k] = v
			}
		} else {
			a.gap("income statement: no data")
		}
		if row, ok := stmtBalance[y]; ok {
			if a.label == "" {
				a.label, a.endDate = row.label, row.endDate
			}
			for k, v := range row.vals {
				a.src[k] = v
			}
		} else {
			a.gap("balancesheet: no data")
		}
		if row, ok := stmtCash[y]; ok {
			if a.label == "" {
				a.label, a.endDate = row.label, row.endDate
			}
			for k, v := range row.vals {
				a.src[k] = v
			}
		} else {
			a.gap("cashflow: no data")
		}
		if a.label == "" {
			a.label = "FY" + strconv.Itoa(y)
		}
		a.year = y
		accs[y] = a
	}
	ordered := make([]int, 0, len(accs))
	for y := range accs {
		ordered = append(ordered, y)
	}
	sort.Ints(ordered)

	// Pass 1: raw copies and single-year ratios for every year.
	for _, y := range ordered {
		computeYear(accs[y])
	}
	// Pass 2: metrics that read the neighbouring years of the series.
	for _, y := range ordered {
		computeGrowth(accs, y)
	}

	out := make([]models.TickerMetrics, 0, len(ordered))
	for _, y := range ordered {
		out = append(out, accs[y].model(co))
	}
	return out, nil
}

// statement maps an FY year number to its merged statement row.
type statement map[int]*fyRow

type fyRow struct {
	label   string
	endDate time.Time
	vals    map[string]*float64
}

// parseStatement decodes one statement snapshot array, keeping only the
// "FY YYYY" rows (TTM and interim rows are dropped) and coercing every
// non-metadata field to a nullable float.
func parseStatement(raw json.RawMessage) (statement, error) {
	if len(raw) == 0 {
		return statement{}, nil
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	out := make(statement, len(rows))
	for _, row := range rows {
		var period string
		if err := json.Unmarshal(row["displayPeriod"], &period); err != nil {
			continue
		}
		m := fyPattern.FindStringSubmatch(period)
		if m == nil {
			continue
		}
		year, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		fr := &fyRow{label: "FY" + m[1], vals: make(map[string]*float64)}
		if end, ok := row["endDate"]; ok {
			if t, err := parseEndDate(end); err == nil {
				fr.endDate = t
			}
		}
		for key, rawVal := range row {
			switch key {
			case "displayPeriod", "endDate", "reporting":
				continue
			}
			if v, ok := decodeNumber(rawVal); ok {
				fr.vals[key] = &v
			}
		}
		out[year] = fr
	}
	return out, nil
}

// parseEndDate accepts the ISO timestamps tickertape emits plus plain dates.
func parseEndDate(raw json.RawMessage) (time.Time, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return time.Time{}, err
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable endDate %q", s)
}

// decodeNumber coerces a JSON value to a float; nulls and non-numbers are
// reported as absent.
func decodeNumber(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, false
	}
	return v, true
}

// acc accumulates the metrics of one FY: src holds the merged statement
// values (keyed by their human-readable labels), out the metric columns
// (keyed by their SQL column names), gaps the list of skipped computations.
type acc struct {
	year    int
	label   string
	endDate time.Time
	src     map[string]*float64
	out     map[string]*float64
	gaps    []string
	gapSet  map[string]bool
}

// gap records a skipped computation once per FY.
func (a *acc) gap(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if !a.gapSet[msg] {
		a.gapSet[msg] = true
		a.gaps = append(a.gaps, msg)
	}
}

// input pairs a source value with the name reported in data_gaps.
type input struct {
	name string
	val  *float64
}

func in(name string, v *float64) input { return input{name, v} }

// calc runs f when every named input is present, storing its result under
// metric; otherwise the missing inputs are recorded in data_gaps. When f
// returns nil the computation was undefined (e.g. zero denominator).
func (a *acc) calc(metric string, ins []input, f func() *float64) {
	var missing []string
	for _, x := range ins {
		if x.val == nil {
			missing = append(missing, x.name)
		}
	}
	if len(missing) > 0 {
		a.gap("%s: missing %s", metric, strings.Join(missing, ", "))
		return
	}
	if v := f(); v != nil {
		a.out[metric] = v
	} else {
		a.gap("%s: undefined (zero or invalid denominator)", metric)
	}
}

// div returns a/b, or nil when b is zero.
func div(a, b *float64) *float64 {
	if a == nil || b == nil || *b == 0 {
		return nil
	}
	v := *a / *b
	return &v
}

// sub returns a-b, or nil when either is missing.
func sub(a, b *float64) *float64 {
	if a == nil || b == nil {
		return nil
	}
	v := *a - *b
	return &v
}

// scale returns v*k, or nil when v is missing.
func scale(v *float64, k float64) *float64 {
	if v == nil {
		return nil
	}
	out := *v * k
	return &out
}

// avg returns the mean of the present values; with only one present value it
// is that value (year-end fallback), with none it is nil.
func avg(vals ...*float64) *float64 {
	var sum float64
	var n int
	for _, v := range vals {
		if v != nil {
			sum += *v
			n++
		}
	}
	if n == 0 {
		return nil
	}
	m := sum / float64(n)
	return &m
}

// yoy returns the year-over-year change (cur-prev)/|prev|, nil when prev is
// missing or zero.
func yoy(cur, prev *float64) *float64 {
	if cur == nil || prev == nil || *prev == 0 {
		return nil
	}
	v := (*cur - *prev) / math.Abs(*prev)
	return &v
}

// cagr returns the compound annual growth rate over n years, nil unless both
// endpoints are positive.
func cagr(cur, base *float64, n int) *float64 {
	if cur == nil || base == nil || *base <= 0 || *cur <= 0 || n <= 0 {
		return nil
	}
	v := math.Pow(*cur / *base, 1/float64(n)) - 1
	return &v
}

// pctChange returns (cur-prev)/|prev| like yoy, but reports nil instead of a
// value when the change cannot be computed (missing or zero base).
func pctChange(cur, prev *float64) *float64 { return yoy(cur, prev) }

// netWorth derives equity minus minority interest from a source map.
func netWorth(src map[string]*float64) *float64 {
	return sub(src[srcEquity], src[srcMinority])
}

// capitalEmployed derives total assets minus current liabilities.
func capitalEmployed(src map[string]*float64) *float64 {
	return sub(src[srcTotalAssets], src[srcCurrentLiab])
}

// investedCapital derives total debt plus net worth.
func investedCapital(src map[string]*float64) *float64 {
	if src[srcTotalDebt] == nil || netWorth(src) == nil {
		return nil
	}
	v := *src[srcTotalDebt] + *netWorth(src)
	return &v
}

// effectiveTax returns taxes / PBT when PBT is positive, else nil.
func effectiveTax(src map[string]*float64) *float64 {
	taxes, pbt := src[srcTaxes], src[srcPBT]
	if taxes == nil || pbt == nil || *pbt <= 0 {
		return nil
	}
	v := *taxes / *pbt
	return &v
}

// computeYear fills the raw copies and the single-year derived metrics.
func computeYear(a *acc) {
	s := a.src

	// Raw copies.
	a.out["total_revenue"] = s[srcRevenue]
	a.out["ebitda"] = s[srcEBITDA]
	a.out["ebit"] = s[srcEBIT]
	a.out["pbt"] = s[srcPBT]
	a.out["net_income"] = s[srcNetIncome]
	a.out["eps"] = s[srcEPS]
	a.out["cfo"] = s[srcCFO]
	a.out["capex"] = s[srcCapex]
	a.out["fcf"] = s[srcFCF]
	a.out["total_equity"] = s[srcEquity]
	a.out["minority_interest"] = s[srcMinority]
	a.out["total_debt"] = s[srcTotalDebt]
	a.out["cash_and_sti"] = s[srcCashSTI]
	a.out["total_assets"] = s[srcTotalAssets]
	a.out["current_assets"] = s[srcCurrentAssets]
	a.out["current_liabilities"] = s[srcCurrentLiab]
	a.out["receivables"] = s[srcReceivables]
	a.out["inventory"] = s[srcInventory]
	a.out["payables"] = s[srcPayables]
	a.out["shares_outstanding"] = s[srcSharesOut]

	// Gross profit is only meaningful when cost of revenue is reported;
	// prefer the stated figure, else derive revenue - COGS.
	if s[srcCOGS] != nil {
		if s[srcGrossProfit] != nil {
			a.out["gross_profit"] = s[srcGrossProfit]
		} else if gp := sub(s[srcRevenue], s[srcCOGS]); gp != nil {
			a.out["gross_profit"] = gp
		}
	} else {
		a.gap("gross_profit: missing cost_of_revenue")
	}

	a.out["net_worth"] = netWorth(s)
	a.out["net_debt"] = sub(s[srcTotalDebt], s[srcCashSTI])

	rev, gp := s[srcRevenue], a.out["gross_profit"]
	cogs := s[srcCOGS]
	ebitda, ebit, ni := s[srcEBITDA], s[srcEBIT], s[srcNetIncome]

	a.calc("gross_margin", []input{in("gross_profit", gp), in("revenue", rev)},
		func() *float64 { return div(gp, rev) })
	a.calc("ebitda_margin", []input{in("ebitda", ebitda), in("revenue", rev)},
		func() *float64 { return div(ebitda, rev) })
	a.calc("ebit_margin", []input{in("ebit", ebit), in("revenue", rev)},
		func() *float64 { return div(ebit, rev) })
	a.calc("net_margin", []input{in("net_income", ni), in("revenue", rev)},
		func() *float64 { return div(ni, rev) })

	a.calc("debt_to_equity", []input{in("total_debt", s[srcTotalDebt]), in("total_equity", s[srcEquity])},
		func() *float64 { return div(s[srcTotalDebt], s[srcEquity]) })
	a.calc("net_debt_to_ebitda", []input{in("net_debt", a.out["net_debt"]), in("ebitda", ebitda)},
		func() *float64 { return div(a.out["net_debt"], ebitda) })
	a.calc("interest_coverage", []input{in("ebit", ebit), in("interest", s[srcInterest])},
		func() *float64 { return div(ebit, s[srcInterest]) })
	a.calc("current_ratio", []input{in("current_assets", s[srcCurrentAssets]), in("current_liabilities", s[srcCurrentLiab])},
		func() *float64 { return div(s[srcCurrentAssets], s[srcCurrentLiab]) })

	// Working capital: COGS-based day metrics use cost of revenue only.
	dso := div(scale(s[srcReceivables], 365), rev)
	dio := div(scale(s[srcInventory], 365), cogs)
	dpo := div(scale(s[srcPayables], 365), cogs)
	if dso == nil {
		a.gap("dso_days: missing receivables or revenue")
	} else {
		a.out["dso_days"] = dso
	}
	if dio == nil {
		a.gap("dio_days: missing inventory or cost_of_revenue")
	} else {
		a.out["dio_days"] = dio
	}
	if dpo == nil {
		a.gap("dpo_days: missing payables or cost_of_revenue")
	} else {
		a.out["dpo_days"] = dpo
	}
	a.calc("wc_cycle_days", []input{in("dso", dso), in("dio", dio), in("dpo", dpo)},
		func() *float64 {
			v := *dso + *dio - *dpo
			return &v
		})

	a.calc("receivables_pct_revenue", []input{in("receivables", s[srcReceivables]), in("revenue", rev)},
		func() *float64 { return div(s[srcReceivables], rev) })
	a.calc("inventory_pct_revenue", []input{in("inventory", s[srcInventory]), in("revenue", rev)},
		func() *float64 { return div(s[srcInventory], rev) })
	a.calc("payables_pct_cogs", []input{in("payables", s[srcPayables]), in("cost_of_revenue", cogs)},
		func() *float64 { return div(s[srcPayables], cogs) })

	a.calc("fcf_margin", []input{in("fcf", s[srcFCF]), in("revenue", rev)},
		func() *float64 { return div(s[srcFCF], rev) })
	a.calc("cfo_to_pat", []input{in("cfo", s[srcCFO]), in("net_income", ni)},
		func() *float64 { return div(s[srcCFO], ni) })
	a.calc("fcf_to_pat", []input{in("fcf", s[srcFCF]), in("net_income", ni)},
		func() *float64 { return div(s[srcFCF], ni) })
	a.calc("cfo_to_ebitda", []input{in("cfo", s[srcCFO]), in("ebitda", ebitda)},
		func() *float64 { return div(s[srcCFO], ebitda) })
}

// computeGrowth fills the multi-year metrics of year y: YoY and CAGR growth,
// degrees of operating leverage and the averaged-balance returns.
func computeGrowth(accs map[int]*acc, y int) {
	a := accs[y]
	prev := accs[y-1]

	// Growth series (pat = net income).
	growths := []struct {
		metric string
		col    string
	}{
		{"revenue", "total_revenue"},
		{"ebitda", "ebitda"},
		{"ebit", "ebit"},
		{"pat", "net_income"},
		{"eps", "eps"},
	}
	for _, g := range growths {
		cur, prior := a.out[g.col], valueAt(prev, g.col)
		if vy := yoy(cur, prior); vy != nil {
			a.out[g.metric+"_yoy"] = vy
		} else {
			a.gap("%s_yoy: missing prior-year %s", g.metric, g.metric)
		}
		for _, n := range []int{3, 5} {
			base := valueAt(accs[y-n], g.col)
			if vc := cagr(cur, base, n); vc != nil {
				a.out[g.metric+fmt.Sprintf("_cagr_%dy", n)] = vc
			} else {
				a.gap("%s_cagr_%dy: missing or non-positive %s %d years back", g.metric, n, g.metric, n)
			}
		}
	}

	// Degrees of operating leverage.
	revChange := pctChange(a.out["total_revenue"], valueAt(prev, "total_revenue"))
	for _, l := range []struct {
		metric, col string
	}{{"dol_ebit", "ebit"}, {"dol_ebitda", "ebitda"}} {
		cur, prior := a.out[l.col], valueAt(prev, l.col)
		if cur == nil || prior == nil || revChange == nil {
			a.gap("%s: missing %s or prior-year revenue/%s", l.metric, l.col, l.metric)
			continue
		}
		profitChange := pctChange(cur, prior)
		if profitChange == nil {
			a.gap("%s: undefined (zero prior-year %s)", l.metric, l.col)
			continue
		}
		if *revChange == 0 {
			a.gap("%s: undefined (zero revenue change)", l.metric)
			continue
		}
		v := *profitChange / *revChange
		a.out[l.metric] = &v
	}

	// Oldest year of the series: the returns below fall back to year-end
	// balances, which is expected rather than a data gap.

	// Returns on averaged balances; the year-end value is the fallback for
	// the oldest year of the series.
	avgOver := func(metric string, current *float64, priorVal *float64, f func(avg *float64) *float64) {
		if current == nil {
			a.gap("%s: missing current-period inputs", metric)
			return
		}
		den := avg(current, priorVal)
		if v := f(den); v != nil {
			a.out[metric] = v
		} else {
			a.gap("%s: undefined (zero average denominator)", metric)
		}
	}
	var priorNetWorth, priorCE, priorIC *float64
	if prev != nil {
		priorNetWorth, priorCE, priorIC = netWorth(prev.src), capitalEmployed(prev.src), investedCapital(prev.src)
	}
	ni := a.src[srcNetIncome]
	ebit := a.src[srcEBIT]

	avgOver("roe", a.src[srcEquity], prevSrc(prev, srcEquity), func(den *float64) *float64 { return div(ni, den) })
	avgOver("ronw", netWorth(a.src), priorNetWorth, func(den *float64) *float64 { return div(ni, den) })
	avgOver("roce", capitalEmployed(a.src), priorCE, func(den *float64) *float64 { return div(ebit, den) })

	// ROIC taxes EBIT at the effective rate taxes/PBT.
	tax := effectiveTax(a.src)
	ic := investedCapital(a.src)
	if ic == nil {
		a.gap("roic: missing total_debt or net_worth inputs")
	} else if tax == nil {
		a.gap("roic: missing taxes or positive pbt for the effective tax rate")
	} else {
		den := avg(ic, priorIC)
		if den != nil && *den != 0 {
			v := *ebit * (1 - *tax) / *den
			a.out["roic"] = &v
		} else {
			a.gap("roic: undefined (zero invested capital)")
		}
	}

	// Balance-sheet YoY growth.
	for _, g := range []struct {
		metric, src string
	}{{"receivables", srcReceivables}, {"inventory", srcInventory}, {"payables", srcPayables}} {
		cur, prior := a.src[g.src], prevSrc(prev, g.src)
		if vy := yoy(cur, prior); vy != nil {
			a.out[g.metric+"_yoy"] = vy
		} else {
			a.gap("%s_yoy: missing prior-year %s", g.metric, g.src)
		}
	}
}

// valueAt reads a metric column from an accumulator, nil-safe on nil accs.
func valueAt(a *acc, col string) *float64 {
	if a == nil {
		return nil
	}
	return a.out[col]
}

// prevSrc reads a source value from the prior year, nil-safe on nil accs.
func prevSrc(a *acc, label string) *float64 {
	if a == nil {
		return nil
	}
	return a.src[label]
}

// model converts the accumulator into its persisted row.
func (a *acc) model(co models.Company) models.TickerMetrics {
	o := a.out
	return models.TickerMetrics{
		CompanyID: co.ID,
		Symbol:    co.Symbol,
		FY:        a.label,
		EndDate:   a.endDate,
		DataGaps:  a.gaps,

		TotalRevenue:       o["total_revenue"],
		GrossProfit:        o["gross_profit"],
		EBITDA:             o["ebitda"],
		EBIT:               o["ebit"],
		PBT:                o["pbt"],
		NetIncome:          o["net_income"],
		EPS:                o["eps"],
		CFO:                o["cfo"],
		Capex:              o["capex"],
		FCF:                o["fcf"],
		TotalEquity:        o["total_equity"],
		MinorityInterest:   o["minority_interest"],
		NetWorth:           o["net_worth"],
		TotalDebt:          o["total_debt"],
		CashAndSTI:         o["cash_and_sti"],
		TotalAssets:        o["total_assets"],
		CurrentAssets:      o["current_assets"],
		CurrentLiabilities: o["current_liabilities"],
		Receivables:        o["receivables"],
		Inventory:          o["inventory"],
		Payables:           o["payables"],
		SharesOutstanding:  o["shares_outstanding"],

		RevenueYoY:    o["revenue_yoy"],
		RevenueCAGR3Y: o["revenue_cagr_3y"],
		RevenueCAGR5Y: o["revenue_cagr_5y"],
		EBITDAYoY:     o["ebitda_yoy"],
		EBITDACAGR3Y:  o["ebitda_cagr_3y"],
		EBITDACAGR5Y:  o["ebitda_cagr_5y"],
		EBITYoY:       o["ebit_yoy"],
		EBITCAGR3Y:    o["ebit_cagr_3y"],
		EBITCAGR5Y:    o["ebit_cagr_5y"],
		PATYoY:        o["pat_yoy"],
		PATCAGR3Y:     o["pat_cagr_3y"],
		PATCAGR5Y:     o["pat_cagr_5y"],
		EPSYoY:        o["eps_yoy"],
		EPSCAGR3Y:     o["eps_cagr_3y"],
		EPSCAGR5Y:     o["eps_cagr_5y"],

		DOLEBIT:      o["dol_ebit"],
		DOLEBITDA:    o["dol_ebitda"],
		GrossMargin:  o["gross_margin"],
		EBITDAMargin: o["ebitda_margin"],
		EBITMargin:   o["ebit_margin"],
		NetMargin:    o["net_margin"],

		ROE:  o["roe"],
		RONW: o["ronw"],
		ROCE: o["roce"],
		ROIC: o["roic"],

		DebtToEquity:     o["debt_to_equity"],
		NetDebt:          o["net_debt"],
		NetDebtToEBITDA:  o["net_debt_to_ebitda"],
		InterestCoverage: o["interest_coverage"],
		CurrentRatio:     o["current_ratio"],

		DSODays:           o["dso_days"],
		DIODays:           o["dio_days"],
		DPODays:           o["dpo_days"],
		WCCycleDays:       o["wc_cycle_days"],
		ReceivablesYoY:    o["receivables_yoy"],
		ReceivablesPctRev: o["receivables_pct_revenue"],
		InventoryYoY:      o["inventory_yoy"],
		InventoryPctRev:   o["inventory_pct_revenue"],
		PayablesYoY:       o["payables_yoy"],
		PayablesPctCOGS:   o["payables_pct_cogs"],

		FCFMargin:   o["fcf_margin"],
		CFOToPAT:    o["cfo_to_pat"],
		FCFToPAT:    o["fcf_to_pat"],
		CFOToEBITDA: o["cfo_to_ebitda"],
	}
}
