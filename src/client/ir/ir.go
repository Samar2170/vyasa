// Package ir fetches a company's investor-relations filings — the latest
// annual report, investor presentation, earnings call transcript (when
// published) and latest quarterly financial results — from the company's
// investor relations website, archives the original PDFs to Archivus storage
// under <parent>/<SYMBOL>/ir_filings, then parses each PDF to plain text and
// archives the text file in the same location.
//
// The official website and investor relations link are resolved once (stored
// links first, then the LLM agent, then a DuckDuckGo fallback) and persisted
// to the company_links table. For every archived filing one document is
// upserted into the company_research_resources table, keyed by
// (company_id, analysis_segment), holding the source link, the archived PDF
// path and the parsed-text path.
package ir

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"vyasa/src/client"
	"vyasa/src/client/archivus"
	"vyasa/src/llm"
	"vyasa/src/models"
	"vyasa/src/rag"
	"vyasa/src/store"
)

// Analysis segments persisted to company_research_resources.
const (
	SegAnnualReport = "annual_report"
	SegPresentation = "investor_presentation"
	SegTranscript   = "earnings_call_transcript"
	SegQuarterly    = "quarterly_financials"
)

const (
	storageSubFolder = "ir_filings"
	sourceIR         = "investor_relations"

	maxDocBytes        = 100 << 20 // refuse PDFs above 100 MB
	maxPageBytes       = 2 << 20   // bytes of HTML read per page
	maxSubPages        = 8         // extra IR sub-pages crawled for candidates
	maxCandidatesLLM   = 80        // link list size offered to the LLM agent
	maxTriesPerSeg     = 3         // download attempts per segment
	maxListings        = 12        // listing/filter pages expanded per segment
	maxListingPages    = 3         // record pages fetched from a listing API
	listingRecordLimit = 100       // records requested per listing API page
	httpTimeout        = 3 * time.Minute
	dayFormat          = "2006-01-02"
)

var _ client.DataSourceClient = (*Client)(nil)

// segmentOrder is the stable processing/reporting order of the segments.
var segmentOrder = []string{SegAnnualReport, SegPresentation, SegTranscript, SegQuarterly}

type Client struct {
	storage client.StorageClient
	store   *store.Store
	llm     *llmChat
	hc      *http.Client
	indexer *rag.Indexer
}

// New creates the client. When cfg carries no API key the LLM chat handle is
// nil and discovery relies on stored links and keyword matching only.
func New(storage client.StorageClient, st *store.Store, provider string, cfg llm.Config, indexer *rag.Indexer) *Client {
	return &Client{
		storage: storage,
		store:   st,
		llm:     newLLMChat(provider, cfg),
		hc:      &http.Client{Timeout: httpTimeout},
		indexer: indexer,
	}
}

// candidate is one link harvested from the investor relations site.
type candidate struct {
	URL     string // absolute document/page URL
	Text    string // anchor text
	PageURL string // page the link was found on
}

// filing is a classified candidate picked for download.
type filing struct {
	Segment string
	URL     string
	Text    string
	PageURL string
}

// filingResult is one downloaded, archived and parsed filing.
type filingResult struct {
	Segment   string
	PDFName   string
	TxtName   string
	SourceURL string
	PageURL   string
	Anchor    string
	Pages     int
	TextChars int
	PDF       []byte
	Text      string
}

// Execute runs the whole pipeline for one company: resolve links, discover
// filings, download, archive the PDFs, parse them and archive the text.
func (c *Client) Execute(ctx context.Context, co models.Company) error {
	links, err := c.resolveLinks(ctx, co)
	if err != nil {
		return fmt.Errorf("ir: %s: %w", co.Symbol, err)
	}

	cands, err := c.collectCandidates(ctx, links.IR)
	if err != nil {
		return fmt.Errorf("ir: %s: %w", co.Symbol, err)
	}
	picks := selectFilings(cands)
	listings := map[string][]filing{}
	var listingsMu sync.Mutex
	picks = c.expandListings(ctx, co, picks, listings, &listingsMu)
	if len(picks) == 0 {
		return fmt.Errorf("ir: %s: no filings found on %s", co.Symbol, links.IR)
	}
	if missing := missingSegments(picks); len(missing) > 0 {
		picks = c.llmAssist(ctx, co, cands, picks, missing)
		picks = c.expandListings(ctx, co, picks, listings, &listingsMu)
	}

	results := c.fetchAndParse(ctx, co, picks, listings, &listingsMu)
	if len(results) == 0 {
		return fmt.Errorf("ir: %s: no filings could be downloaded", co.Symbol)
	}
	if err := c.archiveAndRecord(ctx, co, results); err != nil {
		return fmt.Errorf("ir: %s: %w", co.Symbol, err)
	}
	return nil
}

// fetchAndParse downloads the picked PDFs and extracts plain text from each,
// one worker per segment.
func (c *Client) fetchAndParse(ctx context.Context, co models.Company, picks map[string][]filing, listings map[string][]filing, listingsMu *sync.Mutex) []filingResult {
	day := time.Now()
	var wg sync.WaitGroup
	results := make([]filingResult, len(segmentOrder))

	for i, seg := range segmentOrder {
		list := picks[seg]
		if len(list) == 0 {
			continue
		}
		if len(list) > maxTriesPerSeg {
			list = list[:maxTriesPerSeg]
		}

		wg.Add(1)
		go func(i int, seg string, list []filing) {
			defer wg.Done()

			var data []byte
			var used filing
			for _, f := range list {
				body, got, err := c.pickDownload(ctx, co, seg, f, listings, listingsMu)
				if err != nil {
					log.Printf("[ir] %s: %s: download %s: %v", co.Symbol, seg, f.URL, err)
					continue
				}
				data, used = body, got
				break
			}
			if data == nil {
				return
			}
			log.Printf("[ir] %s: %s: downloaded %s (%d bytes)", co.Symbol, seg, used.URL, len(data))

			text, pages := extractPDFText(data)
			if pages == 0 {
				log.Printf("[ir] %s: %s: pdf at %s has no readable text", co.Symbol, seg, used.URL)
			}
			results[i] = filingResult{
				Segment:   seg,
				PDFName:   docName(co.Symbol, seg, "pdf", day),
				TxtName:   docName(co.Symbol, seg, "txt", day),
				SourceURL: used.URL,
				PageURL:   used.PageURL,
				Anchor:    used.Text,
				Pages:     pages,
				TextChars: len(text),
				PDF:       data,
				Text:      text,
			}
		}(i, seg, list)
	}
	wg.Wait()

	results = slices.DeleteFunc(results, func(r filingResult) bool { return r.PDF == nil })
	return results
}

// pickDownload downloads filing f. When f turns out to be an HTML page
// (a listing rather than a document) it goes one step further: the documents
// offered on that page are harvested and tried, so a listing picked for
// download still yields a filing. Harvests share the expandListings cache.
func (c *Client) pickDownload(ctx context.Context, co models.Company, seg string, f filing, listings map[string][]filing, listingsMu *sync.Mutex) ([]byte, filing, error) {
	body, err := c.download(ctx, f.URL, f.PageURL)
	if err == nil {
		return body, f, nil
	}
	if !errors.Is(err, errNotPDF) {
		return nil, filing{}, err
	}

	docs := c.expandedListing(ctx, co, seg, f, listings, listingsMu)
	if len(docs) > maxTriesPerSeg {
		docs = docs[:maxTriesPerSeg]
	}
	for _, g := range docs {
		log.Printf("[ir] %s: %s: %s is a listing page, trying %s linked on it", co.Symbol, seg, f.URL, g.URL)
		b, derr := c.download(ctx, g.URL, g.PageURL)
		if derr == nil {
			return b, g, nil
		}
		log.Printf("[ir] %s: %s: download %s: %v", co.Symbol, seg, g.URL, derr)
	}
	return nil, filing{}, err
}

// expandedListing returns the document filings found on listing page f, using
// and filling the shared expansion cache; listingsMu guards the cache when
// harvests run concurrently from fetchAndParse workers.
func (c *Client) expandedListing(ctx context.Context, co models.Company, seg string, f filing, listings map[string][]filing, listingsMu *sync.Mutex) []filing {
	listingsMu.Lock()
	found, ok := listings[f.URL]
	listingsMu.Unlock()
	if ok {
		return found
	}
	found = c.pdfsOnPage(ctx, co, seg, f)
	listingsMu.Lock()
	listings[f.URL] = found
	listingsMu.Unlock()
	return found
}

// archiveAndRecord uploads the PDFs and their parsed text to the same
// storage folder, then records paths in company_research_resources and
// saved_files. A failed upload skips that segment (logged) instead of
// aborting the run; the error is only fatal when nothing uploaded at all.
func (c *Client) archiveAndRecord(ctx context.Context, co models.Company, results []filingResult) error {
	if c.storage == nil {
		return fmt.Errorf("storage not configured")
	}
	folder := path.Join(co.Symbol, storageSubFolder)

	byName := make(map[string]string, 2*len(results))
	uploadErrs := make(map[string]string, len(results))
	// one file per upload request: big PDFs take minutes on slow links and a
	// single stalled file must not sink the rest of the batch
	for _, r := range results {
		pdfPaths, err := c.storage.UploadFile(folder, []*archivus.UploadFile{{Name: r.PDFName, Content: r.PDF}})
		if err != nil {
			log.Printf("[ir] %s: upload %s FAILED: %v", co.Symbol, r.PDFName, err)
			uploadErrs[r.Segment] = err.Error()
			continue
		}
		for _, p := range pdfPaths {
			byName[p.Name] = path.Join(p.FolderPath, p.Name)
		}
		txtPaths, err := c.storage.UploadFile(folder, []*archivus.UploadFile{{Name: r.TxtName, Content: []byte(r.Text)}})
		if err != nil {
			log.Printf("[ir] %s: upload %s FAILED: %v", co.Symbol, r.TxtName, err)
			continue // pdf uploaded; text will be retried on the next run
		}
		for _, p := range txtPaths {
			byName[p.Name] = path.Join(p.FolderPath, p.Name)
		}
	}

	var firstErr error
	for _, r := range results {
		pdfPath := byName[r.PDFName]
		txtPath := byName[r.TxtName]
		if pdfPath == "" && txtPath == "" {
			if firstErr == nil {
				firstErr = fmt.Errorf("upload %s: %s", r.PDFName, uploadErrs[r.Segment])
			}
			continue // nothing archived for this segment
		}
		extracted := map[string]any{
			"pdf_name":      r.PDFName,
			"pdf_path":      pdfPath,
			"text_name":     r.TxtName,
			"text_path":     txtPath,
			"pages":         r.Pages,
			"text_chars":    r.TextChars,
			"page_url":      r.PageURL,
			"anchor_text":   r.Anchor,
			"parsed_at":     time.Now(),
			"storage_class": storageSubFolder,
		}
		if errMsg, ok := uploadErrs[r.Segment]; ok {
			extracted["upload_error"] = errMsg
		}
		if err := c.upsertResource(ctx, co, models.CompanyResearchResource{
			CompanyID:            co.ID,
			AnalysisSegment:      r.Segment,
			Source:               sourceIR,
			ResearchResourceLink: r.SourceURL,
			RawDataUrl:           pdfPath,
			ExtractedData:        extracted,
		}); err != nil {
			return err
		}
		if pdfPath != "" {
			if err := c.upsertSavedFile(ctx, co, r.PDFName, folderOf(pdfPath), "pdf", float64(len(r.PDF))); err != nil {
				return err
			}
		}
		if txtPath != "" {
			if c.indexer != nil {
				n, err := c.indexer.IndexDocument(ctx, co, r.Segment, r.SourceURL, r.Text)
				if err != nil {
					log.Printf("[ir] %s: %s: index: %v", co.Symbol, r.Segment, err)
				} else {
					log.Printf("[ir] %s: %s: indexed %d chunks", co.Symbol, r.Segment, n)
					if err := c.store.MarkResourceIndexed(ctx, co.ID, r.Segment, true); err != nil {
						log.Printf("[ir] %s: %s: mark indexed: %v", co.Symbol, r.Segment, err)
					}
				}
			}
			if err := c.upsertSavedFile(ctx, co, r.TxtName, folderOf(txtPath), "txt", float64(r.TextChars)); err != nil {
				return err
			}

		}
	}
	return firstErr
}

// folderOf splits dir/file paths coming back from storage.
func folderOf(p string) string {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return ""
	}
	return p[:i]
}

// docName builds the archived file name for a segment.
func docName(symbol, seg, ext string, day time.Time) string {
	return fmt.Sprintf("%s_%s_%s.%s", sanitizeName(symbol), seg, day.Format(dayFormat), ext)
}

func sanitizeName(s string) string {
	r := strings.NewReplacer(" ", "_", "/", "_", "\\", "_", ":", "_", "\"", "", "'", "")
	return r.Replace(strings.TrimSpace(s))
}

// listResources returns the resource documents stored for co.
func (c *Client) listResources(ctx context.Context, co models.Company) ([]models.CompanyResearchResource, error) {
	rows, err := c.store.Pool.Query(ctx, `SELECT id, analysis_segment, source, research_resource_link,
			raw_data_url, extracted_data, created_at, updated_at
		FROM company_research_resources WHERE company_id = $1 ORDER BY analysis_segment`, co.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.CompanyResearchResource
	for rows.Next() {
		var r models.CompanyResearchResource
		if err := rows.Scan(&r.ID, &r.AnalysisSegment, &r.Source, &r.ResearchResourceLink,
			&r.RawDataUrl, &r.ExtractedData, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.CompanyID = co.ID
		out = append(out, r)
	}
	return out, rows.Err()
}

// upsertResource writes one (company_id, analysis_segment) document.
func (c *Client) upsertResource(ctx context.Context, co models.Company, res models.CompanyResearchResource) error {
	now := time.Now()
	_, err := c.store.Pool.Exec(ctx, `
		INSERT INTO company_research_resources (company_id, analysis_segment, source, research_resource_link, raw_data_url, extracted_data, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
		ON CONFLICT (company_id, analysis_segment) DO UPDATE SET
			source = EXCLUDED.source,
			research_resource_link = EXCLUDED.research_resource_link,
			raw_data_url = EXCLUDED.raw_data_url,
			extracted_data = EXCLUDED.extracted_data,
			updated_at = EXCLUDED.updated_at`,
		co.ID, res.AnalysisSegment, res.Source, res.ResearchResourceLink, res.RawDataUrl, res.ExtractedData, now)
	if err != nil {
		return fmt.Errorf("ir: save %s resource: %w", res.AnalysisSegment, err)
	}
	return nil
}

// upsertSavedFile records one archived file in saved_files.
func (c *Client) upsertSavedFile(ctx context.Context, co models.Company, name, folderPath, ext string, size float64) error {
	now := time.Now()
	_, err := c.store.Pool.Exec(ctx, `
		INSERT INTO saved_files (company_id, name, extension, size, folder_path, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)
		ON CONFLICT (company_id, folder_path, name) DO UPDATE SET
			extension = EXCLUDED.extension,
			size = EXCLUDED.size,
			updated_at = EXCLUDED.updated_at`,
		co.ID, name, ext, size, folderPath, now)
	if err != nil {
		return fmt.Errorf("ir: save file %s: %w", name, err)
	}
	return nil
}

// GetData returns the stored research resources if any exist, otherwise it
// runs the full pipeline and returns the fresh documents.
func (c *Client) GetData(ctx context.Context, co models.Company, _ json.RawMessage) (json.RawMessage, error) {
	docs, err := c.listResources(ctx, co)
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		if err := c.Execute(ctx, co); err != nil {
			return nil, err
		}
		docs, err = c.listResources(ctx, co)
		if err != nil {
			return nil, err
		}
	}
	out, err := json.Marshal(docs)
	if err != nil {
		return nil, fmt.Errorf("ir: encode resources: %w", err)
	}
	return out, nil
}

// RefreshData re-runs the pipeline and returns the stored documents.
func (c *Client) RefreshData(ctx context.Context, co models.Company, _ json.RawMessage) (json.RawMessage, error) {
	if err := c.Execute(ctx, co); err != nil {
		return nil, err
	}
	docs, err := c.listResources(ctx, co)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(docs)
	if err != nil {
		return nil, fmt.Errorf("ir: encode resources: %w", err)
	}
	return out, nil
}

// SaveToFileCollection upserts a resource document from JSON args.
func (c *Client) SaveToFileCollection(ctx context.Context, co models.Company, _ []byte, args json.RawMessage) error {
	var res struct {
		AnalysisSegment      string         `json:"analysis_segment"`
		Source               string         `json:"source"`
		ResearchResourceLink string         `json:"research_resource_link"`
		RawDataUrl           string         `json:"raw_data_url"`
		ExtractedData        map[string]any `json:"extracted_data"`
	}
	if len(args) == 0 {
		return fmt.Errorf("ir: resource args required")
	}
	if err := json.Unmarshal(args, &res); err != nil {
		return fmt.Errorf("ir: decode resource args: %w", err)
	}
	if res.AnalysisSegment == "" {
		return fmt.Errorf("ir: resource args need analysis_segment")
	}
	extracted := map[string]any{}
	for k, v := range res.ExtractedData {
		extracted[k] = v
	}
	return c.upsertResource(ctx, co, models.CompanyResearchResource{
		CompanyID:            co.ID,
		AnalysisSegment:      res.AnalysisSegment,
		Source:               orDefault(res.Source, sourceIR),
		ResearchResourceLink: res.ResearchResourceLink,
		RawDataUrl:           res.RawDataUrl,
		ExtractedData:        extracted,
	})
}

// SaveToFile uploads raw bytes as a filing file and records its location.
func (c *Client) SaveToFile(ctx context.Context, co models.Company, raw []byte, args json.RawMessage) error {
	var spec struct {
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal(args, &spec); err != nil {
		return fmt.Errorf("ir: decode file args: %w", err)
	}
	if spec.Filename == "" {
		return fmt.Errorf("ir: file args need filename")
	}
	paths, err := c.storage.UploadFile(path.Join(co.Symbol, storageSubFolder),
		[]*archivus.UploadFile{{Name: spec.Filename, Content: raw}})
	if err != nil {
		return err
	}
	ext := ""
	if i := strings.LastIndexByte(spec.Filename, '.'); i >= 0 {
		ext = strings.ToLower(spec.Filename[i+1:])
	}
	for _, p := range paths {
		if p.IsError {
			continue
		}
		if err := c.upsertSavedFile(ctx, co, p.Name, p.FolderPath, ext, float64(len(raw))); err != nil {
			return err
		}
	}
	return nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
