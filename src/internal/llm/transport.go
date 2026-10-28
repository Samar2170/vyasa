package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Transport struct {
	provider string
	cfg      Config
}

func NewTransport(provider string, cfg Config) *Transport {
	return &Transport{provider: provider, cfg: cfg.WithDefaults()}
}

func (t *Transport) DoJSON(ctx context.Context, method, url string, headers map[string]string, in any, out any) error {
	body, err := encodeBody(t.provider, in)
	if err != nil {
		return err
	}
	retries := t.cfg.retries()
	for attempt := 0; attempt < retries; attempt++ {
		resp, err := t.roundTrip(ctx, method, url, headers, body)
		if err != nil {
			if attempt < retries && ctx.Err() == nil {
				if werr := waitForRetry(ctx, attempt); werr != nil {
					return fmt.Errorf("%s: %w", t.provider, err)
				}
				continue
			}
			return fmt.Errorf("%s: request failed: %w", t.provider, err)
		}
		data, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if readErr != nil {
			if attempt < retries && ctx.Err() == nil {
				if werr := waitForRetry(ctx, attempt); werr != nil {
					return fmt.Errorf("%s: %w", t.provider, werr)
				}
				continue
			}
			return fmt.Errorf("%s: read response: %w", t.provider, readErr)
		}

		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			pe := newProviderError(t.provider, resp.StatusCode, data)
			if pe.Retryable && attempt < retries {
				if werr := waitForRetry(ctx, attempt); werr != nil {
					return pe
				}
				continue
			}
			return pe
		}

		if out == nil || len(bytes.TrimSpace(data)) == 0 {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s: decode response: %w", t.provider, err)
		}
		return nil
	}
	return fmt.Errorf("%s: request failed", t.provider)
}

func (t *Transport) DoStream(ctx context.Context, method string, url string, headers map[string]string, in any) (*http.Response, error) {
	// WTF??
	body, err := encodeBody(t.provider, in)
	if err != nil {
		return nil, err
	}

	retries := t.cfg.retries()
	for attempt := 0; attempt <= retries; attempt++ {
		resp, err := t.roundTrip(ctx, method, url, headers, body)
		if err != nil {
			if attempt < retries && ctx.Err() == nil {
				if werr := waitForRetry(ctx, attempt); werr != nil {
					return nil, fmt.Errorf("%s: %w", t.provider, err)
				}
				continue
			}
			return nil, fmt.Errorf("%s: request failed: %w", t.provider, err)
		}
		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			return resp, nil
		}
		data, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		pe := newProviderError(t.provider, resp.StatusCode, data)
		if pe.Retryable && attempt < retries {
			if werr := waitForRetry(ctx, attempt); werr != nil {
				return nil, werr
			}
			continue
		}
		return nil, pe
	}
	return nil, fmt.Errorf("%s: request failed", t.provider)
}

func (t *Transport) roundTrip(ctx context.Context, method string, url string, headers map[string]string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	for k, v := range t.cfg.Headers {
		req.Header.Set(k, v)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}

	return t.cfg.HTTPClient.Do(req)
}

func encodeBody(provider string, in any) ([]byte, error) {
	if in == nil {
		return nil, nil
	}
	body, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("%s: encode request: %w", provider, err)
	}
	return body, nil
}

func waitForRetry(ctx context.Context, attempt int) error {
	if attempt > 5 {
		attempt = 5
	}
	delay := time.Duration(1<<uint(attempt)) * 250 * time.Millisecond

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
