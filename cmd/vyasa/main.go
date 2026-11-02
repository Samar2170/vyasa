// Command vyasa is the CLI entry point. It wires secrets (.env), settings
// (config.json), PostgreSQL and Archivus storage into the subcommands:
//
//	vyasa seed [-file EQUITY_L.csv]
//	vyasa collect -symbol RELIANCE [-source indiasm,tickertape,tickertapev2,av]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"vyasa/internal/equity"
	"vyasa/src/client"
	"vyasa/src/client/archivus"
	"vyasa/src/client/av"
	"vyasa/src/client/indiasm"
	"vyasa/src/client/ir"
	"vyasa/src/client/tickertape"
	"vyasa/src/client/tickertapev2"
	"vyasa/src/config"
	"vyasa/src/llm"
	_ "vyasa/src/llm/providers/openrouter"
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
		sources := fs.String("source", "indiasm,tickertapev2,av", "comma-separated data sources (tickertapev2 falls back to saved pages; tickertape reads saved pages only)")
		_ = fs.Parse(os.Args[2:])
		if *symbol == "" {
			log.Fatal("collect requires -symbol SYMBOL")
		}
		storage := mustArchivus(settings)
		runCollect(ctx, st, storage, settings, *symbol, parseSources(*sources))
	case "ir":
		fs := flag.NewFlagSet("ir", flag.ExitOnError)
		symbol := fs.String("symbol", "", "company symbol to collect IR filings for")
		_ = fs.Parse(os.Args[2:])
		if *symbol == "" {
			log.Fatal("ir requires -symbol SYMBOL")
		}
		storage := mustArchivus(settings)
		runIR(ctx, st, storage, settings, *symbol)
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: vyasa <command> [flags]

commands:
  seed      load EQUITY_L.csv companies into PostgreSQL (-file, default EQUITY_L.csv)
  collect   fetch data for one company: -symbol RELIANCE
            -source indiasm,tickertape,tickertapev2,av (tickertape reads saved
            pages from tickertape_files/ directly; tickertapev2 scrapes live pages,
            capped per day by config.json; when it fails it falls back to
            saved pages in tickertape_files/)
  ir        fetch and archive investor-relations filings (annual report,
            presentation, transcript, quarterly financials): -symbol RELIANCE`)
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
// source concurrently: each source runs in its own goroutine, so one
// source's failure (or slow rate limiting) never blocks or breaks another.
// It returns the number of failed sources.
func runCollect(ctx context.Context, st *store.Store, storage client.StorageClient, settings config.Settings, symbol string, sources []string) int {
	co, err := st.GetCompany(ctx, symbol)
	if err != nil {
		log.Fatalf("collect: %v", err)
	}
	if co == nil {
		log.Fatalf("collect: symbol %q not seeded (run: vyasa seed)", symbol)
	}
	log.Printf("[collect] %s (%s): %s", co.Symbol, co.Exchange, co.Name)

	srcs, fallbacks := dataSources(st, storage, settings)
	for _, name := range sources {
		if _, ok := srcs[name]; !ok {
			log.Fatalf("collect: unknown source %q (want: indiasm, tickertape, tickertapev2, av)", name)
		}
	}

	var mu sync.Mutex
	failed := make([]string, 0, len(sources))
	var wg sync.WaitGroup
	for _, name := range sources {
		src := srcs[name]
		wg.Add(1)
		go func(name string, src client.DataSourceClient) {
			defer wg.Done()

			start := time.Now()
			if err := src.Execute(ctx, *co); err != nil {
				if fb, ok := fallbacks[name]; ok {
					log.Printf("[collect] %s: %v — falling back to saved pages", name, err)
					if ferr := fb.Execute(ctx, *co); ferr != nil {
						log.Printf("[collect] %s fallback: FAILED: %v", name, ferr)
						mu.Lock()
						failed = append(failed, name)
						mu.Unlock()
						return
					}
					log.Printf("[collect] %s fallback: done in %s", name, time.Since(start).Round(time.Second))
					return
				}
				log.Printf("[collect] %s: FAILED: %v", name, err)
				mu.Lock()
				failed = append(failed, name)
				mu.Unlock()
				return
			}
			log.Printf("[collect] %s: done in %s", name, time.Since(start).Round(time.Second))
		}(name, src)
	}
	wg.Wait()

	sort.Strings(failed)
	log.Printf("[collect] %d sources: %d ok, %d failed", len(sources), len(sources)-len(failed), len(failed))
	return len(failed)
}

// dataSources builds the requested clients. tickertapev2 scrapes live pages
// with the daily cap from config.json; when it fails (cap reached, scrape
// error) it falls back to the file-based tickertape client reading saved
// pages from tickertape_files/.
func dataSources(st *store.Store, storage client.StorageClient, settings config.Settings) (sources, fallbacks map[string]client.DataSourceClient) {
	sources = make(map[string]client.DataSourceClient, 4)
	fallbacks = make(map[string]client.DataSourceClient, 1)

	if primary := config.Secret("INDIAN_SM_API_KEY"); primary != "" || config.Secret("INDIAN_SM_API_KEY2") != "" {
		sources["indiasm"] = indiasm.New(primary, config.Secret("INDIAN_SM_API_KEY2"), storage, st)
	}
	sources["tickertape"] = tickertape.New(storage, st)
	sources["tickertapev2"] = tickertapev2.New(settings.TickertapeMaxRequestsPerDay, storage, st)
	fallbacks["tickertapev2"] = tickertape.New(storage, st)
	if key := config.Secret("ALPHAVANTAGE_API_KEY"); key != "" {
		sources["av"] = av.New(key, storage, st)
	}
	return sources, fallbacks
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

// runIR fetches and archives the latest investor-relations filings for one
// company: the annual report, investor presentation, earnings call transcript
// and latest quarterly financials.
func runIR(ctx context.Context, st *store.Store, storage client.StorageClient, settings config.Settings, symbol string) {
	co, err := st.GetCompany(ctx, symbol)
	if err != nil {
		log.Fatalf("ir: %v", err)
	}
	if co == nil {
		log.Fatalf("ir: symbol %q not seeded (run: vyasa seed)", symbol)
	}
	log.Printf("[ir] %s (%s): %s", co.Symbol, co.Exchange, co.Name)

	model := mustLLM(settings)
	c := ir.New(storage, st, model)
	start := time.Now()
	if err := c.Execute(ctx, *co); err != nil {
		log.Fatalf("ir: %v", err)
	}
	log.Printf("[ir] %s: done in %s", co.Symbol, time.Since(start).Round(time.Second))
}

// mustLLM builds the LLM client from config.json and the provider API key
// secret (e.g. OPENROUTER_API_KEY for the default openrouter provider).
func mustLLM(settings config.Settings) llm.ModelClient {
	apiKey, err := config.RequireSecret(strings.ToUpper(settings.Provider) + "_API_KEY")
	if err != nil {
		log.Fatal(err)
	}
	model, err := llm.NewClient(settings.Provider, llm.Config{
		APIKey:       apiKey,
		DefaultModel: settings.Model,
		MaxRetries:   settings.MaxRetries,
		Timeout:      settings.Timeout(),
	})
	if err != nil {
		log.Fatal(err)
	}
	return model
}
