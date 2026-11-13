// Package config splits application configuration into two sources:
//
//   - Settings: non-secret values (provider, model, max_steps, timeouts)
//     loaded from a JSON file. Safe to commit.
//   - Secrets: API keys and tokens resolved from the process environment,
//     typically populated from a .env file. Secrets never live in the
//     settings file.
//
// Settings file example (config.json):
//
//	{
//	  "provider": "openrouter",
//	  "model": "z-ai/glm-5.3-flash",
//	  "max_steps": 20,
//	  "timeout_sec": 120,
//	  "max_retries": 3
//	}
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/joho/godotenv"
)

const (
	DefaultProvider   = "openrouter"
	DefaultMaxSteps   = 50
	DefaultTimeoutSec = 420 // 7 minutes, matches llm.Config's default
)

// Settings is the non-secret application configuration. Omitted fields fall
// back to defaults; Model and MaxRetries are optional and left to the
// downstream components (llm.Config, providers) to default.
type Settings struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	MaxSteps   int    `json:"max_steps"`
	TimeoutSec int    `json:"timeout_sec"`
	MaxRetries *int   `json:"max_retries"`
	// TickertapeMaxRequestsPerDay caps the live tickertapev2 stock-page
	// fetches per calendar day.
	TickertapeMaxRequestsPerDay int `json:"tickertape_max_requests_per_day"`
	// StorageParentFolder is the root Archivus folder all archived data is
	// filed under.
	StorageParentFolder string `json:"storage_parent_folder"`
	// StorageSchema is the PostgreSQL schema the store bootstraps and uses
	// for all tables. Empty disables bootstrapping (plain public default).
	StorageSchema string `json:"storage_schema"`

	EmbeddingModel string `json:"embedding_model"`
	EmbeddingDim   int    `json:"embedding_dim"`
	// EmbeddingProvider pins the OpenRouter upstream routing for /embeddings
	// (a JSON "order" list, e.g. ["Azure"]). Empty means OpenRouter's default
	// routing, whose embeddings are not reproducible across calls.
	EmbeddingProvider string `json:"embedding_provider"`

	// RerankModel is the cross-encoder reranker (OpenRouter /rerank) that
	// reorders the hybrid retrieval pool before truncation to topK.
	RerankModel string `json:"rerank_model"`
	// RerankPool is the number of hybrid candidates fetched before reranking.
	RerankPool int `json:"rerank_pool"`
	// RerankEnabled toggles the cross-encoder reranker. Nil means enabled.
	RerankEnabled *bool `json:"rerank_enabled"`
	// RerankBudget caps the cache-miss (query, document) pairs one run may
	// bill through the rerank API; 0 means unlimited. Cached pairs are free
	// and never consume it. One pair is ~285 API tokens (a document trimmed
	// to 800 runes + query + the instruction template), so a 62-item eval
	// sweep at pool 32 is ~2,000 pairs; repeat runs spend nothing because
	// every scored pair is cached.
	RerankBudget int `json:"rerank_budget"`
}

const (
	DefaultTickertapeMaxRequestsPerDay = 5
	DefaultStorageParentFolder         = "financial_data"
	DefaultStorageSchema               = "vyasa"

	DefaultEmbeddingModel = "openai/text-embedding-3-small"
	DefaultEmbeddingDim   = 1536

	DefaultRerankModel = "qwen/qwen3-reranker-8b"
	// DefaultRerankPool is 4x a typical topK: deep enough for the
	// cross-encoder to reorder, shallow enough to keep the per-search rerank
	// token bill (template + query repeated per document) small.
	DefaultRerankPool = 32
)

// Timeout returns the configured timeout as a duration.
func (s Settings) Timeout() time.Duration {
	return time.Duration(s.TimeoutSec) * time.Second
}

func (s *Settings) withDefaults() {
	if s.Provider == "" {
		s.Provider = DefaultProvider
	}
	if s.MaxSteps <= 0 {
		s.MaxSteps = DefaultMaxSteps
	}
	if s.TimeoutSec <= 0 {
		s.TimeoutSec = DefaultTimeoutSec
	}
	if s.TickertapeMaxRequestsPerDay <= 0 {
		s.TickertapeMaxRequestsPerDay = DefaultTickertapeMaxRequestsPerDay
	}
	if s.StorageParentFolder == "" {
		s.StorageParentFolder = DefaultStorageParentFolder
	}
	if s.StorageSchema == "" {
		s.StorageSchema = DefaultStorageSchema
	}
	if s.EmbeddingModel == "" {
		s.EmbeddingModel = DefaultEmbeddingModel
	}
	if s.EmbeddingDim <= 0 {
		s.EmbeddingDim = DefaultEmbeddingDim
	}
	if s.RerankModel == "" {
		s.RerankModel = DefaultRerankModel
	}
	if s.RerankPool <= 0 {
		s.RerankPool = DefaultRerankPool
	}
}

// RerankOn reports whether the cross-encoder reranker should run
// (nil rerank_enabled means enabled).
func (s Settings) RerankOn() bool {
	return s.RerankEnabled == nil || *s.RerankEnabled
}

// Load reads the JSON settings file at path and applies defaults. An empty
// path returns defaults without touching the filesystem. Unknown fields are
// rejected so typos in the config file fail fast instead of being silently
// ignored.
func Load(path string) (Settings, error) {
	var s Settings
	if path == "" {
		s.withDefaults()
		return s, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return Settings{}, fmt.Errorf("config: open %s: %w", path, err)
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Settings{}, fmt.Errorf("config: parse %s: %w", path, err)
	}
	s.withDefaults()
	return s, nil
}

// LoadDotEnv loads variables from the given .env files into the process
// environment. Already-set variables are never overwritten, so real
// environment variables take precedence over file values. Missing files are
// ignored; malformed files return an error. With no paths, ".env" is used.
func LoadDotEnv(paths ...string) error {
	if len(paths) == 0 {
		paths = []string{".env"}
	}
	for _, p := range paths {
		if err := godotenv.Load(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("config: load %s: %w", p, err)
		}
	}
	return nil
}

// Secret returns the value of an environment variable, or "" if unset.
// Use for secrets only; non-secret settings come from Settings.
func Secret(key string) string {
	return os.Getenv(key)
}

// RequireSecret returns the value of an environment variable, or an error
// naming the missing key.
func RequireSecret(key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		return "", fmt.Errorf("config: %s is not set", key)
	}
	return v, nil
}
