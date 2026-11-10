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
	"vyasa/src/models"
	"vyasa/src/rag"
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
		_ = fs.Parse(os.Args[2:])
		storage := mustArchivus(settings)
		runRAGIndex(ctx, st, storage, settings, *force)
	case "rag-search":
		fs := flag.NewFlagSet("rag-search", flag.ExitOnError)
		symbol := fs.String("symbol", "", "company symbol to search filings for")
		query := fs.String("query", "", "search query")
		segment := fs.String("segment", "", "optional segment filter (annual_report, investor_presentation, earnings_call_transcript, quarterly_financials)")
		k := fs.Int("k", 8, "max hits")
		_ = fs.Parse(os.Args[2:])
		if *symbol == "" || *query == "" {
			log.Fatal("rag-search requires -symbol SYMBOL and -query QUERY")
		}
		runRAGSearch(ctx, st, settings, *symbol, *segment, *query, *k)
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
		_ = fs.Parse(os.Args[2:])
		runRAGEval(ctx, st, settings, *dataset, *k, *strict, *symbol, *out)
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
  rag-index index archived filings (chunk, embed, store): [-force] re-index
            even already-indexed investor-relations filings
  rag-search hybrid semantic+keyword search over indexed filings:
            -symbol RELIANCE -query "capex guidance" [-segment ...] [-k 8]
  rag-eval-gen draft a retrieval-eval golden dataset from indexed chunks:
            [-symbol CSV] [-n 60] [-out dataset/rag_golden.jsonl] (review the
            draft by hand before running rag-eval on it)
  rag-eval replay a golden dataset through hybrid search and score
            Hit@k/Recall/MRR/nDCG: [-dataset dataset/rag_golden.jsonl]
            [-k 1,3,5,8,10] [-strict] [-symbol RELIANCE] [-out results.jsonl]`)
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
	}, settings.EmbeddingModel, settings.EmbeddingDim)
}

// runRAGIndex indexes archived filings: for every pending resource it
// re-extracts text from the archived PDF (so chunks get [[page N]] markers
// and refreshed section detection), chunks + embeds + stores it, uploads the
// refreshed text back to Archivus and marks the resource indexed. With
// force, investor-relations resources already indexed are re-processed too.
func runRAGIndex(ctx context.Context, st *store.Store, storage client.StorageClient, settings config.Settings, force bool) {
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
		text, err := filingText(arch, settings.StorageParentFolder, p)
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

const irSource = "investor_relations"

// filingText fetches the parsed text of an archived filing. The archived PDF
// is preferred and re-extracted so the text carries [[page N]] markers; the
// stored text file is the fallback for unreadable PDFs. A successful PDF
// re-extract is uploaded back to the same location so the archive stays
// consistent with what was indexed.
func filingText(arch *archivus.ArchivusClient, parentFolder string, p store.PendingResource) (string, error) {
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
				return text, nil
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
		return "", err
	}
	return string(data), nil
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
// prints the ranked chunks.
func runRAGSearch(ctx context.Context, st *store.Store, settings config.Settings, symbol, segment, query string, k int) {
	ix := rag.NewIndexer(st, mustEmbedder(settings), settings.EmbeddingModel, settings.EmbeddingDim)
	co, err := st.GetCompany(ctx, symbol)
	if err != nil {
		log.Fatalf("rag-search: %v", err)
	}
	if co == nil {
		log.Fatalf("rag-search: symbol %q not seeded (run: vyasa seed)", symbol)
	}
	hits, err := ix.Search(ctx, co.ID, segment, query, k)
	if err != nil {
		log.Fatalf("rag-search: %v", err)
	}
	if len(hits) == 0 {
		fmt.Println("no matching passages")
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
		if h.Section != "" {
			section = " — " + h.Section
		}
		fmt.Printf("[%d] score %.4f — %s %s%s\n%s\nsource: %s\n\n",
			i+1, h.Score, h.Symbol, label, section, h.Text, h.DocumentURL)
	}
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
// segment and symbol.
func runRAGEval(ctx context.Context, st *store.Store, settings config.Settings, dataset, ks string, strict bool, symbol, out string) {
	items, err := eval.LoadJSONL(dataset)
	if err != nil {
		log.Fatalf("rag-eval: %v", err)
	}
	cutoffs, err := parseKs(ks)
	if err != nil {
		log.Fatalf("rag-eval: %v", err)
	}
	ix := rag.NewIndexer(st, mustEmbedder(settings), settings.EmbeddingModel, settings.EmbeddingDim)

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
