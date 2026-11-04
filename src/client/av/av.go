package av

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"vyasa/src/client"
	"vyasa/src/client/archivus"
	"vyasa/src/helpers"
	"vyasa/src/models"
	"vyasa/src/store"
)

const (
	baseURL        = "https://www.alphavantage.co/query"
	rateLimitDelay = 13 * time.Second // free tier: ~5 requests/min, 25/day
	httpTimeout    = 60 * time.Second

	// FunctionTimeSeriesDaily returns end-of-day OHLCV (last ~100 sessions).
	FunctionTimeSeriesDaily = "TIME_SERIES_DAILY"
	// FunctionGlobalQuote returns the current price/volume for a symbol.
	FunctionGlobalQuote = "GLOBAL_QUOTE"

	storageFolder  = "alphavantage"
	fileTimeFormat = "2006-01-02"
)

// validFunctions maps each supported API function to its response's top-level
// payload key (the check a successful response must pass) and the table the
// response is stored in.
var validFunctions = map[string][2]string{
	FunctionTimeSeriesDaily: {"Time Series (Daily)", models.TableAVTimeSeries},
	FunctionGlobalQuote:     {"Global Quote", models.TableAVGlobalQuote},
}

type Client struct {
	apiKey  string
	storage client.StorageClient
	store   *store.Store
	hc      *http.Client

	mu       sync.Mutex
	lastCall time.Time
}

// New creates a client for apiKey; storage and store are optional.
func New(apiKey string, storage client.StorageClient, st *store.Store) *Client {
	return &Client{
		apiKey:  apiKey,
		storage: storage,
		store:   st,
		hc:      &http.Client{Timeout: httpTimeout},
	}
}

func (c *Client) decodeArguments(args json.RawMessage) (string, error) {
	var params map[string]string
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("av: decode arguments: %w", err)
	}
	if function, ok := params["function"]; !ok || function == "" {
		return validFunctions[FunctionTimeSeriesDaily][0], nil
	}
	return params["function"], nil
}

func (c *Client) fetch(ctx context.Context, co models.Company, args json.RawMessage) (json.RawMessage, error) {
	if c.apiKey == "" {
		return nil, fmt.Errorf("av: missing API key")
	}
	function, err := c.decodeArguments(args)
	if err != nil {
		return nil, err
	}

	if err := c.waitRateLimit(ctx); err != nil {
		return nil, err
	}

	q := url.Values{}
	q.Set("function", function)
	q.Set("symbol", co.Symbol+"."+co.Exchange)
	q.Set("apikey", c.apiKey)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("av: %s: %s", resp.Status, string(body))
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("av: empty response")
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("av: decode response: %w", err)
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("av: %s %s: empty response (unknown symbol?)", function, co.Symbol)
	}
	for _, key := range []string{"Error Message", "Note", "Information"} {
		if msg, ok := payload[key]; ok {
			return nil, fmt.Errorf("av: %s %s: %s: %s", function, co.Symbol, key, helpers.RawString(msg))
		}
	}
	if key, ok := validFunctions[function]; ok {
		raw, ok := payload[key[0]]
		if !ok {
			keys := make([]string, 0, len(payload))
			for k := range payload {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			return nil, fmt.Errorf("av: %s %s: response missing %q (got keys: %s)", function, co.Symbol, key[0], strings.Join(keys, ", "))
		}
		// Unknown symbols come back with the key present but empty
		// ({"Global Quote": {}}): treat that as a failed lookup.
		if trimmed := strings.TrimSpace(string(raw)); trimmed == "{}" {
			return nil, fmt.Errorf("av: %s %s: empty %q (unknown symbol?)", function, co.Symbol, key[0])
		}
	}

	return json.RawMessage(body), nil
}

func (c *Client) fetchAndSave(ctx context.Context, function string, co models.Company, args json.RawMessage) (json.RawMessage, error) {
	raw, err := c.fetch(ctx, co, args)
	if err != nil {
		return nil, err
	}
	if _, err := c.saveResponse(ctx, co, raw, function); err != nil {
		return nil, err
	}
	return raw, nil
}

func (c *Client) RefreshData(ctx context.Context, co models.Company, args json.RawMessage) (json.RawMessage, error) {
	function, err := c.decodeArguments(args)
	if err != nil {
		return nil, err
	}
	if _, ok := validFunctions[function]; !ok {
		return nil, fmt.Errorf("av: unsupported function %q", function)
	}
	return c.fetchAndSave(ctx, function, co, args)
}

// GetData returns the stored response for the current month if one exists,
// otherwise it fetches from the API and persists the result.
func (c *Client) GetData(ctx context.Context, co models.Company, args json.RawMessage) (json.RawMessage, error) {
	function, err := c.decodeArguments(args)
	if err != nil {
		return nil, err
	}
	if _, ok := validFunctions[function]; !ok {
		return nil, fmt.Errorf("av: unsupported function %q", function)
	}

	if c.store != nil {
		stored, err := c.monthResponse(ctx, co, function)
		if err != nil || stored != nil {
			return stored, err
		}
	}
	return c.fetchAndSave(ctx, function, co, args)
}

// monthResponse returns the latest response recorded for the company in the
// current month, or nil if none exists.
func (c *Client) monthResponse(ctx context.Context, co models.Company, function string) (json.RawMessage, error) {
	now := time.Now()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	table := validFunctions[function][1]

	var raw []byte
	err := c.store.Pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT response FROM %s WHERE company_id = $1 AND function = $2 AND date >= $3 ORDER BY date DESC LIMIT 1`, table),
		co.ID, function, monthStart,
	).Scan(&raw)
	if store.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("av: read %s response: %w", co.Symbol, err)
	}
	return json.RawMessage(raw), nil
}

// latestResponse returns the most recently stored response for the company
// and function, regardless of date, for change detection.
func (c *Client) latestResponse(ctx context.Context, co models.Company, function string) (json.RawMessage, bool, error) {
	table := validFunctions[function][1]

	var raw []byte
	err := c.store.Pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT response FROM %s WHERE company_id = $1 AND function = $2 ORDER BY date DESC LIMIT 1`, table),
		co.ID, function,
	).Scan(&raw)
	if store.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("av: read %s latest response: %w", co.Symbol, err)
	}
	return json.RawMessage(raw), true, nil
}

// saveResponse stores the raw response when it differs from the most recent
// stored one, and reports whether it was saved. Re-running collection with
// unchanged data is a no-op.
func (c *Client) saveResponse(ctx context.Context, co models.Company, raw []byte, function string) (bool, error) {
	if c.store == nil {
		return false, fmt.Errorf("av: store not configured")
	}
	fn, ok := validFunctions[function]
	if !ok {
		return false, fmt.Errorf("av: unsupported function %q", function)
	}

	if latest, ok, err := c.latestResponse(ctx, co, function); err != nil {
		return false, err
	} else if ok && client.JSONEqual(latest, raw) {
		log.Printf("[av] %s: %s: response unchanged, skipping save", co.Symbol, function)
		return false, nil
	}

	var response map[string]any
	if err := json.Unmarshal(raw, &response); err != nil {
		return false, fmt.Errorf("av: decode response: %w", err)
	}

	now := time.Now()
	_, err := c.store.Pool.Exec(ctx,
		fmt.Sprintf(`INSERT INTO %s (company_id, symbol, function, date, response, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $6)`, fn[1]),
		co.ID, co.Symbol, function, now, response, now,
	)
	if err != nil {
		return false, fmt.Errorf("av: save response: %w", err)
	}
	return true, nil
}

// SaveToFileCollection stores the raw response in the table mapped to the
// requested function (see validFunctions) unless it is unchanged.
func (c *Client) SaveToFileCollection(ctx context.Context, co models.Company, raw []byte, args json.RawMessage) error {
	function, err := c.decodeArguments(args)
	if err != nil {
		return err
	}
	_, err = c.saveResponse(ctx, co, raw, function)
	return err
}

// SaveToDB stores the raw API response in the table mapped to the requested
// function (see validFunctions), preserving its fields at the top level.
func (c *Client) SaveToDB(ctx context.Context, co models.Company, raw []byte, args json.RawMessage) error {
	if c.store == nil {
		return fmt.Errorf("av: store not configured")
	}
	function, err := c.decodeArguments(args)
	if err != nil {
		return err
	}
	fn, ok := validFunctions[function]
	if !ok {
		return fmt.Errorf("av: unsupported function %q", function)
	}
	var response map[string]any
	if err := json.Unmarshal(raw, &response); err != nil {
		return fmt.Errorf("av: decode response: %w", err)
	}
	if _, err := c.store.Pool.Exec(ctx,
		fmt.Sprintf(`INSERT INTO %s (company_id, symbol, function, date, response) VALUES ($1, $2, $3, $4, $5)`, fn[1]),
		co.ID, co.Symbol, function, time.Now(), response,
	); err != nil {
		return fmt.Errorf("av: save response: %w", err)
	}
	return nil
}

// SaveToFile uploads the raw response to storage and records its location.
func (c *Client) SaveToFile(ctx context.Context, co models.Company, raw []byte, args json.RawMessage) error {
	function, err := c.decodeArguments(args)
	if err != nil {
		return err
	}
	if c.storage == nil {
		return nil
	}

	fileName := fmt.Sprintf("%s_%s_%s.json", co.Symbol, function, time.Now().Format(fileTimeFormat))
	paths, err := c.storage.UploadFile(co.Symbol, []*archivus.UploadFile{{Name: fileName, Content: raw}})
	if err != nil {
		return err
	}
	return client.SaveUploadedPaths(ctx, c.store, co, paths)
}

// Execute fetches data for every supported function and persists each
// response to the database and file storage when it changed.
func (c *Client) Execute(ctx context.Context, co models.Company) error {
	for function := range validFunctions {
		args, err := json.Marshal(map[string]string{"function": function})
		if err != nil {
			return err
		}
		raw, err := c.fetch(ctx, co, args)
		if err != nil {
			return err
		}
		saved, err := c.saveResponse(ctx, co, raw, function)
		if err != nil {
			return err
		}
		if !saved {
			continue
		}
		if err := c.SaveToFile(ctx, co, raw, args); err != nil {
			return err
		}
	}
	return nil
}
