// Package tickertape extracts stock data from saved Tickertape stock pages
// (see tickertape_files/tickertape_kotak_resp.html): the page HTML embeds the
// full payload as JSON inside a <script id="__NEXT_DATA__"> tag, and the
// useful sections (commentary, FAQ, financial statements, peers, events) live
// under props.pageProps. The client archives the extract to Archivus storage,
// saves the full extract to the tickertape table and each section into its
// own PostgreSQL table, where the metrics package reads the financial
// statements from.
package tickertape

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"vyasa/src/client"
	"vyasa/src/client/archivus"
	"vyasa/src/models"
	"vyasa/src/store"
)

const (
	defaultFilesDir = "tickertape_files"
	fileTimeFormat  = "2006-01-02T15-04-05"
	dayFormat       = "2006-01-02"

	nextDataMarker = `id="__NEXT_DATA__"`
)

var _ client.DataSourceClient = (*Client)(nil)

// ExtractedKeys are the pageProps sections archived from a stock page.
var ExtractedKeys = []string{
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
}

// SectionTables maps each section key to its own table.
var SectionTables = map[string]string{
	"commentary":                 models.TableTickerCommentary,
	"stockPageFaq":               models.TableTickerFaq,
	"income-normal-annual":       models.TableTickerIncomeAnnual,
	"income-normal-interim":      models.TableTickerIncomeInterim,
	"balancesheet-normal-annual": models.TableTickerBalancesheetAnnual,
	"cashflow-normal-annual":     models.TableTickerCashflowAnnual,
	"peers-technical":            models.TableTickerPeers,
	"events-corp-actions":        models.TableTickerEventsCorpActions,
	"events-announcements":       models.TableTickerEventsAnnouncements,
	"events-legal":               models.TableTickerEventsLegal,
}

type Client struct {
	storage  client.StorageClient
	store    *store.Store
	filesDir string
}

// New creates a client that reads saved tickertape pages from filesDir
// (defaulting to tickertape_files/).
func New(storage client.StorageClient, st *store.Store) *Client {
	return &Client{storage: storage, store: st, filesDir: defaultFilesDir}
}

// parse reads the saved tickertape page for co and returns the extracted
// stock data as JSON: the pageProps sections embedded in the page's
// __NEXT_DATA__ script.
func (c *Client) parse(co models.Company) ([]byte, error) {
	path, err := c.htmlPath(co.Symbol)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tickertape: read %s: %w", path, err)
	}
	sections, err := ExtractSections(co.Symbol, raw)
	if err != nil {
		return nil, err
	}

	buf, err := json.MarshalIndent(sections, "", "    ")
	if err != nil {
		return nil, fmt.Errorf("tickertape: %s: encode extract: %w", co.Symbol, err)
	}
	return buf, nil
}

// htmlPath locates the saved page for symbol in filesDir. Page files follow
// no single naming convention (tickertape_kotak_resp.html,
// PARAGMILK_tickertape_resp.html), so decorations are stripped from each
// .html base name and matched against the symbol: an exact match wins, a
// short prefix (kotak for kotakbank) is accepted.
func (c *Client) htmlPath(symbol string) (string, error) {
	entries, err := os.ReadDir(c.filesDir)
	if err != nil {
		return "", fmt.Errorf("tickertape: read %s: %w", c.filesDir, err)
	}

	lower := strings.ToLower(symbol)
	if i := strings.IndexByte(lower, '.'); i > 0 {
		lower = lower[:i] // drop exchange suffix
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		core := strings.ToLower(strings.TrimSuffix(e.Name(), ".html"))
		core = strings.TrimSuffix(core, "_resp")
		core = strings.TrimSuffix(core, "_tickertape")
		core = strings.TrimPrefix(core, "tickertape_")
		if core == lower || (len(core) >= 4 && strings.HasPrefix(lower, core)) {
			return filepath.Join(c.filesDir, e.Name()), nil
		}
	}
	return "", fmt.Errorf("tickertape: no saved page for %s in %s", symbol, c.filesDir)
}

// ExtractSections reads a saved page and returns the extracted sections keyed
// by their pageProps key.
func ExtractSections(symbol string, page []byte) (map[string]json.RawMessage, error) {
	data, err := extractNextData(page)
	if err != nil {
		return nil, fmt.Errorf("tickertape: %s: %w", symbol, err)
	}

	var root struct {
		Props struct {
			PageProps json.RawMessage `json:"pageProps"`
		} `json:"props"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("tickertape: %s: decode __NEXT_DATA__: %w", symbol, err)
	}
	if len(root.Props.PageProps) == 0 {
		return nil, fmt.Errorf("tickertape: %s: no pageProps in __NEXT_DATA__", symbol)
	}

	var pageProps map[string]json.RawMessage
	if err := json.Unmarshal(root.Props.PageProps, &pageProps); err != nil {
		return nil, fmt.Errorf("tickertape: %s: decode pageProps: %w", symbol, err)
	}

	out := make(map[string]json.RawMessage, len(ExtractedKeys))
	for _, key := range ExtractedKeys {
		if v, ok := pageProps[key]; ok {
			out[key] = v
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("tickertape: %s: no stock data sections in pageProps", symbol)
	}
	if err := renameStatementKeys(out); err != nil {
		return nil, fmt.Errorf("tickertape: %s: %w", symbol, err)
	}
	return out, nil
}

// extractNextData returns the raw JSON between the __NEXT_DATA__ script tags.
func extractNextData(page []byte) ([]byte, error) {
	i := bytes.Index(page, []byte(nextDataMarker))
	if i < 0 {
		return nil, fmt.Errorf("no __NEXT_DATA__ script in HTML")
	}
	start := bytes.IndexByte(page[i:], '>')
	if start < 0 {
		return nil, fmt.Errorf("malformed __NEXT_DATA__ script tag")
	}
	start += i + 1

	end := bytes.Index(page[start:], []byte("</script>"))
	if end < 0 {
		return nil, fmt.Errorf("unterminated __NEXT_DATA__ script")
	}
	end += start

	data := bytes.TrimSpace(page[start:end])
	if len(data) == 0 {
		return nil, fmt.Errorf("empty __NEXT_DATA__ script")
	}
	return data, nil
}

func (c *Client) parseAndSave(ctx context.Context, co models.Company) (json.RawMessage, error) {
	raw, err := c.parse(co)
	if err != nil {
		return nil, err
	}
	if err := c.SaveToFileCollection(ctx, co, raw, nil); err != nil {
		return nil, err
	}
	return raw, nil
}

// GetData returns the stored extract for the current month if one exists,
// otherwise it parses the saved page and persists the result.
func (c *Client) GetData(ctx context.Context, co models.Company, args json.RawMessage) (json.RawMessage, error) {
	if c.store != nil {
		stored, err := c.monthResponse(ctx, co)
		if err != nil || stored != nil {
			return stored, err
		}
	}
	return c.parseAndSave(ctx, co)
}

// RefreshData parses the saved page and persists the result unconditionally.
func (c *Client) RefreshData(ctx context.Context, co models.Company, args json.RawMessage) (json.RawMessage, error) {
	return c.parseAndSave(ctx, co)
}

// monthResponse returns the latest extract recorded for the company in the
// current month, or nil if none exists.
func (c *Client) monthResponse(ctx context.Context, co models.Company) (json.RawMessage, error) {
	now := time.Now()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	var raw []byte
	err := c.store.Pool.QueryRow(ctx,
		`SELECT response FROM tickertape WHERE symbol = $1 AND date >= $2 ORDER BY date DESC LIMIT 1`,
		co.Symbol, monthStart,
	).Scan(&raw)
	if store.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("tickertape: read %s extract: %w", co.Symbol, err)
	}
	return json.RawMessage(raw), nil
}

// SaveToFileCollection upserts the full extract into the tickertape
// table and each section into its own table (see SaveSections).
func (c *Client) SaveToFileCollection(ctx context.Context, co models.Company, raw []byte, _ json.RawMessage) error {
	if c.store == nil {
		return fmt.Errorf("tickertape: store not configured")
	}
	return SaveSections(ctx, c.store, co, raw)
}

// SaveSections upserts the full extract into the tickertape table and
// each section into its own table, keyed by (symbol, date) so the payloads
// stay queryable through the jsonb response column.
func SaveSections(ctx context.Context, s *store.Store, co models.Company, raw []byte) error {
	if s == nil {
		return fmt.Errorf("tickertape: store not configured")
	}

	var extract map[string]json.RawMessage
	if err := json.Unmarshal(raw, &extract); err != nil {
		return fmt.Errorf("tickertape: decode extract: %w", err)
	}

	day := time.Now()
	now := time.Now()
	full, err := jsonValue(raw)
	if err != nil {
		return fmt.Errorf("tickertape: %s: %w", co.Symbol, err)
	}
	if err := upsertDoc(ctx, s.Pool, models.TableTickertape,
		co, "", day, full, now,
	); err != nil {
		return fmt.Errorf("tickertape: save extract: %w", err)
	}

	for _, key := range ExtractedKeys {
		section, ok := extract[key]
		if !ok {
			continue
		}
		value, err := jsonValue(section)
		if err != nil {
			return fmt.Errorf("tickertape: %s: decode %s: %w", co.Symbol, key, err)
		}
		if err := upsertDoc(ctx, s.Pool, SectionTables[key],
			co, key, day, value, now,
		); err != nil {
			return fmt.Errorf("tickertape: %s: save %s: %w", co.Symbol, key, err)
		}
	}
	return nil
}

// jsonValue decodes a JSON payload into a Go value pgx encodes as jsonb:
// objects, arrays and scalars all round-trip.
func jsonValue(raw []byte) (any, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	return v, nil
}

// upsertDoc writes one (symbol, date) extract or section document.
func upsertDoc(ctx context.Context, pool *pgxpool.Pool, table string, co models.Company, section string, day time.Time, value any, now time.Time) error {
	_, err := pool.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s (company_id, symbol, section, date, response, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $6)
			ON CONFLICT (symbol, date) DO UPDATE
			SET company_id = EXCLUDED.company_id,
				section = EXCLUDED.section,
				response = EXCLUDED.response,
				updated_at = EXCLUDED.updated_at`,
		table),
		co.ID, co.Symbol, section, day, value, now,
	)
	return err
}

// SaveToFile archives the extract to storage and records its location.
func (c *Client) SaveToFile(ctx context.Context, co models.Company, raw []byte, _ json.RawMessage) error {
	if c.storage == nil {
		return nil
	}

	fileName := fmt.Sprintf("%s_tickertape_%s.json", co.Symbol, time.Now().Format(fileTimeFormat))
	paths, err := c.storage.UploadFile(co.Symbol, []*archivus.UploadFile{{Name: fileName, Content: raw}})
	if err != nil {
		return err
	}
	return client.SaveUploadedPaths(ctx, c.store, co, paths)
}

// Execute parses the saved tickertape page and persists the extract to the
// database and file storage.
func (c *Client) Execute(ctx context.Context, co models.Company) error {
	raw, err := c.parse(co)
	if err != nil {
		return err
	}
	if err := c.SaveToFileCollection(ctx, co, raw, nil); err != nil {
		return err
	}
	return c.SaveToFile(ctx, co, raw, nil)
}
