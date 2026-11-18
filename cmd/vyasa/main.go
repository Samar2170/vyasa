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
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"vyasa/seed/equity"
	"vyasa/src/agent"
	"vyasa/src/client"
	"vyasa/src/client/archivus"
	"vyasa/src/client/av"
	"vyasa/src/client/indiasm"
	"vyasa/src/client/ir"
	"vyasa/src/client/tickertape"
	"vyasa/src/client/tickertapev2"
	"vyasa/src/config"
	"vyasa/src/eval"
	"vyasa/src/llm"
	"vyasa/src/llm/providers/openrouter"
	"vyasa/src/metrics"
	"vyasa/src/models"
	"vyasa/src/rag"
	"vyasa/src/research"
	"vyasa/src/session"
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
	case "rag-index":
		fs := flag.NewFlagSet("rag-index", flag.ExitOnError)
		force := fs.Bool("force", false, "re-index every archived filing, even ones already indexed")
		tables := fs.Bool("tables", true, "also extract and index financial tables from the PDFs")
		_ = fs.Parse(os.Args[2:])
		storage := mustArchivus(settings)
		runRAGIndex(ctx, st, storage, settings, *force, *tables)
	case "rag-tables":
		fs := flag.NewFlagSet("rag-tables", flag.ExitOnError)
		symbol := fs.String("symbol", "", "limit the migration to one symbol")
		dryRun := fs.Bool("dry-run", false, "list what would be processed without touching anything")
		_ = fs.Parse(os.Args[2:])
		storage := mustArchivus(settings)
		runRAGTables(ctx, st, storage, settings, *symbol, *dryRun)
	case "rag-search":
		fs := flag.NewFlagSet("rag-search", flag.ExitOnError)
		symbol := fs.String("symbol", "", "company symbol to search filings for")
		query := fs.String("query", "", "search query")
		segment := fs.String("segment", "", "optional segment filter (annual_report, investor_presentation, earnings_call_transcript, quarterly_financials)")
		k := fs.Int("k", 8, "max hits")
		rerank := fs.Bool("rerank", true, "use the cross-encoder reranker (-rerank=false to disable)")
		_ = fs.Parse(os.Args[2:])
		if *symbol == "" || *query == "" {
			log.Fatal("rag-search requires -symbol SYMBOL and -query QUERY")
		}
		runRAGSearch(ctx, st, settings, *symbol, *segment, *query, *k, *rerank, false)
	case "rag-table-search":
		fs := flag.NewFlagSet("rag-table-search", flag.ExitOnError)
		symbol := fs.String("symbol", "", "company symbol to search tables for")
		query := fs.String("query", "", "search query, e.g. 'consolidated revenue by quarter'")
		segment := fs.String("segment", "", "optional segment filter (annual_report, investor_presentation, earnings_call_transcript, quarterly_financials)")
		k := fs.Int("k", 8, "max tables")
		rerank := fs.Bool("rerank", true, "use the cross-encoder reranker (-rerank=false to disable)")
		_ = fs.Parse(os.Args[2:])
		if *symbol == "" || *query == "" {
			log.Fatal("rag-table-search requires -symbol SYMBOL and -query QUERY")
		}
		runRAGSearch(ctx, st, settings, *symbol, *segment, *query, *k, *rerank, true)
	case "metrics":
		fs := flag.NewFlagSet("metrics", flag.ExitOnError)
		symbols := fs.String("symbol", "", "comma-separated symbols (default: every symbol with statement data)")
		_ = fs.Parse(os.Args[2:])
		runMetrics(ctx, st, parseSymbols(*symbols))
	case "rag-eval-gen":
		fs := flag.NewFlagSet("rag-eval-gen", flag.ExitOnError)
		symbols := fs.String("symbol", "", "comma-separated symbols to sample (default: every indexed symbol)")
		n := fs.Int("n", 60, "approximate number of questions to generate")
		out := fs.String("out", "dataset/rag_golden.jsonl", "output JSONL path")
		_ = fs.Parse(os.Args[2:])
		runRAGEvalGen(ctx, st, settings, parseSources(*symbols), *n, *out)
	case "rag-eval":
		fs := flag.NewFlagSet("rag-eval", flag.ExitOnError)
		dataset := fs.String("dataset", "dataset/rag_golden.jsonl", "golden dataset JSONL")
		k := fs.String("k", "1,3,5,8,10", "comma-separated k cutoffs")
		strict := fs.Bool("strict", false, "score exact ordinals only (no +-1 neighbour tolerance)")
		symbol := fs.String("symbol", "", "optional symbol filter")
		out := fs.String("out", "", "optional path for per-item results JSONL")
		rerank := fs.Bool("rerank", true, "use the cross-encoder reranker (-rerank=false to disable)")
		budget := fs.Int("rerank-budget", settings.RerankBudget, "max cache-miss rerank pairs this run may bill (0 = unlimited)")
		_ = fs.Parse(os.Args[2:])
		runRAGEval(ctx, st, settings, *dataset, *k, *strict, *symbol, *out, *rerank, *budget)
	case "research":
		fs := flag.NewFlagSet("research", flag.ExitOnError)
		symbol := fs.String("symbol", "", "company symbol to research")
		out := fs.String("out", "reports", "directory for the markdown report")
		steps := fs.Int("steps", 0, "optional per-agent step-limit override")
		stream := fs.Bool("stream", true, "stream agent text deltas to the terminal (-stream=false to disable)")
		quiet := fs.Bool("quiet", false, "suppress the live agent trace (session logging to PostgreSQL still runs)")
		budget := fs.Float64("budget", settings.ResearchCostBudget, "cost budget for the whole workflow in USD (0 = unlimited; default from config.json research_cost_budget)")
		_ = fs.Parse(os.Args[2:])
		if *symbol == "" {
			log.Fatal("research requires -symbol SYMBOL")
		}
		storage := mustArchivus(settings)
		runResearch(ctx, st, storage, settings, *symbol, *out, *steps, *stream, *quiet, *budget)
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
            presentation, transcript, quarterly financials): -symbol RELIANCE
  metrics   compute per-FY key metrics from the annual statement tables and
            store them in ticker_metrics: [-symbol CSV] (default: every
            symbol with statement data)
  rag-index index archived filings (chunk, embed, store), plus financial
            tables into the separate rag_tables store [-tables=false]:
            [-force] re-index even already-indexed investor-relations filings
  rag-tables backfill financial tables for filings archived before table
            extraction existed (downloads each PDF, mines tables, indexes
            them, archives the JSON): [-symbol SYM] [-dry-run]
  rag-search hybrid semantic+keyword search over indexed filings, reranked
            by a cross-encoder: -symbol RELIANCE -query "capex guidance"
            [-segment ...] [-k 8] [-rerank=false]
  rag-table-search the same over the extracted financial tables: -symbol
            RELIANCE -query "consolidated revenue by quarter" [-segment ...]
            [-k 8] [-rerank=false]
  rag-eval-gen draft a retrieval-eval golden dataset from indexed chunks:
            [-symbol CSV] [-n 60] [-out dataset/rag_golden.jsonl] (review the
            draft by hand before running rag-eval on it)
  rag-eval replay a golden dataset through hybrid search and score
            Hit@k/Recall/MRR/nDCG: [-dataset dataset/rag_golden.jsonl]
            [-k 1,3,5,8,10] [-strict] [-symbol RELIANCE] [-out results.jsonl]
            [-rerank=false] [-rerank-budget 2000]
  research  run the equity research workflow for one company (metrics
            analysis, annual report, investor presentation, synthesis with
            clarifying Q&A; saves the markdown report to DB, file and
            Archivus). The whole agent trace streams to the terminal and is
            persisted to the session_logs table: -symbol RELIANCE
            [-out reports] [-steps N] [-stream=false] [-quiet] [-budget N]`)
	os.Exit(2)
}

// mustStore connects to PostgreSQL using the POSTGRES_DSN secret and applies
// the schema (created when missing, see store.New).
func mustStore(ctx context.Context, settings config.Settings) *store.Store {
	dsn, err := config.RequireSecret("POSTGRES_DSN")
	if err != nil {
		log.Fatal(err)
	}
	st, err := store.New(ctx, dsn, settings.StorageSchema, store.WithEmbeddingDim(settings.EmbeddingDim))
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
	indexer := rag.NewIndexer(st, mustEmbedder(settings), settings.EmbeddingModel, settings.EmbeddingDim)

	c := ir.New(storage, st, settings.Provider, model, indexer)
	start := time.Now()
	if err := c.Execute(ctx, *co); err != nil {
		log.Fatalf("ir: %v", err)
	}
	log.Printf("[ir] %s: done in %s", co.Symbol, time.Since(start).Round(time.Second))
}

// mustLLM builds the LLM configuration from config.json and the provider
// API key secret (e.g. OPENROUTER_API_KEY for the default openrouter
// provider). The ir client posts its prompts to the provider's
// chat-completions endpoint with it.
func mustLLM(settings config.Settings) llm.Config {
	apiKey, err := config.RequireSecret(strings.ToUpper(settings.Provider) + "_API_KEY")
	if err != nil {
		log.Fatal(err)
	}
	model := settings.Model
	if model == "" {
		model = openrouter.DefaultModel
	}
	return llm.Config{
		APIKey:       apiKey,
		DefaultModel: model,
		MaxRetries:   settings.MaxRetries,
		Timeout:      settings.Timeout(),
	}
}

// mustEmbedder builds the embeddings client from the provider API key and
// the embedding_model / embedding_dim settings.
func mustEmbedder(settings config.Settings) *llm.Embedder {
	apiKey, err := config.RequireSecret(strings.ToUpper(settings.Provider) + "_API_KEY")
	if err != nil {
		log.Fatal(err)
	}
	return llm.NewEmbedder(llm.Config{
		APIKey:     apiKey,
		MaxRetries: settings.MaxRetries,
		Timeout:    settings.Timeout(),
	}, settings.EmbeddingModel, settings.EmbeddingDim).
		WithProvider(providerOrder(settings.EmbeddingProvider))
}

// providerOrder splits a comma-separated OpenRouter provider routing order
// (nil when empty) for the embeddings "provider" request field.
func providerOrder(s string) []string {
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mustReranker builds the cross-encoder rerank client from the provider API
// key and the rerank_model / rerank_pool settings, backed by a persistent
// Postgres rerank-score cache. budget (nil = uncapped) limits the cache-miss
// pairs the API may bill. Returns nil when rerank_enabled is false.
func mustReranker(ctx context.Context, st *store.Store, settings config.Settings, budget *llm.RerankBudget) *llm.Reranker {
	if !settings.RerankOn() {
		return nil
	}
	apiKey, err := config.RequireSecret(strings.ToUpper(settings.Provider) + "_API_KEY")
	if err != nil {
		log.Fatal(err)
	}
	return llm.NewReranker(llm.Config{
		APIKey:     apiKey,
		MaxRetries: settings.MaxRetries,
		Timeout:    settings.Timeout(),
	}, settings.RerankModel).WithCache(st).WithBudget(budget)
}

// runRAGIndex indexes archived filings: for every pending resource it
// re-extracts text from the archived PDF (so chunks get [[page N]] markers
// and refreshed section detection), chunks + embeds + stores it, uploads the
// refreshed text back to Archivus and marks the resource indexed. With
// tables, the PDF is also mined for financial tables which are indexed into
// the separate rag_tables store. With force, investor-relations resources
// already indexed are re-processed too.
func runRAGIndex(ctx context.Context, st *store.Store, storage client.StorageClient, settings config.Settings, force, tables bool) {
	arch, ok := storage.(*archivus.ArchivusClient)
	if !ok {
		log.Fatal("rag-index: storage client does not support file lookup")
	}
	ix := rag.NewIndexer(st, mustEmbedder(settings), settings.EmbeddingModel, settings.EmbeddingDim)

	if force {
		if err := st.ResetResourcesIndexed(ctx, irSource); err != nil {
			log.Fatalf("rag-index: %v", err)
		}
	}

	pending, err := st.UnindexedResources(ctx)
	if err != nil {
		log.Fatalf("rag-index: %v", err)
	}
	if len(pending) == 0 {
		log.Printf("[rag-index] every archived filing is indexed")
		return
	}

	var failed int
	for _, p := range pending {
		start := time.Now()
		text, pdfBytes, err := filingText(arch, settings.StorageParentFolder, p)
		if err != nil {
			log.Printf("[rag-index] %s: %s: fetch %s: %v", p.Symbol, p.AnalysisSegment, p.RawDataUrl, err)
			failed++
			continue
		}
		co := models.Company{BaseModel: models.BaseModel{ID: p.CompanyID}, Symbol: p.Symbol}
		meta := rag.DocMeta{
			Segment:     p.AnalysisSegment,
			DocumentURL: p.DocumentURL,
			Period:      rag.DerivePeriod(p.AnchorText, p.DocumentURL, rag.TextHead(text, 4000)),
			Source:      p.Source,
			IsQuarterly: p.AnalysisSegment == "quarterly_financials",
		}
		n, err := ix.IndexDocument(ctx, co, meta, text)
		if err != nil {
			log.Printf("[rag-index] %s: %s: FAILED: %v", p.Symbol, p.AnalysisSegment, err)
			failed++
			continue
		}
		if tables && len(pdfBytes) > 0 {
			if tn, err := indexFilingTables(ctx, arch, st, ix, settings.StorageParentFolder, co, meta, p, pdfBytes); err != nil {
				log.Printf("[rag-index] %s: %s: tables: %v", p.Symbol, p.AnalysisSegment, err)
			} else if tn > 0 {
				log.Printf("[rag-index] %s: %s: indexed %d tables (period %s)", p.Symbol, p.AnalysisSegment, tn, orDash(meta.Period))
			}
		}
		if err := st.MarkResourceIndexed(ctx, p.CompanyID, p.AnalysisSegment, true); err != nil {
			log.Printf("[rag-index] %s: %s: mark indexed: %v", p.Symbol, p.AnalysisSegment, err)
			failed++
			continue
		}
		log.Printf("[rag-index] %s: %s: indexed %d chunks (period %s) in %s",
			p.Symbol, p.AnalysisSegment, n, orDash(meta.Period), time.Since(start).Round(time.Second))
	}
	log.Printf("[rag-index] %d resources: %d ok, %d failed", len(pending), len(pending)-failed, failed)
}

// indexFilingTables mines the archived PDF for financial tables, indexes
// them into rag_tables and archives the JSON beside the source PDF so the
// extraction is reusable without re-parsing. Returns the number of indexed
// table units (0 when the PDF holds no tables).
func indexFilingTables(ctx context.Context, arch *archivus.ArchivusClient, st *store.Store, ix *rag.Indexer, parentFolder string, co models.Company, meta rag.DocMeta, p store.PendingResource, pdfBytes []byte) (int, error) {
	tables := ir.ExtractedTables(pdfBytes)
	if len(tables) == 0 {
		return 0, nil
	}
	n, err := ix.IndexTables(ctx, co, meta, tables)
	if err != nil {
		return 0, err
	}
	if p.RawDataUrl != "" {
		name := tablesFileName(p.RawDataUrl)
		payload, jerr := ir.TablesJSON(p.AnalysisSegment, p.DocumentURL, tables)
		if jerr != nil {
			log.Printf("[rag-index] %s: %s: encode tables: %v", p.Symbol, p.AnalysisSegment, jerr)
			return n, nil
		}
		if _, uerr := arch.UploadFile(folderRel(parentFolder, p.RawDataUrl),
			[]*archivus.UploadFile{{Name: name, Content: payload}}); uerr != nil {
			log.Printf("[rag-index] %s: %s: tables upload: %v", p.Symbol, p.AnalysisSegment, uerr)
		} else if rerr := st.RecordResourceTables(ctx, p.CompanyID, p.AnalysisSegment, name,
			path.Join(folderRel(parentFolder, p.RawDataUrl), name), len(tables)); rerr != nil {
			log.Printf("[rag-index] %s: %s: record tables path: %v", p.Symbol, p.AnalysisSegment, rerr)
		}
	}
	return n, nil
}

// tablesFileName derives the archived tables JSON name from the PDF's file
// name.
func tablesFileName(pdfPath string) string {
	name := fileNameOf(pdfPath)
	if name == "" {
		return ""
	}
	return strings.TrimSuffix(name, ".pdf") + ".tables.json"
}

const irSource = "investor_relations"

// runRAGTables migrates existing companies to the rag_tables store: every
// archived filing without a tables file is re-downloaded, mined for
// financial tables, indexed (embed + insert) and its JSON archived beside
// the source PDF. Prose chunks and the indexed flags are left untouched, so
// the migration never re-bills embeddings for chunks. Idempotent: the
// extracted_data.tables_path it records marks a filing as done.
func runRAGTables(ctx context.Context, st *store.Store, storage client.StorageClient, settings config.Settings, symbol string, dryRun bool) {
	arch, ok := storage.(*archivus.ArchivusClient)
	if !ok {
		log.Fatal("rag-tables: storage client does not support file lookup")
	}
	ix := rag.NewIndexer(st, mustEmbedder(settings), settings.EmbeddingModel, settings.EmbeddingDim)

	pending, err := st.TableBackfillResources(ctx, symbol)
	if err != nil {
		log.Fatalf("rag-tables: %v", err)
	}
	if len(pending) == 0 {
		log.Printf("[rag-tables] every archived filing already has its tables indexed")
		return
	}
	log.Printf("[rag-tables] %d filings to migrate", len(pending))
	if dryRun {
		for _, p := range pending {
			log.Printf("[rag-tables] would process %s: %s (%s)", p.Symbol, p.AnalysisSegment, p.RawDataUrl)
		}
		return
	}

	var failed int
	for _, p := range pending {
		start := time.Now()
		data, err := downloadArchivedFile(arch, settings.StorageParentFolder, p.RawDataUrl)
		if err != nil {
			log.Printf("[rag-tables] %s: %s: fetch %s: %v", p.Symbol, p.AnalysisSegment, p.RawDataUrl, err)
			failed++
			continue
		}
		co := models.Company{BaseModel: models.BaseModel{ID: p.CompanyID}, Symbol: p.Symbol}
		meta := rag.DocMeta{
			Segment:     p.AnalysisSegment,
			DocumentURL: p.DocumentURL,
			Period:      rag.DerivePeriod(p.AnchorText, p.DocumentURL, ""),
			Source:      p.Source,
			IsQuarterly: p.AnalysisSegment == "quarterly_financials",
		}
		n, err := indexFilingTables(ctx, arch, st, ix, settings.StorageParentFolder, co, meta, p, data)
		if err != nil {
			log.Printf("[rag-tables] %s: %s: FAILED: %v", p.Symbol, p.AnalysisSegment, err)
			failed++
			continue
		}
		outcome := "no tables found"
		if n > 0 {
			outcome = fmt.Sprintf("indexed %d table units", n)
		}
		log.Printf("[rag-tables] %s: %s: %s in %s", p.Symbol, p.AnalysisSegment, outcome, time.Since(start).Round(time.Second))
	}
	log.Printf("[rag-tables] %d filings: %d ok, %d failed", len(pending), len(pending)-failed, failed)
}

// filingText fetches the parsed text of an archived filing. The archived PDF
// is preferred and re-extracted so the text carries [[page N]] markers; the
// stored text file is the fallback for unreadable PDFs. A successful PDF
// re-extract is uploaded back to the same location so the archive stays
// consistent with what was indexed. The PDF bytes are returned alongside the
// text (nil when only the stored text was available) so table extraction can
// mine the same file.
func filingText(arch *archivus.ArchivusClient, parentFolder string, p store.PendingResource) (string, []byte, error) {
	if p.RawDataUrl != "" {
		data, err := downloadArchivedFile(arch, parentFolder, p.RawDataUrl)
		if err == nil {
			text, pages := ir.ExtractPDFText(data)
			if pages > 0 && strings.TrimSpace(text) != "" {
				if txtName := fileNameOf(p.TextPath); txtName != "" {
					if _, uerr := arch.UploadFile(folderRel(parentFolder, p.TextPath),
						[]*archivus.UploadFile{{Name: txtName, Content: []byte(text)}}); uerr != nil {
						log.Printf("[rag-index] %s: %s: refresh text upload: %v", p.Symbol, p.AnalysisSegment, uerr)
					}
				}
				return text, data, nil
			}
			log.Printf("[rag-index] %s: %s: PDF re-extract empty (%d pages), falling back to stored text",
				p.Symbol, p.AnalysisSegment, pages)
		} else {
			log.Printf("[rag-index] %s: %s: PDF fetch failed (%v), falling back to stored text",
				p.Symbol, p.AnalysisSegment, err)
		}
	}
	data, err := downloadArchivedFile(arch, parentFolder, p.TextPath)
	if err != nil {
		return "", nil, err
	}
	return string(data), nil, nil
}

// downloadArchivedFile fetches one archived file from Archivus. Stored paths
// are absolute under the parent folder; LookupFile resolves against the
// parent, so the prefix is stripped first.
func downloadArchivedFile(arch *archivus.ArchivusClient, parentFolder, storedPath string) ([]byte, error) {
	dir, name := folderRel(parentFolder, storedPath), fileNameOf(storedPath)
	info, err := arch.LookupFile(dir, name)
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, fmt.Errorf("archivus: %s not found", storedPath)
	}
	data, _, err := arch.DownloadFile(info.ID)
	return data, err
}

// folderRel converts a stored absolute-under-parent path to the
// parent-relative folder LookupFile/UploadFile expect.
func folderRel(parentFolder, storedPath string) string {
	return strings.TrimPrefix(folderOf(storedPath), parentFolder+"/")
}

// fileNameOf returns the file name of a stored path.
func fileNameOf(storedPath string) string {
	_, name := path.Split(storedPath)
	return name
}

// folderOf splits the directory from a stored path.
func folderOf(storedPath string) string {
	i := strings.LastIndexByte(storedPath, '/')
	if i < 0 {
		return ""
	}
	return storedPath[:i]
}

// orDash renders empty strings as a dash in logs and output.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// runRAGSearch runs a hybrid search over one company's indexed filings and
// prints the ranked hits — prose chunks by default, or the extracted
// financial tables with -tables. With rerank on, the cross-encoder reorders
// a deep candidate pool before truncation.
func runRAGSearch(ctx context.Context, st *store.Store, settings config.Settings, symbol, segment, query string, k int, rerank, tables bool) {
	ix := rag.NewIndexer(st, mustEmbedder(settings), settings.EmbeddingModel, settings.EmbeddingDim)
	if rerank {
		ix = ix.WithReranker(mustReranker(ctx, st, settings, nil), settings.RerankPool)
	}
	co, err := st.GetCompany(ctx, symbol)
	if err != nil {
		log.Fatalf("rag-search: %v", err)
	}
	if co == nil {
		log.Fatalf("rag-search: symbol %q not seeded (run: vyasa seed)", symbol)
	}
	var hits []models.RAGHit
	if tables {
		hits, err = ix.SearchTables(ctx, co.ID, segment, query, k)
	} else {
		hits, err = ix.Search(ctx, co.ID, segment, query, k)
	}
	if err != nil {
		log.Fatalf("rag-search: %v", err)
	}
	if len(hits) == 0 {
		if tables {
			fmt.Println("no matching tables")
		} else {
			fmt.Println("no matching passages")
		}
		return
	}
	for i, h := range hits {
		label := h.Segment
		if h.Period != "" {
			label += " " + h.Period
		}
		if h.IsQuarterly {
			label += " [quarterly]"
		}
		section := ""
		if tables {
			caption := h.Section
			if caption == "" {
				caption = "untitled table"
			}
			section = fmt.Sprintf(" — table %q, page %d", caption, h.Offset)
		} else if h.Section != "" {
			section = " — " + h.Section
		}
		score := fmt.Sprintf("score %.4f", h.Score)
		if h.Reranked {
			score = fmt.Sprintf("rerank %.4f (rrf %.4f)", h.RerankScore, h.Score)
		}
		text := h.Text
		if tables {
			text = truncateRunesCmd(text, tableHitRunes)
		}
		fmt.Printf("[%d] %s — %s %s%s\n%s\nsource: %s\n\n",
			i+1, score, h.Symbol, label, section, text, h.DocumentURL)
	}
}

// tableHitRunes caps how much of a table's markdown the CLI may print.
const tableHitRunes = 1600

// truncateRunesCmd cuts s to at most n runes at a word boundary.
func truncateRunesCmd(s string, n int) string {
	r := []rune(s)
	if n <= 0 || len(r) <= n {
		return s
	}
	cut := string(r[:n])
	if i := strings.LastIndexAny(cut, " \n\t"); i > n/2 {
		return strings.TrimRight(cut[:i], " \n\t")
	}
	return cut
}

// parseSymbols splits the -symbol list, trims it and dedupes it. Unlike
// parseSources the values keep their case: symbols are case-sensitive
// (M&M, WAAREEENER).
func parseSymbols(list string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, s := range strings.Split(list, ",") {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// runMetrics derives the per-FY key metrics from the latest annual statement
// snapshots and upserts them into ticker_metrics. With no -symbol list it
// recomputes every symbol that has statement data.
func runMetrics(ctx context.Context, st *store.Store, symbols []string) {
	if len(symbols) == 0 {
		var err error
		symbols, err = st.MetricsSymbols(ctx)
		if err != nil {
			log.Fatalf("metrics: %v", err)
		}
		if len(symbols) == 0 {
			log.Printf("[metrics] no statement data to compute from (run: vyasa collect)")
			return
		}
		log.Printf("[metrics] %d symbols with statement data", len(symbols))
	}

	var okCount, skipped, failed int
	for _, sym := range symbols {
		co, err := st.GetCompany(ctx, sym)
		if err != nil {
			log.Fatalf("metrics: %v", err)
		}
		if co == nil {
			co = &models.Company{Symbol: sym}
		}
		rows, err := metrics.Compute(ctx, st, *co)
		if err != nil {
			log.Printf("[metrics] %s: FAILED: %v", sym, err)
			failed++
			continue
		}
		if len(rows) == 0 {
			log.Printf("[metrics] %s: no statement data, skipping", sym)
			skipped++
			continue
		}
		if err := st.UpsertMetrics(ctx, rows); err != nil {
			log.Printf("[metrics] %s: FAILED: %v", sym, err)
			failed++
			continue
		}
		gapped := 0
		for _, r := range rows {
			if len(r.DataGaps) > 0 {
				gapped++
			}
		}
		log.Printf("[metrics] %s: %d FYs stored (%d with gaps)", sym, len(rows), gapped)
		okCount++
	}
	log.Printf("[metrics] %d symbols: %d ok, %d skipped, %d failed", len(symbols), okCount, skipped, failed)
}

// runRAGEvalGen drafts the golden retrieval-eval dataset: it samples indexed
// chunks and asks the model to write one grounded question per chunk. The
// draft is meant to be reviewed and curated by hand before rag-eval uses it.
func runRAGEvalGen(ctx context.Context, st *store.Store, settings config.Settings, symbols []string, n int, out string) {
	client, err := llm.NewClient(settings.Provider, mustLLM(settings))
	if err != nil {
		log.Fatalf("rag-eval-gen: %v", err)
	}
	if _, err := eval.Generate(ctx, st, client, eval.GenOptions{
		Symbols: symbols,
		Total:   n,
		Workers: 3,
	}, out); err != nil {
		log.Fatalf("rag-eval-gen: %v", err)
	}
}

// runRAGEval replays the golden dataset through the production hybrid search
// and prints Hit@k / Recall@k / MRR / nDCG, overall and broken down by
// segment and symbol. With rerank on, the cross-encoder reorders the
// candidate pool before truncation; budgetPairs > 0 caps the cache-miss
// pairs the sweep may bill, with pool-over-budget items keeping RRF order.
func runRAGEval(ctx context.Context, st *store.Store, settings config.Settings, dataset, ks string, strict bool, symbol, out string, rerank bool, budgetPairs int) {
	items, err := eval.LoadJSONL(dataset)
	if err != nil {
		log.Fatalf("rag-eval: %v", err)
	}
	cutoffs, err := parseKs(ks)
	if err != nil {
		log.Fatalf("rag-eval: %v", err)
	}
	var budget *llm.RerankBudget
	if budgetPairs > 0 {
		budget = llm.NewRerankBudget(budgetPairs)
	}
	ix := rag.NewIndexer(st, mustEmbedder(settings), settings.EmbeddingModel, settings.EmbeddingDim)
	if rerank {
		ix = ix.WithReranker(mustReranker(ctx, st, settings, budget), settings.RerankPool)
	}

	opts := eval.RunOptions{Ks: cutoffs, Window: 1, Segment: ""}
	if strict {
		opts.Window = 0
	}
	if symbol != "" {
		opts.Symbols = map[string]bool{strings.ToUpper(strings.TrimSpace(symbol)): true}
	}
	rep, err := eval.Run(ctx, ix, st, items, opts)
	if err != nil {
		log.Fatalf("rag-eval: %v", err)
	}
	rep.Print(os.Stdout)
	if budget != nil {
		log.Printf("[rag-eval] rerank budget: spent %d pairs, %d pools skipped, %d/%d left",
			budget.Spent(), budget.Skipped(), budget.Left(), budgetPairs)
	}
	if out != "" {
		if err := rep.SaveResults(out); err != nil {
			log.Fatalf("rag-eval: %v", err)
		}
		log.Printf("[rag-eval] per-item results written to %s", out)
	}
	if rep.Failed > 0 {
		log.Printf("[rag-eval] warning: %d items skipped (%s)", rep.Failed, "symbol not seeded")
	}
}

// parseKs parses the -k flag into sorted positive cutoffs.
func parseKs(spec string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, err := strconv.Atoi(part)
		if err != nil || k <= 0 {
			return nil, fmt.Errorf("invalid k %q", part)
		}
		out = append(out, k)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no valid k in %q", spec)
	}
	sort.Ints(out)
	return out, nil
}

// runResearch executes the equity research workflow for one company. The
// whole agent trace (runs, steps, tool calls, streamed text, usage) is
// persisted to session_logs under one session id and rendered on the
// terminal; the report plus where it was persisted is printed at the end.
func runResearch(ctx context.Context, st *store.Store, storage client.StorageClient, settings config.Settings, symbol, outDir string, steps int, stream, quiet bool, budgetUSD float64) {
	ai, err := llm.NewClient(settings.Provider, mustLLM(settings))
	if err != nil {
		log.Fatalf("research: %v", err)
	}
	ix := rag.NewIndexer(st, mustEmbedder(settings), settings.EmbeddingModel, settings.EmbeddingDim)
	if settings.RerankOn() {
		ix = ix.WithReranker(mustReranker(ctx, st, settings, nil), settings.RerankPool)
	}

	sess := session.New(ctx, st, map[string]any{
		"command":    "research",
		"symbol":     symbol,
		"model":      settings.Model,
		"stream":     stream,
		"budget_usd": budgetUSD,
	})
	summary := map[string]any{"command": "research", "symbol": symbol, "model": settings.Model}

	sinks := []agent.EventSink{sess.Sink()}
	if !quiet {
		sinks = append(sinks, session.TerminalSink())
	}

	res, err := research.Run(ctx, research.Deps{
		St:            st,
		Client:        ai,
		Indexer:       ix,
		Storage:       storage,
		Model:         settings.Model,
		ParentFolder:  settings.StorageParentFolder,
		StepLimit:     steps,
		Stream:        stream,
		Sink:          session.Tee(sinks...),
		CostBudgetUSD: budgetUSD,
	}, symbol)
	if err != nil {
		summary["status"] = string(agent.FinishError)
		summary["error"] = err.Error()
		sess.Close(summary)
		log.Fatalf("research: %v", err)
	}

	summary["status"] = string(res.Finish)
	summary["steps"] = res.Steps
	summary["usage"] = session.UsageSummary(res.Usage)
	summary["report_id"] = res.Report.ID
	summary["report_file"] = res.FilePath
	if res.Report.ArchivedPath != "" {
		summary["report_archived"] = res.Report.ArchivedPath
	}
	if res.BudgetLimit > 0 {
		summary["budget_spent_usd"] = res.BudgetSpent
	}
	if len(res.Warnings) > 0 {
		summary["warnings"] = res.Warnings
	}
	sess.Close(summary)

	log.Printf("[research] session %s persisted (%s)", sess.ID(), summary["status"])
	log.Printf("[research] %s: report id %s, %d question(s) asked", symbol, res.Report.ID, len(res.Report.Questions))
	log.Printf("[research] markdown: %s", res.FilePath)
	if res.Report.ArchivedPath != "" {
		log.Printf("[research] archived: %s", res.Report.ArchivedPath)
	}
	log.Printf("[research] usage: %d steps, in %d tok, out %d tok, cached %d tok",
		res.Steps, res.Usage.InputTokens, res.Usage.OutputTokens, res.Usage.CachedTokens)
	if res.Usage.Cost != nil {
		log.Printf("[research] cost: $%.4f", *res.Usage.Cost)
	}
	if res.BudgetLimit > 0 {
		log.Printf("[research] cost budget: $%.4f spent of $%.4f (exhausted: %v)",
			res.BudgetSpent, res.BudgetLimit, res.BudgetExhausted)
	}
	for _, w := range res.Warnings {
		log.Printf("[research] warning: %s", w)
	}
	if res.Finish != agent.FinishCompleted {
		log.Printf("[research] warning: synthesizer finished with %s — report may be partial", res.Finish)
	}
	fmt.Println()
	fmt.Println(res.Report.ReportMD)
}
