// Metrics persistence: reads the latest annual-statement snapshots the
// metrics package computes from, and upserts the computed per-FY rows into
// ticker_metrics.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"vyasa/src/models"
)

// LatestStatement returns the response array of the most recent snapshot
// stored for symbol in one of the annual statement tables (income, balancesheet,
// cashflow). ok is false when no snapshot exists.
func (s *Store) LatestStatement(ctx context.Context, table, symbol string) (json.RawMessage, bool, error) {
	var raw []byte
	err := s.Pool.QueryRow(ctx,
		`SELECT response FROM `+table+` WHERE symbol = $1 ORDER BY date DESC LIMIT 1`,
		symbol,
	).Scan(&raw)
	if IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: read %s %s: %w", table, symbol, err)
	}
	return json.RawMessage(raw), true, nil
}

// MetricsSymbols lists every symbol that has a stored snapshot in any of the
// three annual statement tables.
func (s *Store) MetricsSymbols(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT DISTINCT symbol FROM `+models.TableTickerIncomeAnnual+`
		UNION SELECT DISTINCT symbol FROM `+models.TableTickerBalancesheetAnnual+`
		UNION SELECT DISTINCT symbol FROM `+models.TableTickerCashflowAnnual+`
		ORDER BY symbol`)
	if err != nil {
		return nil, fmt.Errorf("store: list metrics symbols: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var sym string
		if err := rows.Scan(&sym); err != nil {
			return nil, fmt.Errorf("store: scan metrics symbols: %w", err)
		}
		out = append(out, sym)
	}
	return out, rows.Err()
}

// GetMetrics returns every stored metrics row for a symbol, oldest to newest
// FY. Tool layers slice and filter from this full series.
func (s *Store) GetMetrics(ctx context.Context, symbol string) ([]models.TickerMetrics, error) {
	cols := `company_id, symbol, fy, end_date, data_gaps, ` + strings.Join(models.TickerMetricColumns, ", ")
	rows, err := s.Pool.Query(ctx,
		`SELECT `+cols+` FROM ticker_metrics WHERE symbol = $1 ORDER BY fy`, symbol)
	if err != nil {
		return nil, fmt.Errorf("store: get metrics %s: %w", symbol, err)
	}
	defer rows.Close()

	var out []models.TickerMetrics
	for rows.Next() {
		m, err := scanMetrics(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan metrics %s: %w", symbol, err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// scanMetrics scans one ticker_metrics row; the destination order must match
// the SELECT column list of GetMetrics.
func scanMetrics(row pgx.Row) (models.TickerMetrics, error) {
	var (
		m      models.TickerMetrics
		gaps   []byte
		fields = metricFieldPtrs(&m)
	)
	dests := make([]any, 0, 5+len(fields))
	dests = append(dests, &m.CompanyID, &m.Symbol, &m.FY, &m.EndDate, &gaps)
	dests = append(dests, fields...) // **float64 targets: NULL scans to nil
	if err := row.Scan(dests...); err != nil {
		return m, err
	}
	if len(gaps) > 0 {
		if err := json.Unmarshal(gaps, &m.DataGaps); err != nil {
			return m, fmt.Errorf("decode data_gaps: %w", err)
		}
	}
	return m, nil
}

// metricFieldPtrs returns the model's metric fields as scan destinations
// (**float64 each) in models.TickerMetricColumns order, so NULL columns scan
// to nil fields.
func metricFieldPtrs(m *models.TickerMetrics) []any {
	return []any{
		&m.TotalRevenue, &m.GrossProfit, &m.EBITDA, &m.EBIT, &m.PBT, &m.NetIncome, &m.EPS,
		&m.CFO, &m.Capex, &m.FCF,
		&m.TotalEquity, &m.MinorityInterest, &m.NetWorth, &m.TotalDebt, &m.CashAndSTI,
		&m.TotalAssets, &m.CurrentAssets, &m.CurrentLiabilities,
		&m.Receivables, &m.Inventory, &m.Payables, &m.SharesOutstanding,

		&m.RevenueYoY, &m.RevenueCAGR3Y, &m.RevenueCAGR5Y,
		&m.EBITDAYoY, &m.EBITDACAGR3Y, &m.EBITDACAGR5Y,
		&m.EBITYoY, &m.EBITCAGR3Y, &m.EBITCAGR5Y,
		&m.PATYoY, &m.PATCAGR3Y, &m.PATCAGR5Y,
		&m.EPSYoY, &m.EPSCAGR3Y, &m.EPSCAGR5Y,

		&m.DOLEBIT, &m.DOLEBITDA,
		&m.GrossMargin, &m.EBITDAMargin, &m.EBITMargin, &m.NetMargin,

		&m.ROE, &m.RONW, &m.ROCE, &m.ROIC,

		&m.DebtToEquity, &m.NetDebt, &m.NetDebtToEBITDA, &m.InterestCoverage, &m.CurrentRatio,

		&m.DSODays, &m.DIODays, &m.DPODays, &m.WCCycleDays,
		&m.ReceivablesYoY, &m.ReceivablesPctRev,
		&m.InventoryYoY, &m.InventoryPctRev,
		&m.PayablesYoY, &m.PayablesPctCOGS,

		&m.FCFMargin, &m.CFOToPAT, &m.FCFToPAT, &m.CFOToEBITDA,
	}
}

// UpsertMetrics writes the per-FY metrics rows keyed by (symbol, fy).
func (s *Store) UpsertMetrics(ctx context.Context, rows []models.TickerMetrics) error {
	if len(rows) == 0 {
		return nil
	}
	now := time.Now()
	for _, m := range rows {
		gaps := "[]"
		if len(m.DataGaps) > 0 {
			buf, err := json.Marshal(m.DataGaps)
			if err != nil {
				return fmt.Errorf("store: encode data_gaps %s %s: %w", m.Symbol, m.FY, err)
			}
			gaps = string(buf)
		}
		_, err := s.Pool.Exec(ctx, `
			INSERT INTO ticker_metrics (
				company_id, symbol, fy, end_date, data_gaps,
				total_revenue, gross_profit, ebitda, ebit, pbt, net_income, eps,
				cfo, capex, fcf,
				total_equity, minority_interest, net_worth, total_debt, cash_and_sti,
				total_assets, current_assets, current_liabilities,
				receivables, inventory, payables, shares_outstanding,
				revenue_yoy, revenue_cagr_3y, revenue_cagr_5y,
				ebitda_yoy, ebitda_cagr_3y, ebitda_cagr_5y,
				ebit_yoy, ebit_cagr_3y, ebit_cagr_5y,
				pat_yoy, pat_cagr_3y, pat_cagr_5y,
				eps_yoy, eps_cagr_3y, eps_cagr_5y,
				dol_ebit, dol_ebitda,
				gross_margin, ebitda_margin, ebit_margin, net_margin,
				roe, ronw, roce, roic,
				debt_to_equity, net_debt, net_debt_to_ebitda, interest_coverage, current_ratio,
				dso_days, dio_days, dpo_days, wc_cycle_days,
				receivables_yoy, receivables_pct_revenue,
				inventory_yoy, inventory_pct_revenue,
				payables_yoy, payables_pct_cogs,
				fcf_margin, cfo_to_pat, fcf_to_pat, cfo_to_ebitda,
				created_at, updated_at
			) VALUES (
				$1, $2, $3, $4, $5,
				$6, $7, $8, $9, $10, $11, $12,
				$13, $14, $15,
				$16, $17, $18, $19, $20,
				$21, $22, $23,
				$24, $25, $26, $27,
				$28, $29, $30,
				$31, $32, $33,
				$34, $35, $36,
				$37, $38, $39,
				$40, $41, $42,
				$43, $44,
				$45, $46, $47, $48,
				$49, $50, $51, $52,
				$53, $54, $55, $56, $57,
				$58, $59, $60, $61,
				$62, $63,
				$64, $65,
				$66, $67,
				$68, $69, $70, $71,
				$72, $73
			)
			ON CONFLICT (symbol, fy) DO UPDATE SET
				company_id = EXCLUDED.company_id,
				end_date = EXCLUDED.end_date,
				data_gaps = EXCLUDED.data_gaps,
				total_revenue = EXCLUDED.total_revenue,
				gross_profit = EXCLUDED.gross_profit,
				ebitda = EXCLUDED.ebitda,
				ebit = EXCLUDED.ebit,
				pbt = EXCLUDED.pbt,
				net_income = EXCLUDED.net_income,
				eps = EXCLUDED.eps,
				cfo = EXCLUDED.cfo,
				capex = EXCLUDED.capex,
				fcf = EXCLUDED.fcf,
				total_equity = EXCLUDED.total_equity,
				minority_interest = EXCLUDED.minority_interest,
				net_worth = EXCLUDED.net_worth,
				total_debt = EXCLUDED.total_debt,
				cash_and_sti = EXCLUDED.cash_and_sti,
				total_assets = EXCLUDED.total_assets,
				current_assets = EXCLUDED.current_assets,
				current_liabilities = EXCLUDED.current_liabilities,
				receivables = EXCLUDED.receivables,
				inventory = EXCLUDED.inventory,
				payables = EXCLUDED.payables,
				shares_outstanding = EXCLUDED.shares_outstanding,
				revenue_yoy = EXCLUDED.revenue_yoy,
				revenue_cagr_3y = EXCLUDED.revenue_cagr_3y,
				revenue_cagr_5y = EXCLUDED.revenue_cagr_5y,
				ebitda_yoy = EXCLUDED.ebitda_yoy,
				ebitda_cagr_3y = EXCLUDED.ebitda_cagr_3y,
				ebitda_cagr_5y = EXCLUDED.ebitda_cagr_5y,
				ebit_yoy = EXCLUDED.ebit_yoy,
				ebit_cagr_3y = EXCLUDED.ebit_cagr_3y,
				ebit_cagr_5y = EXCLUDED.ebit_cagr_5y,
				pat_yoy = EXCLUDED.pat_yoy,
				pat_cagr_3y = EXCLUDED.pat_cagr_3y,
				pat_cagr_5y = EXCLUDED.pat_cagr_5y,
				eps_yoy = EXCLUDED.eps_yoy,
				eps_cagr_3y = EXCLUDED.eps_cagr_3y,
				eps_cagr_5y = EXCLUDED.eps_cagr_5y,
				dol_ebit = EXCLUDED.dol_ebit,
				dol_ebitda = EXCLUDED.dol_ebitda,
				gross_margin = EXCLUDED.gross_margin,
				ebitda_margin = EXCLUDED.ebitda_margin,
				ebit_margin = EXCLUDED.ebit_margin,
				net_margin = EXCLUDED.net_margin,
				roe = EXCLUDED.roe,
				ronw = EXCLUDED.ronw,
				roce = EXCLUDED.roce,
				roic = EXCLUDED.roic,
				debt_to_equity = EXCLUDED.debt_to_equity,
				net_debt = EXCLUDED.net_debt,
				net_debt_to_ebitda = EXCLUDED.net_debt_to_ebitda,
				interest_coverage = EXCLUDED.interest_coverage,
				current_ratio = EXCLUDED.current_ratio,
				dso_days = EXCLUDED.dso_days,
				dio_days = EXCLUDED.dio_days,
				dpo_days = EXCLUDED.dpo_days,
				wc_cycle_days = EXCLUDED.wc_cycle_days,
				receivables_yoy = EXCLUDED.receivables_yoy,
				receivables_pct_revenue = EXCLUDED.receivables_pct_revenue,
				inventory_yoy = EXCLUDED.inventory_yoy,
				inventory_pct_revenue = EXCLUDED.inventory_pct_revenue,
				payables_yoy = EXCLUDED.payables_yoy,
				payables_pct_cogs = EXCLUDED.payables_pct_cogs,
				fcf_margin = EXCLUDED.fcf_margin,
				cfo_to_pat = EXCLUDED.cfo_to_pat,
				fcf_to_pat = EXCLUDED.fcf_to_pat,
				cfo_to_ebitda = EXCLUDED.cfo_to_ebitda,
				updated_at = EXCLUDED.updated_at`,
			m.CompanyID, m.Symbol, m.FY, m.EndDate, gaps,
			m.TotalRevenue, m.GrossProfit, m.EBITDA, m.EBIT, m.PBT, m.NetIncome, m.EPS,
			m.CFO, m.Capex, m.FCF,
			m.TotalEquity, m.MinorityInterest, m.NetWorth, m.TotalDebt, m.CashAndSTI,
			m.TotalAssets, m.CurrentAssets, m.CurrentLiabilities,
			m.Receivables, m.Inventory, m.Payables, m.SharesOutstanding,
			m.RevenueYoY, m.RevenueCAGR3Y, m.RevenueCAGR5Y,
			m.EBITDAYoY, m.EBITDACAGR3Y, m.EBITDACAGR5Y,
			m.EBITYoY, m.EBITCAGR3Y, m.EBITCAGR5Y,
			m.PATYoY, m.PATCAGR3Y, m.PATCAGR5Y,
			m.EPSYoY, m.EPSCAGR3Y, m.EPSCAGR5Y,
			m.DOLEBIT, m.DOLEBITDA,
			m.GrossMargin, m.EBITDAMargin, m.EBITMargin, m.NetMargin,
			m.ROE, m.RONW, m.ROCE, m.ROIC,
			m.DebtToEquity, m.NetDebt, m.NetDebtToEBITDA, m.InterestCoverage, m.CurrentRatio,
			m.DSODays, m.DIODays, m.DPODays, m.WCCycleDays,
			m.ReceivablesYoY, m.ReceivablesPctRev,
			m.InventoryYoY, m.InventoryPctRev,
			m.PayablesYoY, m.PayablesPctCOGS,
			m.FCFMargin, m.CFOToPAT, m.FCFToPAT, m.CFOToEBITDA,
			now, now,
		)
		if err != nil {
			return fmt.Errorf("store: upsert ticker_metrics %s %s: %w", m.Symbol, m.FY, err)
		}
	}
	return nil
}
