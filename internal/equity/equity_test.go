package equity

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	const raw = `SYMBOL,NAME OF COMPANY, SERIES, DATE OF LISTING, PAID UP VALUE, MARKET LOT, ISIN NUMBER, FACE VALUE
20MICRONS,20 Microns Limited,EQ,06-OCT-2008,5,1,INE144J01027,5
3IINFOLTD,3i Infotech Limited,BE,22-OCT-2021,10,1,INE748C01038,10
,Empty Symbol Ltd,EQ,01-JAN-2020,10,1,INE000000000,10
BROKEN
`

	cos, err := Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cos) != 2 {
		t.Fatalf("got %d companies, want 2: %+v", len(cos), cos)
	}

	first := cos[0]
	if first.Symbol != "20MICRONS" {
		t.Errorf("symbol = %q, want 20MICRONS", first.Symbol)
	}
	if first.Name != "20 Microns Limited" {
		t.Errorf("name = %q, want 20 Microns Limited", first.Name)
	}
	if first.Series != "EQ" {
		t.Errorf("series = %q, want EQ", first.Series)
	}
	if first.ISINCode != "INE144J01027" {
		t.Errorf("isin = %q, want INE144J01027", first.ISINCode)
	}
	if first.Exchange != Exchange {
		t.Errorf("exchange = %q, want %q", first.Exchange, Exchange)
	}
	if cos[1].Series != "BE" {
		t.Errorf("second series = %q, want BE", cos[1].Series)
	}
}

func TestParseReorderedColumns(t *testing.T) {
	const raw = `NAME OF COMPANY,SERIES,SYMBOL,ISIN NUMBER
Reliance Industries Limited,EQ,RELIANCE,INE002A01018
`
	cos, err := Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cos) != 1 {
		t.Fatalf("got %d companies, want 1", len(cos))
	}
	if cos[0].Symbol != "RELIANCE" || cos[0].ISINCode != "INE002A01018" {
		t.Errorf("unexpected company: %+v", cos[0])
	}
}

func TestParseMissingSymbolColumn(t *testing.T) {
	if _, err := Parse(strings.NewReader("NAME,ISIN\nFoo,INE1\n")); err == nil {
		t.Fatal("expected error for missing SYMBOL column")
	}
}
