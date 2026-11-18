package models

import "time"

// Table names for the market data clients. They match the legacy collection
// names so existing data can be imported as-is.
const (
	TableAVTimeSeries  = "av_timeseries"
	TableAVGlobalQuote = "av_global_quote"

	TableIndiaSMStock = "indiasm_stock"
	TableTickertape   = "tickertape"

	// TableAPIUsage tracks per-client outbound request budgets per day,
	// keyed by (client, date); used by the tickertapev2 scraper to enforce
	// its daily cap.
	TableAPIUsage = "api_usage"
)

// Tickertape pageProps sections; each is stored in its own table so the
// payload fields stay queryable through the jsonb response column.
const (
	TableTickerCommentary          = "ticker_commentary"
	TableTickerFaq                 = "ticker_faq"
	TableTickerIncomeAnnual        = "ticker_income_annual"
	TableTickerIncomeInterim       = "ticker_income_interim"
	TableTickerBalancesheetAnnual  = "ticker_balancesheet_annual"
	TableTickerCashflowAnnual      = "ticker_cashflow_annual"
	TableTickerPeers               = "ticker_peers"
	TableTickerEventsCorpActions   = "ticker_events_corp_actions"
	TableTickerEventsAnnouncements = "ticker_events_announcements"
	TableTickerEventsLegal         = "ticker_events_legal"

	// TableTickerMetrics holds the per-FY derived metrics computed from the
	// three annual statement tables (see package metrics).
	TableTickerMetrics = "ticker_metrics"
)

// TickerMetrics is one per-FY row of derived key metrics computed from the
// annual tickertape statement tables (see package metrics). Metric values
// are nil when their inputs were missing for that year; every skipped
// computation is itemised in DataGaps. Ratio-style metrics are stored as
// fractions (0.15 = 15%), day-based ones in days.
type TickerMetrics struct {
	CompanyID string    `json:"company_id"`
	Symbol    string    `json:"symbol"`
	FY        string    `json:"fy"`
	EndDate   time.Time `json:"end_date"`
	DataGaps  []string  `json:"data_gaps,omitempty"`

	// Raw figures copied from the statements.
	TotalRevenue       *float64 `json:"total_revenue,omitempty"`
	GrossProfit        *float64 `json:"gross_profit,omitempty"`
	EBITDA             *float64 `json:"ebitda,omitempty"`
	EBIT               *float64 `json:"ebit,omitempty"`
	PBT                *float64 `json:"pbt,omitempty"`
	NetIncome          *float64 `json:"net_income,omitempty"`
	EPS                *float64 `json:"eps,omitempty"`
	CFO                *float64 `json:"cfo,omitempty"`
	Capex              *float64 `json:"capex,omitempty"`
	FCF                *float64 `json:"fcf,omitempty"`
	TotalEquity        *float64 `json:"total_equity,omitempty"`
	MinorityInterest   *float64 `json:"minority_interest,omitempty"`
	NetWorth           *float64 `json:"net_worth,omitempty"`
	TotalDebt          *float64 `json:"total_debt,omitempty"`
	CashAndSTI         *float64 `json:"cash_and_sti,omitempty"`
	TotalAssets        *float64 `json:"total_assets,omitempty"`
	CurrentAssets      *float64 `json:"current_assets,omitempty"`
	CurrentLiabilities *float64 `json:"current_liabilities,omitempty"`
	Receivables        *float64 `json:"receivables,omitempty"`
	Inventory          *float64 `json:"inventory,omitempty"`
	Payables           *float64 `json:"payables,omitempty"`
	SharesOutstanding  *float64 `json:"shares_outstanding,omitempty"`

	// Growth.
	RevenueYoY    *float64 `json:"revenue_yoy,omitempty"`
	RevenueCAGR3Y *float64 `json:"revenue_cagr_3y,omitempty"`
	RevenueCAGR5Y *float64 `json:"revenue_cagr_5y,omitempty"`
	EBITDAYoY     *float64 `json:"ebitda_yoy,omitempty"`
	EBITDACAGR3Y  *float64 `json:"ebitda_cagr_3y,omitempty"`
	EBITDACAGR5Y  *float64 `json:"ebitda_cagr_5y,omitempty"`
	EBITYoY       *float64 `json:"ebit_yoy,omitempty"`
	EBITCAGR3Y    *float64 `json:"ebit_cagr_3y,omitempty"`
	EBITCAGR5Y    *float64 `json:"ebit_cagr_5y,omitempty"`
	PATYoY        *float64 `json:"pat_yoy,omitempty"`
	PATCAGR3Y     *float64 `json:"pat_cagr_3y,omitempty"`
	PATCAGR5Y     *float64 `json:"pat_cagr_5y,omitempty"`
	EPSYoY        *float64 `json:"eps_yoy,omitempty"`
	EPSCAGR3Y     *float64 `json:"eps_cagr_3y,omitempty"`
	EPSCAGR5Y     *float64 `json:"eps_cagr_5y,omitempty"`

	// Operating leverage and margins.
	DOLEBIT      *float64 `json:"dol_ebit,omitempty"`
	DOLEBITDA    *float64 `json:"dol_ebitda,omitempty"`
	GrossMargin  *float64 `json:"gross_margin,omitempty"`
	EBITDAMargin *float64 `json:"ebitda_margin,omitempty"`
	EBITMargin   *float64 `json:"ebit_margin,omitempty"`
	NetMargin    *float64 `json:"net_margin,omitempty"`

	// Returns.
	ROE  *float64 `json:"roe,omitempty"`
	RONW *float64 `json:"ronw,omitempty"`
	ROCE *float64 `json:"roce,omitempty"`
	ROIC *float64 `json:"roic,omitempty"`

	// Solvency and liquidity.
	DebtToEquity     *float64 `json:"debt_to_equity,omitempty"`
	NetDebt          *float64 `json:"net_debt,omitempty"`
	NetDebtToEBITDA  *float64 `json:"net_debt_to_ebitda,omitempty"`
	InterestCoverage *float64 `json:"interest_coverage,omitempty"`
	CurrentRatio     *float64 `json:"current_ratio,omitempty"`

	// Working capital.
	DSODays           *float64 `json:"dso_days,omitempty"`
	DIODays           *float64 `json:"dio_days,omitempty"`
	DPODays           *float64 `json:"dpo_days,omitempty"`
	WCCycleDays       *float64 `json:"wc_cycle_days,omitempty"`
	ReceivablesYoY    *float64 `json:"receivables_yoy,omitempty"`
	ReceivablesPctRev *float64 `json:"receivables_pct_revenue,omitempty"`
	InventoryYoY      *float64 `json:"inventory_yoy,omitempty"`
	InventoryPctRev   *float64 `json:"inventory_pct_revenue,omitempty"`
	PayablesYoY       *float64 `json:"payables_yoy,omitempty"`
	PayablesPctCOGS   *float64 `json:"payables_pct_cogs,omitempty"`

	// Cash flow quality.
	FCFMargin   *float64 `json:"fcf_margin,omitempty"`
	CFOToPAT    *float64 `json:"cfo_to_pat,omitempty"`
	FCFToPAT    *float64 `json:"fcf_to_pat,omitempty"`
	CFOToEBITDA *float64 `json:"cfo_to_ebitda,omitempty"`
}

// TickerMetricColumns lists the metric columns of ticker_metrics in schema
// order. It is the single source of truth for scanning full metric rows and
// for validating agent-supplied metric names (tools.go in package metrics).
var TickerMetricColumns = []string{
	"total_revenue", "gross_profit", "ebitda", "ebit", "pbt", "net_income", "eps",
	"cfo", "capex", "fcf",
	"total_equity", "minority_interest", "net_worth", "total_debt", "cash_and_sti",
	"total_assets", "current_assets", "current_liabilities",
	"receivables", "inventory", "payables", "shares_outstanding",

	"revenue_yoy", "revenue_cagr_3y", "revenue_cagr_5y",
	"ebitda_yoy", "ebitda_cagr_3y", "ebitda_cagr_5y",
	"ebit_yoy", "ebit_cagr_3y", "ebit_cagr_5y",
	"pat_yoy", "pat_cagr_3y", "pat_cagr_5y",
	"eps_yoy", "eps_cagr_3y", "eps_cagr_5y",

	"dol_ebit", "dol_ebitda",
	"gross_margin", "ebitda_margin", "ebit_margin", "net_margin",

	"roe", "ronw", "roce", "roic",

	"debt_to_equity", "net_debt", "net_debt_to_ebitda", "interest_coverage", "current_ratio",

	"dso_days", "dio_days", "dpo_days", "wc_cycle_days",
	"receivables_yoy", "receivables_pct_revenue",
	"inventory_yoy", "inventory_pct_revenue",
	"payables_yoy", "payables_pct_cogs",

	"fcf_margin", "cfo_to_pat", "fcf_to_pat", "cfo_to_ebitda",
}
