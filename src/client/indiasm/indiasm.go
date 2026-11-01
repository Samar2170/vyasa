// Package indiasm provides a client for the stock.indianapi.in API: the
// /stock endpoint returns the company profile, financials, shareholding and
// recent news for an Indian stock. Raw payloads are archived to Archivus
// storage and mirrored into PostgreSQL for indexing and querying.
package indiasm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"vyasa/internal/helpers"
	"vyasa/src/client"
	"vyasa/src/client/archivus"
	"vyasa/src/models"
	"vyasa/src/store"
)

const (
	baseURL        = "https://stock.indianapi.in"
	httpTimeout    = 60 * time.Second
	fileTimeFormat = "2006-01-02T15-04-05"
	dayFormat      = "2006-01-02"
)

var _ client.DataSourceClient = (*Client)(nil)

type Client struct {
	apiKey  string
	storage client.StorageClient
	store   *store.Store
	hc      *http.Client
}

// New creates a client for apiKey, falling back to fallbackKey when the
// primary key is empty (as in the source notebook). storage and store are
// optional.
func New(apiKey, fallbackKey string, storage client.StorageClient, st *store.Store) *Client {
	if apiKey == "" {
		apiKey = fallbackKey
	}
	return &Client{
		apiKey:  apiKey,
		storage: storage,
		store:   st,
		hc:      &http.Client{Timeout: httpTimeout},
	}
}

// fetch calls the /stock endpoint for co, falling back to the company name
// when the API reports the symbol as unknown.
func (c *Client) fetch(ctx context.Context, co models.Company) (json.RawMessage, error) {
	if c.apiKey == "" {
		return nil, fmt.Errorf("indiasm: missing API key")
	}

	raw, err := c.fetchByName(ctx, co.Symbol)
	if err != nil {
		if !strings.Contains(err.Error(), "Stock not found") || co.Name == "" {
			return nil, err
		}
		raw, err = c.fetchByName(ctx, co.Name)
		if err != nil {
			return nil, err
		}
	}
	return raw, nil
}

func (c *Client) fetchByName(ctx context.Context, name string) (json.RawMessage, error) {
	q := url.Values{}
	q.Set("name", name)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/stock?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", c.apiKey)

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
		return nil, fmt.Errorf("indiasm: %s: %s", resp.Status, string(body))
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("indiasm: empty response")
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("indiasm: decode response: %w", err)
	}
	for _, key := range []string{"error", "message"} {
		if msg, ok := payload[key]; ok {
			return nil, fmt.Errorf("indiasm: %s: %s", name, helpers.RawString(msg))
		}
	}
	if _, ok := payload["companyName"]; !ok {
		return nil, fmt.Errorf("indiasm: %s: response missing %q", name, "companyName")
	}

	return json.RawMessage(body), nil
}

func (c *Client) fetchAndSave(ctx context.Context, co models.Company, args json.RawMessage) (json.RawMessage, error) {
	raw, err := c.fetch(ctx, co)
	if err != nil {
		return nil, err
	}
	if err := c.SaveToFileCollection(ctx, co, raw, args); err != nil {
		return nil, err
	}
	return raw, nil
}

// GetData returns the stored response for the current month if one exists,
// otherwise it fetches from the API and persists the result.
func (c *Client) GetData(ctx context.Context, co models.Company, args json.RawMessage) (json.RawMessage, error) {
	if c.store != nil {
		stored, err := c.monthResponse(ctx, co)
		if err != nil || stored != nil {
			return stored, err
		}
	}
	return c.fetchAndSave(ctx, co, args)
}

// RefreshData fetches from the API and persists the result unconditionally.
func (c *Client) RefreshData(ctx context.Context, co models.Company, args json.RawMessage) (json.RawMessage, error) {
	return c.fetchAndSave(ctx, co, args)
}

// monthResponse returns the latest response recorded for the company in the
// current month, or nil if none exists.
func (c *Client) monthResponse(ctx context.Context, co models.Company) (json.RawMessage, error) {
	now := time.Now()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	var raw []byte
	err := c.store.Pool.QueryRow(ctx,
		`SELECT response FROM indiasm_stock WHERE company_id = $1 AND date >= $2 ORDER BY date DESC LIMIT 1`,
		co.ID, monthStart,
	).Scan(&raw)
	if store.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("indiasm: read %s response: %w", co.Symbol, err)
	}
	return json.RawMessage(raw), nil
}

// SaveToFileCollection stores the raw response in the indiasm table.
func (c *Client) SaveToFileCollection(ctx context.Context, co models.Company, raw []byte, _ json.RawMessage) error {
	if c.store == nil {
		return fmt.Errorf("indiasm: store not configured")
	}
	var response map[string]any
	if err := json.Unmarshal(raw, &response); err != nil {
		return fmt.Errorf("indiasm: decode response: %w", err)
	}

	now := time.Now()
	_, err := c.store.Pool.Exec(ctx,
		`INSERT INTO indiasm_stock (company_id, symbol, date, response, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $5)`,
		co.ID, co.Symbol, now, response, now,
	)
	if err != nil {
		return fmt.Errorf("indiasm: save response: %w", err)
	}
	return nil
}

// SaveToDB stores the raw API response in the indiasm stock table,
// preserving its fields at the top level.
func (c *Client) SaveToDB(ctx context.Context, co models.Company, raw []byte, _ json.RawMessage) error {
	if c.store == nil {
		return fmt.Errorf("indiasm: store not configured")
	}
	var response map[string]any
	if err := json.Unmarshal(raw, &response); err != nil {
		return fmt.Errorf("indiasm: decode response: %w", err)
	}
	if _, err := c.store.Pool.Exec(ctx,
		`INSERT INTO indiasm_stock (company_id, symbol, date, response) VALUES ($1, $2, $3, $4)`,
		co.ID, co.Symbol, time.Now(), response,
	); err != nil {
		return fmt.Errorf("indiasm: save response: %w", err)
	}
	return nil
}

// SaveToFile uploads the raw response to storage and records its location.
func (c *Client) SaveToFile(ctx context.Context, co models.Company, raw []byte, _ json.RawMessage) error {
	if c.storage == nil {
		return nil
	}

	fileName := fmt.Sprintf("%s_indiasm_%s.json", co.Symbol, time.Now().Format(fileTimeFormat))
	paths, err := c.storage.UploadFile(co.Symbol, []*archivus.UploadFile{{Name: fileName, Content: raw}})
	if err != nil {
		return err
	}
	return client.SaveUploadedPaths(ctx, c.store, co, paths)
}

// Execute fetches the stock profile and persists it to the database and
// file storage.
func (c *Client) Execute(ctx context.Context, co models.Company) error {
	raw, err := c.fetch(ctx, co)
	if err != nil {
		return err
	}
	if err := c.SaveToFileCollection(ctx, co, raw, nil); err != nil {
		return err
	}
	return c.SaveToFile(ctx, co, raw, nil)
}
