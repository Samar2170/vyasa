package models

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
)
