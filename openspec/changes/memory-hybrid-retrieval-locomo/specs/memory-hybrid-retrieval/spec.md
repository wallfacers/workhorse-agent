# memory-hybrid-retrieval

## ADDED Requirements

### Requirement: Three-signal RRF fusion

The system SHALL provide a retrieval API (`retrieval.Search(ctx, query, k)`)
that computes up to three independent rankings over memory entries — (1)
semantic: cosine similarity between the embedded query and `memory_embeddings`
vectors, computed in Go; (2) keyword: the existing FTS5 BM25 MATCH ranking with
CJK-trigram synthesis and LIKE fallback; (3) entity: exact-match count of
normalized query tokens against `memory_entities.entity_norm` — and fuses them
with Reciprocal Rank Fusion (`score = Σ 1/(60 + rank)`), returning the top-k
entries by fused score with deterministic name tiebreak.

#### Scenario: Signals agree

- **WHEN** an entry ranks highly in all available signals for a query
- **THEN** it appears at or near the top of the fused result

#### Scenario: Semantic-only match is still found

- **WHEN** a query shares no keywords with an entry but is semantically close
  (e.g., paraphrase) and embedding is configured
- **THEN** the entry is retrievable via the vector signal despite zero BM25 and
  entity hits

### Requirement: Graceful signal degradation

Retrieval SHALL never fail because a signal is unavailable. With no embedding
client, fusion runs over BM25 + entity; with no entity rows, over BM25 alone —
which SHALL be behaviorally identical to the pre-feature `MemorySearch` path.
Signal errors (e.g., embedding endpoint down at query time) demote to the
remaining signals with a WARN.

#### Scenario: No embedding configured

- **WHEN** `memory.embedding` is unconfigured and a search runs
- **THEN** results come from BM25 + entity fusion, with no error surfaced to the
  caller

#### Scenario: Empty store

- **WHEN** a search runs against a store with no entries
- **THEN** an empty result set is returned without error

### Requirement: Time-aware result rendering

Each retrieval result SHALL carry the entry's `event_date` (when set) and
`created_at`, and tool-facing renderings SHALL include them as
`[event: <date>]` and `[recorded: <date>]` prefixes so the answering model can
perform temporal disambiguation between accumulated ADD-only versions of a fact.

#### Scenario: Dated instances distinguishable

- **WHEN** two entries record different values of the same evolving fact with
  different `event_date`s and both are retrieved
- **THEN** both results expose their dates, allowing the consumer to select the
  temporally correct instance
