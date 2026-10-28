package llm

import (
	"net/http"
	"time"
)

type Config struct {
	APIKey       string
	BaseURL      string
	DefaultModel string
	HTTPClient   *http.Client
	Headers      map[string]string
	MaxRetries   *int
	Timeout      time.Duration
}

func (c Config) WithDefaults() Config {
	if c.Timeout <= 0 {
		c.Timeout = 7 * time.Minute
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{Timeout: c.Timeout}
	}
	if c.Headers != nil {
		headers := make(map[string]string, len(c.Headers))
		for k, v := range c.Headers {
			headers[k] = v
		}
		c.Headers = headers
	}
	return c
}

func (c Config) retries() int {
	if c.MaxRetries == nil {
		return 2
	}
	if *c.MaxRetries < 0 {
		return 0
	}
	return *c.MaxRetries
}
