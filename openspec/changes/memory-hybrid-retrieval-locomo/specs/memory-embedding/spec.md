# memory-embedding

## ADDED Requirements

### Requirement: OpenAI-compatible embedding client

The system SHALL provide an embedding client (`internal/embedding`) that calls an
OpenAI-compatible `POST {base_url}/embeddings` endpoint and returns one float32
vector per input text. The client SHALL be pure Go (stdlib HTTP, no CGO, no
third-party vector or ML libraries). Configuration lives under
`memory.embedding.{base_url, model, api_key, dimensions, timeout_seconds}` with
defaults targeting a local Ollama instance (`http://127.0.0.1:11434/v1`,
Chinese-friendly model such as `qwen3-embedding:0.6b`). When `dimensions > 0` it
SHALL be passed through to the endpoint. The `api_key` value MUST never appear
in logs, traces, or error messages.

#### Scenario: Batch embed round-trip

- **WHEN** `Embed(ctx, texts)` is called with N texts against a healthy endpoint
- **THEN** it returns N vectors in input order, each with the model's (or the
  configured `dimensions`) dimensionality

#### Scenario: Unconfigured embedding disables the client

- **WHEN** `memory.embedding.base_url` or `memory.embedding.model` is empty
- **THEN** no client is constructed (nil), and all vector-dependent features
  degrade per the hybrid-retrieval degradation matrix, with a single startup
  INFO noting semantic search is disabled

#### Scenario: Endpoint failure is non-fatal

- **WHEN** the endpoint times out or returns an error during a write-behind embed
- **THEN** the entry remains fully usable (FTS/entity searchable), a WARN is
  logged without the API key, and the vector is left for the backfill sweep

### Requirement: Vector side-table lifecycle

The system SHALL persist entry vectors in a `memory_embeddings` table
(`entry_name` PK, `model`, `dims`, `vec` BLOB of little-endian float32,
`updated_at` unix micros), created by schema migration v8. Vectors SHALL be
written asynchronously by a single background goroutine (write-behind), never
blocking entry writes. A backfill sweep at startup (and opportunistically after
writes) SHALL embed entries that have no vector row or whose stored `model`
differs from the configured model. Entry deletion and merge SHALL cascade to
this table.

#### Scenario: Write-behind embedding

- **WHEN** an entry is created while the embedding client is configured
- **THEN** the entry write returns without waiting on the embedding call, and a
  vector row for that entry appears once the background embed completes

#### Scenario: Model change triggers re-embed

- **WHEN** `memory.embedding.model` changes and the process restarts
- **THEN** rows whose `model` differs are re-embedded by the backfill sweep and
  overwritten with the new model's vectors

#### Scenario: Cascade on delete

- **WHEN** an entry is deleted or merged away
- **THEN** its `memory_embeddings` row is removed in the same operation
