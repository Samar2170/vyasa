# Repo Map

`vyasa` is a Go CLI for building a retrieval-augmented research corpus over
Indian equities: it seeds NSE companies into PostgreSQL, collects market data
and investor-relations filings from pluggable sources, archives everything to
Archivus file storage, and turns the archived filings into a hybrid
(semantic + lexical) RAG index with cross-encoder reranking and an eval
harness. An agent layer (core loop, delegation hub, pipelines) builds on the
same LLM and tool interfaces.

## CLI Commands (`cmd/vyasa/main.go`)

| Command | What it does |
| --- | --- |
| `seed` | load companies from the NSE equity master list (EQUITY_L.csv) into PostgreSQL |
| `collect` | fetch market data for one company from `indiasm`, `tickertape`, `tickertapev2`, `av` sources concurrently, with fallbacks |
| `ir` | fetch and archive a company's IR filings: annual report, investor presentation, earnings-call transcript, latest quarterly financials |
| `rag-index` | chunk + embed + store every unindexed archived filing (PDF re-extracted for `[[page N]]` markers; `-force` re-indexes) |
| `rag-search` | hybrid semantic+keyword search over one company's filings, reranked by a cross-encoder |
| `rag-eval-gen` | draft a golden retrieval-eval dataset from indexed chunks (LLM writes one grounded question per sampled chunk) |
| `rag-eval` | replay a golden dataset through the production search; scores Hit@k / Recall@k / MRR / nDCG |

## Repository Tree

```
.
├── cmd/
│   └── vyasa/
│       └── main.go              # CLI entry point: wires .env + config.json + Postgres + Archivus, runs subcommands
├── seed/
│   └── equity/                  # package equity: parses the NSE equity master list (EQUITY_L.csv) into Company records
│       ├── equity.go
│       └── equity_test.go
├── src/
│   ├── agent/                   # package agent: core agent loop, delegation hub, pipelines, event stream
│   │   ├── agent.go             #   Agent = config + Run loop (LLM steps, tool calls); Runner interface; Result/FinishStatus
│   │   ├── hub.go               #   Hub: named agent registry; DelegateTools + Roster for orchestrators
│   │   ├── delegate.go          #   DelegateTool: exposes any Runner as a types.Tool (LLM-router delegation)
│   │   ├── pipeline.go          #   Pipeline: static multi-step chains of agents/Runners
│   │   ├── event.go             #   Event/EventSink: one event stream for streaming, persistence, observability
│   │   ├── tool_exec.go         #   tool execution: parallel calls, output truncation
│   │   ├── runopts.go           #   RunOption: streaming, event sinks, parent trace IDs, per-run step limits
│   │   ├── agent_test.go
│   │   └── DESIGN.md            #   design doc for the agent layer
│   ├── chunks/                  # package chunks: splits parsed filings into RAG chunks (rune offsets preserved)
│   │   ├── chunks.go            #   Chunk + context line model
│   │   ├── chunker.go           #   Chunker interface + FixedChunker (sliding window)
│   │   ├── annual.go            #   AnnualReportChunker: section-aware annual report chunking
│   │   ├── slides.go            #   PresentationChunker: slide-by-slide chunking of investor presentations
│   │   └── *_test.go
│   ├── client/                  # package client: DataSourceClient/StorageClient interfaces + shared JSON helpers
│   │   ├── client.go            #   interfaces + SaveUploadedPaths (records uploaded locations in saved_files)
│   │   ├── json.go              #   JSONEqual: semantic JSON comparison
│   │   ├── archivus/            #   package archivus: Archivus file-storage client (upload, download, lookup, folders)
│   │   │   ├── archivus.go      #     ArchivusClient: file upload/download/lookup against the Archivus API
│   │   │   ├── types.go         #     API response/request types (FileInfo, UploadFile…)
│   │   │   └── helpers.go       #     folder/path utilities
│   │   ├── av/                  #   package av: AlphaVantage market-data client
│   │   │   ├── av.go            #     DataSourceClient fetching + archiving AlphaVantage series/quotes
│   │   │   └── helpers.go       #     payload shaping helpers
│   │   ├── indiasm/             #   package indiasm: stock.indianapi.in client (indiasm.go)
│   │   ├── ir/                  #   package ir: investor-relations filing pipeline
│   │   │   ├── ir.go            #     pipeline: resolve company site/IR link → discover → archive filings
│   │   │   ├── classify.go      #     classifies candidate URLs/documents into analysis segments
│   │   │   ├── links.go         #     link extraction from IR pages
│   │   │   ├── listing.go       #     digs filings out of headless-CMS record-listing pages
│   │   │   ├── pdf.go           #     PDF text extraction ([[page N]] markers) — ExtractPDFText
│   │   │   ├── chat.go          #     LLM chat helper for ambiguous classification decisions
│   │   │   └── *_test.go
│   │   ├── tickertape/          #   package tickertape: extracts stock data from saved Tickertape HTML pages (fallback)
│   │   │   ├── tickertape.go    #     DataSourceClient reading saved pages from tickertape_files/
│   │   │   └── keyMappings.go   #     Tickertape JSON key → metric mappings
│   │   └── tickertapev2/        #   package tickertapev2: scrapes live Tickertape pages, daily request cap
│   │       ├── tickertapev2.go  #     live scraper; falls back to saved pages (package tickertape) on failure
│   │       └── *_test.go
│   ├── config/                  # package config: config.json Settings (non-secret) + .env secrets (godotenv)
│   │   ├── config.go            #   Load, LoadDotEnv, Secret/RequireSecret, defaults, RerankOn, Timeout
│   │   └── config_test.go
│   ├── eval/                    # package eval: retrieval-quality measurement against a golden dataset
│   │   ├── dataset.go           #   Item/ChunkKey golden-dataset model; LoadJSONL/SaveJSONL
│   │   ├── gen.go               #   Generate: samples indexed chunks, asks LLM to write one grounded question each
│   │   ├── run.go               #   Run: replays dataset through Searcher; Report aggregate + per-item JSONL output
│   │   ├── metrics.go           #   Metrics: Hit@k, Recall@k, MRR, nDCG@k (±1 window / strict modes)
│   │   └── *_test.go
│   ├── helpers/                 # package helpers: small shared utilities
│   │   └── helpers.go           #   RawString: renders a raw JSON value as a plain string
│   ├── llm/                     # package llm: provider-agnostic LLM layer
│   │   ├── client.go            #   ModelClient/Stream interfaces, Accumulator (streaming deltas)
│   │   ├── factory.go           #   provider registry: RegisterProvider / NewClient
│   │   ├── config.go            #   Config (api key, model, retries, timeout)
│   │   ├── transport.go         #   HTTP transport: POST, retries, backoff
│   │   ├── embeddings.go        #   Embedder: batch embeddings with provider routing
│   │   ├── rerank.go            #   Reranker: cross-encoder scoring; RerankBudget; RerankCache persistence interface
│   │   ├── budget.go            #   Budget: per-session LLM call/token caps
│   │   ├── tools.go             #   ToolSpecFromTool: bridge from types.Tool to provider tool specs
│   │   ├── types.go             #   message roles/content types
│   │   ├── errors.go            #   ProviderError
│   │   ├── *_test.go            #   tests for budget/embeddings/rerank/transport
│   │   ├── openrouter.go        #   openrouter registration glue
│   │   └── providers/
│   │       └── openrouter/      # package openrouter: OpenRouter chat-completions client
│   │           ├── client.go    #   ModelClient implementation (chat + streaming)
│   │           ├── convert.go   #   request/response translation ↔ wire format
│   │           ├── wire.go      #   wire types (JSON payloads)
│   │           └── *_test.go
│   ├── models/                  # package models: persisted domain types + PostgreSQL table names
│   │   ├── models.go            #   Company, CompanyLink, CompanyResearchResource, saved_files…
│   │   └── market.go            #   market-data tables (av_timeseries, indiasm_stock, tickertape, api_usage)
│   ├── rag/                     # package rag: the RAG core
│   │   ├── rag.go               #   Indexer: chunk → embed → store (idempotent per company+segment); Search: hybrid pool → rerank → top-K
│   │   ├── period.go            #   DerivePeriod: normalizes "FY 2025 - 26"/"Q1 FY27"/URL shapes into FY/Q labels
│   │   ├── tools.go             #   SearchTool: hybrid retrieval exposed to agents as a types.Tool (rag_search)
│   │   └── *_test.go
│   ├── store/                   # package store: PostgreSQL connection, schema, and all queries
│   │   ├── store.go             #   Store: idempotent schema creation (pgx), jsonb payloads + indexed columns
│   │   ├── tables.go            #   per-section tickertape tables (shared schema)
│   │   ├── company.go           #   GetCompany, CreateMissingCompanies (seed path)
│   │   ├── rag.go               #   RAG queries: insert/delete chunks, vector + lexical search, SearchRAGHybrid + FuseRRF, MarkResourceIndexed
│   │   ├── rerank_cache.go      #   persistent rerank-score cache (pair-hash keyed)
│   │   └── rag_test.go
│   └── types/                   # package types: Tool interface + Registry shared by agent, llm, rag
│       └── tool.go              #   Tool interface, ExecutionOutput, Registry
├── dataset/
│   └── rag_golden.jsonl         # curated golden retrieval-eval dataset (hand-reviewed draft from rag-eval-gen)
├── tickertape_files/            # saved Tickertape HTML pages read by the tickertape fallback client
│   ├── reliance_tickertape.html
│   └── WAAREEENER_tickertape.html
├── apitest/
│   └── openrouter.ipynb         # scratch notebook for OpenRouter API experiments
├── EQUITY_L.csv                 # NSE equity master list (seed input)
├── config.json                  # non-secret settings (provider, models, embedding dim, rerank, storage, caps)
├── .env                         # secrets (POSTGRES_DSN, ARCHIVUS_*, *_API_KEY) — not committed
├── .gitignore                   # ignores .env, research*, *_files/, dataset/*, __tbd__/*
├── go.mod / go.sum              # module vyasa (go 1.26); pgx/v5, godotenv, ledongthuc/pdf, golang.org/x/net
├── prompts.md                   # prompt-engineering notes
└── research.md                  # RAG-for-finance research notes
```

## Package Overview

| Package | Role |
| --- | --- |
| `cmd/vyasa` | CLI entry point; subcommand wiring and runner functions |
| `seed/equity` | Parse the NSE equity master CSV into companies |
| `src/agent` | Agent loop, hub/delegation, pipelines, event stream |
| `src/chunks` | Chunkers: fixed, section-aware annual reports, slide-aware presentations |
| `src/client` | Data-source + storage interfaces; concrete clients: `archivus`, `av`, `indiasm`, `ir`, `tickertape`, `tickertapev2` |
| `src/config` | `config.json` settings + `.env` secret loading |
| `src/eval` | Golden-dataset generation and retrieval metrics (Hit@k, Recall@k, MRR, nDCG) |
| `src/helpers` | Small shared utilities |
| `src/llm` | Provider-agnostic LLM clients, embeddings, reranker, budgets, transports; `providers/openrouter` |
| `src/models` | Domain types + table names (jsonb payloads, indexed columns) |
| `src/rag` | Indexer + hybrid search (RRF) + cross-encoder rerank + period normalization; `rag_search` agent tool |
| `src/store` | PostgreSQL schema and every query; persistent rerank cache |
| `src/types` | `Tool` interface + registry (shared contract between agent/llm/rag) |

## Data Flow

```
EQUITY_L.csv ──seed──▶ companies (Postgres)
                         │
              collect ───┼──▶ indiasm / av / tickertape* ──▶ market tables + Archivus archive
                         │
                    ir ──┴──▶ IR filings ──▶ company_research_resources + Archivus
                                              │
                                        rag-index (chunk ▸ embed ▸ store) ──▶ rag_chunks
                                              │
                     rag-search / rag-eval ◀──┘   (hybrid vector+lexical ▸ RRF ▸ rerank ▸ top-K)
```
