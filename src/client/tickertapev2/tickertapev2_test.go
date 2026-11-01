package tickertapev2

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vyasa/src/client/tickertape"
)

func TestBaseSymbol(t *testing.T) {
	cases := map[string]string{
		"reliance":     "RELIANCE",
		" reliance.ns": "RELIANCE",
		"KOTAKBANK.BO": "KOTAKBANK",
		"INFY.":        "INFY",
		"DEEPAKFERT":   "DEEPAKFERT",
	}
	for in, want := range cases {
		if got := baseSymbol(in); got != want {
			t.Errorf("baseSymbol(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPickStock(t *testing.T) {
	stocks := []searchStock{
		{Ticker: "RELIANCEPP", Type: "stock", Slug: "/stocks/reliance-industries----partly-paid-up-RELIpp"},
		{Ticker: "RELIANCE", Type: "stock", Slug: "/stocks/reliance-industries-RELI"},
		{Ticker: "RCOM", Type: "stock", Slug: "/stocks/reliance-communications-RLCM"},
	}

	got, ok := pickStock(stocks, "RELIANCE")
	if !ok {
		t.Fatal("pickStock returned no match")
	}
	if got.Slug != "/stocks/reliance-industries-RELI" {
		t.Errorf("pickStock slug = %q, want exact RELIANCE match", got.Slug)
	}

	if _, ok := pickStock(nil, "RELIANCE"); ok {
		t.Error("pickStock matched on empty results")
	}

	fallback, ok := pickStock(stocks[2:], "UNKNOWN")
	if !ok || fallback.Ticker != "RCOM" {
		t.Errorf("pickStock fallback = %+v, ok=%v; want RCOM", fallback, ok)
	}
}

func TestPageURL(t *testing.T) {
	if got := pageURL("/stocks/reliance-industries-RELI"); got != "https://www.tickertape.in/stocks/reliance-industries-RELI" {
		t.Errorf("pageURL = %q", got)
	}
	if got := pageURL("stocks/x-XXXX"); got != "https://www.tickertape.in/stocks/x-XXXX" {
		t.Errorf("pageURL without leading slash = %q", got)
	}
}

func TestExtractSectionsFromSavedPage(t *testing.T) {
	path := filepath.Join("..", "..", "..", "tickertape_files", "reliance_tickertape.html")
	page, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("fixture unavailable: %v", err)
	}

	sections, err := tickertape.ExtractSections("RELIANCE", page)
	if err != nil {
		t.Fatalf("ExtractSections: %v", err)
	}
	for _, key := range []string{
		"commentary",
		"stockPageFaq",
		"income-normal-annual",
		"income-normal-interim",
		"balancesheet-normal-annual",
		"cashflow-normal-annual",
		"peers-technical",
		"events-corp-actions",
		"events-announcements",
		"events-legal",
	} {
		if _, ok := sections[key]; !ok {
			t.Errorf("missing section %q", key)
		}
	}

	// Statement keys must be normalised to the readable labels metrics reads.
	if got := string(sections["income-normal-annual"]); !strings.Contains(got, "Total Revenue") {
		t.Errorf("income-normal-annual not normalised: %.200s", got)
	}
}
