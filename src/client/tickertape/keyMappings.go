package tickertape

import (
	"encoding/json"
	"fmt"
	"strings"
)

var incomeKeyMappings = map[string]string{
	"incTrev": "Total Revenue",
	"incCrev": "Cost Of Revenue",
	"incGpro": "Gross Profit",
	"incOpc":  "Operating Cost",
	"incRaw":  "Raw Material Cost",
	"incPfc":  "Power and Fuel Cost",
	"incEpc":  "Employee Cost",
	"incSga":  "Selling & Administrative Expenses",
	"incOpe":  "Operating & Other expenses",
	"incEbi":  "EBITDA",
	"incDep":  "Depreciation",
	"incPbi":  "EBIT",
	"incIoi":  "Interest",
	"incPbt":  "PBT",
	"incToi":  "Taxes & Other Items",
	"incNinc": "Net Income",
	"incEps":  "EPS",
	"incDps":  "Dividend Per share",
	"incPyr":  "Payout Ratio",
}

var balancesheetKeys = map[string]string{
	"balCsti": "Cash and Short Term Investments",
	"balTrec": "Total Recievables",
	"balTinv": "Total Inventory",
	"balOca":  "Other Current Assets",
	"balTca":  "Total Current Assets",
	"balNetl": "Net Liabilities",
	"balNppe": "Non-Current Property, Plant and Equipment",
	"balGint": "Goodwill",
	"balLti":  "Long Term Investments",
	"balOtha": "Other Total Assets",
	"balTota": "Total Assets",
	"balAccp": "Accounts Payable",
	"balTdep": "Total Depreciation",
	"balOcl":  "Other Current Liabilities",
	"balTcl":  "Total Current Liabilities",
	"balTltd": "Total Long Term Debt",
	"balTdeb": "Total Debt",
	"balDit":  "Deferred Income Taxes",
	"balMint": "Minority Interest",
	"balOthl": "Other Long Term Liabilities",
	"balTotl": "Total Liabilities",
	"balComs": "Common Stock",
	"balApic": "Additional Paid-in Capital",
	"balRtne": "Retained Earnings",
	"balOeq":  "Other Equity",
	"balTeq":  "Total Equity",
	"balTlse": "Total Liabilities and Equity",
	"balTcso": "Total Common Stock Outstanding",
	"balTpso": "Total Preferred Stock Outstanding",
	"balNca":  "Net Current Assets",
	"balCa":   "Cash",
	"balNcl":  "Net Current Liabilities",
	"balDta":  "Deferred Tax Assets",
}

var cashflowKeys = map[string]string{
	"cafCiwc": "Change in Working Capital",
	"cafCfoa": "Cash Flow from Operating Activities",
	"cafCexp": "Capital Expenditure",
	"cafCfia": "Cash Flow from Investing Activities",
	"cafTcdp": "Total Cash Dividends Paid",
	"cafCffa": "Cash Flow from Financing Activities",
	"cafNcic": "Net Change in Cash and Cash Equivalents",
	"cafFcf":  "Free Cash Flow",
}

// sectionKeyMappings maps each financial statement section to the key renames
// applied to its rows before the data is saved.
var sectionKeyMappings = map[string]map[string]string{
	"income-normal-annual":       incomeKeyMappings,
	"income-normal-interim":      incomeKeyMappings,
	"balancesheet-normal-annual": balancesheetKeys,
	"cashflow-normal-annual":     cashflowKeys,
}

// renameStatementKeys rewrites the metric keys of the financial statement
// sections so stored rows use the readable labels from the key mappings.
// Metadata keys (displayPeriod, endDate, reporting) are left untouched, and
// interim income rows (qInc*) are matched after stripping the leading "q".
func renameStatementKeys(sections map[string]json.RawMessage) error {
	for section, mapping := range sectionKeyMappings {
		raw, ok := sections[section]
		if !ok {
			continue
		}
		renamed, err := renameRowKeys(raw, mapping)
		if err != nil {
			return fmt.Errorf("%s: rename keys: %w", section, err)
		}
		sections[section] = renamed
	}
	return nil
}

// renameRowKeys renames object keys across a JSON array of statement rows.
func renameRowKeys(raw json.RawMessage, mapping map[string]string) (json.RawMessage, error) {
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	for i, row := range rows {
		out := make(map[string]json.RawMessage, len(row))
		for key, value := range row {
			out[lookupKey(key, mapping)] = value
		}
		rows[i] = out
	}
	return json.Marshal(rows)
}

// lookupKey returns the mapped label for key, falling back to key. A leading
// "q" (interim income rows) is ignored so qInc* rows reuse incomeKeyMappings.
func lookupKey(key string, mapping map[string]string) string {
	if label, ok := mapping[key]; ok {
		return label
	}
	if strings.HasPrefix(key, "q") && len(key) > 1 {
		base := strings.ToLower(key[1:2]) + key[2:]
		if label, ok := mapping[base]; ok {
			return label
		}
	}
	return key
}
