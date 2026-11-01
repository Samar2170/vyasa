// Package tickertapev2 scrapes live Tickertape stock pages. Unlike the
// file-based tickertape client, it resolves each company against Tickertape's
// search API (https://api.tickertape.in/search) to obtain the canonical stock
// slug, downloads the server-rendered page and extracts the pageProps payload
// embedded in its <script id="__NEXT_DATA__"> tag. The sections are persisted
// to the same PostgreSQL tables as the v1 client, so the metrics package
// keeps working unchanged.
//
// Outbound stock-page fetches are capped per day (maxPerDay, see
// config.json). The running count is persisted in the api_usage table, keyed
// by (client, date), so the cap survives restarts and resets automatically
// with the local calendar day. Slug lookups against the search API are not
// counted.
package tickertapev2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"vyasa/src/client"
	"vyasa/src/client/archivus"
	"vyasa/src/client/tickertape"
	"vyasa/src/models"
	"vyasa/src/store"
)

const (
	searchURL = "https://api.tickertape.in/search"
	pageBase  = "https://www.tickertape.in"

	httpTimeout    = 60 * time.Second
	fileTimeFormat = "2006-01-02T15-04-05"

	usageClient = "tickertapev2"

	// userAgent identifies the scraper as a regular browser; Tickertape rejects
	// requests with the default Go client agent.
	userAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
)

// ErrDailyLimit is returned when the per-day stock-page fetch budget is spent.
// The caller should stop processing further companies until the next day.
var ErrDailyLimit = errors.New("tickertapev2: daily request limit reached")

var _ client.DataSourceClient = (*Client)(nil)

type Client struct {
	storage   client.StorageClient
	store     *store.Store
	hc        *http.Client
	maxPerDay int
}

// New creates a scraper with the given daily request cap (0 = unlimited).
func New(maxPerDay int, storage client.StorageClient, st *store.Store) *Client {
	return &Client{
		storage:   storage,
		store:     st,
		hc:        &http.Client{Timeout: httpTimeout},
		maxPerDay: maxPerDay,
	}
}

// searchResponse is the subset of the search API payload used to resolve a
// symbol to its stock slug.
type searchResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	Data    struct {
		Stocks []searchStock `json:"stocks"`
	} `json:"data"`
}

type searchStock struct {
	Ticker string `json:"ticker"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Slug   string `json:"slug"`
	SID    string `json:"sid"`
}

// resolveSlug asks the search API for symbol and returns the canonical stock
// slug (e.g. /stocks/reliance-industries-RELI). Exchange suffixes are ignored.
func (c *Client) resolveSlug(ctx context.Context, symbol string) (string, error) {
	q := url.Values{}
	q.Set("text", baseSymbol(symbol))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("tickertapev2: search %s: %w", symbol, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("tickertapev2: search %s: read: %w", symbol, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("tickertapev2: search %s: %s: %s", symbol, resp.Status, string(body))
	}

	var sr searchResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return "", fmt.Errorf("tickertapev2: search %s: decode: %w", symbol, err)
	}
	if !sr.Success {
		return "", fmt.Errorf("tickertapev2: search %s: %s", symbol, sr.Error)
	}

	stock, ok := pickStock(sr.Data.Stocks, baseSymbol(symbol))
	if !ok {
		return "", fmt.Errorf("tickertapev2: no stock match for %s", symbol)
	}
	return pageURL(stock.Slug), nil
}

// baseSymbol strips an exchange suffix (RELIANCE.NS -> RELIANCE) and upper-cases.
func baseSymbol(symbol string) string {
	s := strings.ToUpper(strings.TrimSpace(symbol))
	if i := strings.IndexByte(s, '.'); i > 0 {
		s = s[:i]
	}
	return s
}

// pickStock returns the stock whose ticker matches symbol (case-insensitive);
// the first stock entry is the fallback when the API omits an exact ticker.
func pickStock(stocks []searchStock, symbol string) (searchStock, bool) {
	for _, s := range stocks {
		if strings.EqualFold(s.Ticker, symbol) {
			return s, true
		}
	}
	for _, s := range stocks {
		if s.Type == "stock" && s.Slug != "" {
			return s, true
		}
	}
	return searchStock{}, false
}

// pageURL builds the absolute stock page URL from a search API slug.
func pageURL(slug string) string {
	if !strings.HasPrefix(slug, "/") {
		slug = "/" + slug
	}
	return pageBase + slug
}

// fetchPage downloads the stock page HTML for slug. It consumes one daily
// request slot; ErrDailyLimit is returned when the budget is exhausted.
func (c *Client) fetchPage(ctx context.Context, slug string) ([]byte, error) {
	if err := c.reserveSlot(ctx); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, slug, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tickertapev2: fetch %s: %w", slug, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("tickertapev2: fetch %s: read: %w", slug, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("tickertapev2: fetch %s: %s", slug, resp.Status)
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("tickertapev2: fetch %s: empty response", slug)
	}
	return body, nil
}

// reserveSlot atomically increments the daily counter for this client and
// returns ErrDailyLimit once the cap is exceeded. The increment is rolled back
// when the cap would be breached so the stored count stays at the limit.
func (c *Client) reserveSlot(ctx context.Context) error {
	if c.store == nil {
		return fmt.Errorf("tickertapev2: store not configured")
	}
	if c.maxPerDay <= 0 {
		return nil
	}

	now := time.Now()
	var count int
	err := c.store.Pool.QueryRow(ctx, `
		INSERT INTO api_usage (client, date, count, created_at, updated_at)
		VALUES ($1, $2, 1, $3, $3)
		ON CONFLICT (client, date) DO UPDATE
		SET count = api_usage.count + 1, updated_at = EXCLUDED.updated_at
		RETURNING count`,
		usageClient, now, now,
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("tickertapev2: reserve request slot: %w", err)
	}
	if count > c.maxPerDay {
		if _, uerr := c.store.Pool.Exec(ctx,
			`UPDATE api_usage SET count = count - 1 WHERE client = $1 AND date = $2`,
			usageClient, now,
		); uerr != nil {
			return fmt.Errorf("tickertapev2: release request slot: %w", uerr)
		}
		return ErrDailyLimit
	}
	return nil
}

// parse resolves and downloads the live page for co, returning the extracted
// pageProps sections as JSON (keys normalised by the shared tickertape package).
func (c *Client) parse(ctx context.Context, co models.Company) (json.RawMessage, error) {
	slug, err := c.resolveSlug(ctx, co.Symbol)
	if err != nil {
		return nil, err
	}
	page, err := c.fetchPage(ctx, slug)
	if err != nil {
		return nil, err
	}
	sections, err := tickertape.ExtractSections(co.Symbol, page)
	if err != nil {
		return nil, err
	}
	buf, err := json.MarshalIndent(sections, "", "    ")
	if err != nil {
		return nil, fmt.Errorf("tickertapev2: %s: encode extract: %w", co.Symbol, err)
	}
	return buf, nil
}

func (c *Client) parseAndSave(ctx context.Context, co models.Company) (json.RawMessage, error) {
	raw, err := c.parse(ctx, co)
	if err != nil {
		return nil, err
	}
	if err := c.SaveToFileCollection(ctx, co, raw, nil); err != nil {
		return nil, err
	}
	return raw, nil
}

// GetData returns the stored extract for the current month if one exists,
// otherwise it scrapes the live page and persists the result.
func (c *Client) GetData(ctx context.Context, co models.Company, args json.RawMessage) (json.RawMessage, error) {
	if c.store != nil {
		stored, err := c.monthResponse(ctx, co)
		if err != nil || stored != nil {
			return stored, err
		}
	}
	return c.parseAndSave(ctx, co)
}

// RefreshData scrapes the live page and persists the result unconditionally.
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
		return nil, fmt.Errorf("tickertapev2: read %s extract: %w", co.Symbol, err)
	}
	return json.RawMessage(raw), nil
}

// SaveToFileCollection upserts the full extract and each section into the same
// tables as the v1 client (see tickertape.SaveSections).
func (c *Client) SaveToFileCollection(ctx context.Context, co models.Company, raw []byte, _ json.RawMessage) error {
	if c.store == nil {
		return fmt.Errorf("tickertapev2: store not configured")
	}
	return tickertape.SaveSections(ctx, c.store, co, raw)
}

// SaveToFile uploads the raw extract to storage and records its location.
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

// Execute scrapes the live page and persists the extract to the database and
// file storage. It returns ErrDailyLimit once the daily fetch budget is spent.
func (c *Client) Execute(ctx context.Context, co models.Company) error {
	raw, err := c.parse(ctx, co)
	if err != nil {
		return err
	}
	if err := c.SaveToFileCollection(ctx, co, raw, nil); err != nil {
		return err
	}
	return c.SaveToFile(ctx, co, raw, nil)
}
