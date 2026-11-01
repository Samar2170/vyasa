// Command vyasa is the CLI entry point. It wires secrets (.env), settings
// (config.json), PostgreSQL and Archivus storage into the subcommands:
//
//	vyasa seed [-file EQUITY_L.csv]
//	vyasa collect -symbol RELIANCE [-source indiasm,tickertape,av]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"vyasa/internal/equity"
	"vyasa/src/client"
	"vyasa/src/client/archivus"
	"vyasa/src/client/av"
	"vyasa/src/client/indiasm"
	"vyasa/src/client/tickertapev2"
	"vyasa/src/config"
	"vyasa/src/store"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[vyasa] ")

	if err := config.LoadDotEnv(".env"); err != nil {
		log.Fatal(err)
	}
	settings, err := config.Load("config.json")
	if err != nil {
		log.Fatal(err)
	}

	if len(os.Args) < 2 {
		usage()
	}
	ctx := context.Background()
	st := mustStore(ctx, settings)
	defer st.Close()

	switch os.Args[1] {
	case "seed":
		fs := flag.NewFlagSet("seed", flag.ExitOnError)
		file := fs.String("file", "EQUITY_L.csv", "CSV file to seed companies from")
		_ = fs.Parse(os.Args[2:])
		runSeed(ctx, st, *file)
	case "collect":
		fs := flag.NewFlagSet("collect", flag.ExitOnError)
		symbol := fs.String("symbol", "", "company symbol to collect data for")
		sources := fs.String("source", "indiasm,tickertapev2,av", "comma-separated data sources")
		_ = fs.Parse(os.Args[2:])
		if *symbol == "" {
			log.Fatal("collect requires -symbol SYMBOL")
		}
		storage := mustArchivus(settings)
		runCollect(ctx, st, storage, settings, *symbol, parseSources(*sources))
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: vyasa <command> [flags]

commands:
  seed      load EQUITY_L.csv companies into PostgreSQL (-file, default EQUITY_L.csv)
  collect   fetch data for one company: -symbol RELIANCE
            -source indiasm,tickertape,av (tickertape reads saved pages from
            tickertape_files/, use tickertapev2 for live scraping)`)
	os.Exit(2)
}

// mustStore connects to PostgreSQL using the POSTGRES_DSN secret and applies
// the schema (created when missing, see store.New).
func mustStore(ctx context.Context, settings config.Settings) *store.Store {
	dsn, err := config.RequireSecret("POSTGRES_DSN")
	if err != nil {
		log.Fatal(err)
	}
	st, err := store.New(ctx, dsn, settings.StorageSchema)
	if err != nil {
		log.Fatal(err)
	}
	return st
}

// mustArchivus builds the file-storage client from the ARCHIVUS_* secrets
// and the storage_parent_folder setting.
func mustArchivus(settings config.Settings) client.StorageClient {
	apiKey, err := config.RequireSecret("ARCHIVUS_API_KEY")
	if err != nil {
		log.Fatal(err)
	}
	baseURL, err := config.RequireSecret("ARCHIVUS_BASE_URL")
	if err != nil {
		log.Fatal(err)
	}
	driveID := config.Secret("ARCHIVUS_DRIVE_ID")
	return archivus.NewArchivusClient(apiKey, baseURL, settings.StorageParentFolder, driveID).
		WithTimeout(5 * time.Minute)
}

// runSeed parses the NSE equity master list and inserts the companies whose
// symbol is not already present, leaving existing rows untouched.
func runSeed(ctx context.Context, st *store.Store, file string) {
	cos, err := equity.ParseFile(file)
	if err != nil {
		log.Fatalf("seed: %v", err)
	}
	created, skipped, err := st.CreateMissingCompanies(ctx, cos)
	if err != nil {
		log.Fatalf("seed: %v", err)
	}
	log.Printf("[seed] %s: %d parsed, %d created, %d already present", file, len(cos), created, skipped)
}

// runCollect resolves the company and fetches data from every requested
// source sequentially (av rate-limits itself; the others are cheap).
func runCollect(ctx context.Context, st *store.Store, storage client.StorageClient, settings config.Settings, symbol string, sources []string) {
	co, err := st.GetCompany(ctx, symbol)
	if err != nil {
		log.Fatalf("collect: %v", err)
	}
	if co == nil {
		log.Fatalf("collect: symbol %q not seeded (run: vyasa seed)", symbol)
	}
	log.Printf("[collect] %s (%s): %s", co.Symbol, co.Exchange, co.Name)

	for _, name := range sources {
		src, ok := dataSources(st, storage, settings)[name]
		if !ok {
			log.Fatalf("collect: unknown source %q (want: indiasm, tickertape, tickertapev2, av)", name)
		}
		start := time.Now()
		if err := src.Execute(ctx, *co); err != nil {
			log.Printf("[collect] %s: FAILED: %v", name, err)
			continue
		}
		log.Printf("[collect] %s: done in %s", name, time.Since(start).Round(time.Second))
	}
}

// dataSources builds the requested clients. tickertape reads saved pages
// from tickertape_files/; tickertapev2 scrapes live pages with the daily cap
// from config.json.
func dataSources(st *store.Store, storage client.StorageClient, settings config.Settings) map[string]client.DataSourceClient {
	sources := make(map[string]client.DataSourceClient, 4)

	if primary := config.Secret("INDIAN_SM_API_KEY"); primary != "" || config.Secret("INDIAN_SM_API_KEY2") != "" {
		sources["indiasm"] = indiasm.New(primary, config.Secret("INDIAN_SM_API_KEY2"), storage, st)
	}
	// sources["tickertape"] = tickertape.New(storage, st)
	sources["tickertapev2"] = tickertapev2.New(settings.TickertapeMaxRequestsPerDay, storage, st)
	if key := config.Secret("ALPHAVANTAGE_API_KEY"); key != "" {
		sources["av"] = av.New(key, storage, st)
	}
	return sources
}

// parseSources splits the -source list, lower-cases it and dedupes it.
func parseSources(list string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, s := range strings.Split(list, ",") {
		s = strings.TrimSpace(strings.ToLower(s))
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
