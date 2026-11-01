// Package equity parses the NSE equity master list (EQUITY_L.csv) into
// Company records. The CSV columns are matched by header name, and every
// parsed company is tagged with the BSE exchange: AlphaVantage queries use
// BSE symbols (SYMBOL.BSE), which is what the av client builds from the
// exchange field.
package equity

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strings"

	"vyasa/src/models"
)

// Exchange is the exchange assigned to every company parsed from EQUITY_L.csv.
const Exchange = "BSE"

// CSV columns the parser consumes.
const (
	colSymbol = "SYMBOL"
	colName   = "NAME OF COMPANY"
	colSeries = "SERIES"
	colISIN   = "ISIN NUMBER"
)

// ParseFile reads an EQUITY_L.csv file from path and parses it.
func ParseFile(path string) ([]models.Company, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("equity: open %s: %w", path, err)
	}
	defer f.Close()
	return Parse(f)
}

// Parse reads an EQUITY_L.csv stream and returns one Company per usable row.
// Columns are resolved by header name so the order is not assumed. Rows with
// no symbol or ragged columns are skipped rather than failing the whole file.
func Parse(r io.Reader) ([]models.Company, error) {
	cr := csv.NewReader(r)
	cr.TrimLeadingSpace = true
	cr.FieldsPerRecord = -1

	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("equity: read header: %w", err)
	}
	idx := map[string]int{}
	for i, h := range header {
		idx[strings.TrimSpace(h)] = i
	}
	symAt, ok := idx[colSymbol]
	if !ok {
		return nil, fmt.Errorf("equity: missing %q column", colSymbol)
	}

	field := func(rec []string, name string) string {
		i, ok := idx[name]
		if !ok || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}

	var out []models.Company
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("equity: read row: %w", err)
		}
		if len(rec) < len(header) {
			continue
		}
		symbol := strings.TrimSpace(rec[symAt])
		if symbol == "" {
			continue
		}
		out = append(out, models.Company{
			Symbol:   strings.ToUpper(symbol),
			Exchange: Exchange,
			Name:     field(rec, colName),
			Series:   field(rec, colSeries),
			ISINCode: field(rec, colISIN),
		})
	}
	return out, nil
}
