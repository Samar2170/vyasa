// Package store manages the PostgreSQL connection and schema for all
// persisted domain data. Payload-style data lives in jsonb columns so the
// arbitrary API responses stay queryable; the fields the clients filter on
// are real columns with supporting indexes.
//
// The schema is created idempotently on boot, so New is safe to call on an
// existing database. Foreign keys are intentionally absent — the clients
// write rows for companies whose records live elsewhere.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const defaultEmbedDim = 1536

// Store wraps the connection pool.
type Store struct {
	Pool *pgxpool.Pool

	embedDim int
}

type Option func(s *Store)

func WithEmbeddingDim(dim int) Option {
	return func(s *Store) {
		s.embedDim = dim
	}
}

// New connects to the PostgreSQL instance at dsn and applies the schema.
// When schema is non-empty it is created if missing, every pooled connection
// gets `SET search_path TO <schema>` so unqualified SQL lands there, and a
// privilege check fails fast with a clear error when the DSN role cannot use
// it. An empty schema keeps the server default (usually public).
func New(ctx context.Context, dsn, schema string, opts ...Option) (*Store, error) {
	if dsn == "" {
		return nil, errors.New("store: postgres DSN is required")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parse DSN: %w", err)
	}
	cfg.MaxConnLifetime = time.Hour

	if schema != "" {
		sanitized := pgx.Identifier{schema}.Sanitize()
		cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
			_, err := conn.Exec(ctx, "SET search_path TO "+sanitized+", public")
			return err
		}
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	s := &Store{Pool: pool}
	if err := s.ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}

	for _, opt := range opts {
		opt(s)
	}

	if schema != "" {
		if err := s.bootstrapSchema(ctx, schema); err != nil {
			pool.Close()
			return nil, err
		}
	}
	if err := s.Migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// bootstrapSchema creates the working schema when missing and verifies the
// connection role can use it. Unqualified CREATE then resolves to this
// schema instead of falling through to public.
func (s *Store) bootstrapSchema(ctx context.Context, schema string) error {
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := s.Pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+ident+" AUTHORIZATION current_user"); err != nil {
		return fmt.Errorf("store: create schema %s: %w", schema, err)
	}
	var ok bool
	err := s.Pool.QueryRow(ctx, `SELECT has_schema_privilege(current_user, $1, 'CREATE')
		AND has_schema_privilege(current_user, $1, 'USAGE')`, schema).Scan(&ok)
	if err != nil {
		return fmt.Errorf("store: check privileges for schema %s: %w", schema, err)
	}
	if !ok {
		return fmt.Errorf("store: DSN role cannot create or use schema %q — grant it or adjust POSTGRES_DSN", schema)
	}
	return nil
}

func (s *Store) ping(ctx context.Context) error {
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.Pool.Ping(pctx); err != nil {
		return fmt.Errorf("store: ping: %w", err)
	}
	return nil
}

// Close releases the pool.
func (s *Store) Close() {
	if s != nil && s.Pool != nil {
		s.Pool.Close()
	}
}

// IsNotFound reports whether err is a no-rows result.
func IsNotFound(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

const schema = `
CREATE TABLE IF NOT EXISTS companies (
	id text PRIMARY KEY DEFAULT gen_random_uuid()::text,
	symbol text NOT NULL,
	exchange text NOT NULL DEFAULT '',
	name text NOT NULL DEFAULT '',
	industry text NOT NULL DEFAULT '',
	series text NOT NULL DEFAULT '',
	isin_code text NOT NULL DEFAULT '',
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now(),
	UNIQUE (symbol, exchange)
);

CREATE TABLE IF NOT EXISTS company_links (
	company_id text PRIMARY KEY,
	official_website text NOT NULL DEFAULT '',
	investor_relations_link text NOT NULL DEFAULT '',
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS company_research_resources (
	company_id text NOT NULL,
	analysis_segment text NOT NULL,
	source text NOT NULL DEFAULT '',
	research_resource_link text NOT NULL DEFAULT '',
	raw_data_url text NOT NULL DEFAULT '',
	extracted_data jsonb NOT NULL DEFAULT '{}'::jsonb,
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (company_id, analysis_segment)
);

CREATE TABLE IF NOT EXISTS saved_files (
	id text PRIMARY KEY DEFAULT gen_random_uuid()::text,
	company_id text NOT NULL,
	name text NOT NULL,
	extension text NOT NULL DEFAULT '',
	size double precision NOT NULL DEFAULT 0,
	folder_path text NOT NULL DEFAULT '',
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now(),
	UNIQUE (company_id, folder_path, name)
);

CREATE TABLE IF NOT EXISTS document_chunks (
	id text PRIMARY KEY DEFAULT gen_random_uuid()::text,
	company_id text NOT NULL,
	symbol text NOT NULL DEFAULT '',
	segment text NOT NULL DEFAULT '',
	ordinal integer NOT NULL DEFAULT 0,
	"offset" integer NOT NULL DEFAULT 0,
	text text NOT NULL DEFAULT '',
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS document_chunks_lookup
	ON document_chunks (company_id, segment);

CREATE TABLE IF NOT EXISTS av_timeseries (
	id text PRIMARY KEY DEFAULT gen_random_uuid()::text,
	company_id text NOT NULL,
	symbol text NOT NULL DEFAULT '',
	function text NOT NULL,
	date date NOT NULL,
	response jsonb NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS av_timeseries_lookup
	ON av_timeseries (company_id, function, date DESC);

CREATE TABLE IF NOT EXISTS av_global_quote (
	id text PRIMARY KEY DEFAULT gen_random_uuid()::text,
	company_id text NOT NULL,
	symbol text NOT NULL DEFAULT '',
	function text NOT NULL,
	date date NOT NULL,
	response jsonb NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS av_global_quote_lookup
	ON av_global_quote (company_id, function, date DESC);

CREATE TABLE IF NOT EXISTS indiasm_stock (
	id text PRIMARY KEY DEFAULT gen_random_uuid()::text,
	company_id text NOT NULL,
	symbol text NOT NULL DEFAULT '',
	date date NOT NULL,
	response jsonb NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS indiasm_stock_lookup
	ON indiasm_stock (company_id, date DESC);

CREATE TABLE IF NOT EXISTS tickertape (
	id text PRIMARY KEY DEFAULT gen_random_uuid()::text,
	company_id text NOT NULL DEFAULT '',
	symbol text NOT NULL,
	date date NOT NULL,
	response jsonb NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now(),
	UNIQUE (symbol, date)
);

CREATE TABLE IF NOT EXISTS api_usage (
	client text NOT NULL,
	date date NOT NULL,
	count integer NOT NULL DEFAULT 0,
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (client, date)
);
`

const tickerSectionSchema = `
CREATE TABLE IF NOT EXISTS %[1]s (
	id text PRIMARY KEY DEFAULT gen_random_uuid()::text,
	company_id text NOT NULL DEFAULT '',
	symbol text NOT NULL,
	section text NOT NULL DEFAULT '',
	date date NOT NULL,
	response jsonb NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now(),
	UNIQUE (symbol, date)
);

ALTER TABLE company_research_resources
	ADD COLUMN IF NOT EXISTS indexed boolean NOT NULL DEFAULT false;
`

const ragChunksSchema = `

CREATE TABLE IF NOT EXISTS rag_chunks (
	id text PRIMARY KEY DEFAULT gen_random_uuid()::text,
	company_id text NOT NULL,
	symbol text NOT NULL DEFAULT '',
	segment text NOT NULL DEFAULT '',
	document_url text NOT NULL DEFAULT '',
	ordinal integer NOT NULL DEFAULT 0,
	"offset" integer NOT NULL DEFAULT 0,
	section text NOT NULL DEFAULT '',
	text text NOT NULL DEFAULT '',
	embedding vector(%[1]d) NOT NULL,
	model text NOT NULL DEFAULT '',
	period text NOT NULL DEFAULT '',
	source text NOT NULL DEFAULT '',
	is_quarterly boolean NOT NULL DEFAULT false,
	tsv tsvector GENERATED ALWAYS AS (to_tsvector('english', text)) STORED,
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS rag_chunks_lookup
	ON rag_chunks (company_id, segment);
CREATE INDEX IF NOT EXISTS rag_chunks_vector
	ON rag_chunks USING hnsw (embedding vector_cosine_ops);
CREATE INDEX IF NOT EXISTS rag_chunks_lexical
	ON rag_chunks USING gin (tsv);

ALTER TABLE rag_chunks ADD COLUMN IF NOT EXISTS period text NOT NULL DEFAULT '';
ALTER TABLE rag_chunks ADD COLUMN IF NOT EXISTS source text NOT NULL DEFAULT '';
ALTER TABLE rag_chunks ADD COLUMN IF NOT EXISTS is_quarterly boolean NOT NULL DEFAULT false;
`

// Migrate creates the schema if it does not exist yet.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.Pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	if _, err := s.Pool.Exec(ctx, fmt.Sprintf(ragChunksSchema, s.embedDim)); err != nil {
		return fmt.Errorf("store: migrate rag_chunks: %w", err)
	}

	for _, table := range tickerSectionTables() {
		stmt := fmt.Sprintf(tickerSectionSchema, table)
		if _, err := s.Pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("store: migrate %s: %w", table, err)
		}
	}
	return nil
}
