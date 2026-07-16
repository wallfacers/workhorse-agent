# prompt-memory (delta)

## MODIFIED Requirements

### Requirement: Memory entry store

The system SHALL persist memory as rows in a `memory_entries` SQLite table in the
existing store database, one row per fact, with columns: `id` (ULID), `name`
(unique slug), `trigger`, `content`, `pinned` (bool), `durability`
(`evergreen`|`volatile`), `category`, `hit_count`, `last_used_at`, `created_at`,
`updated_at`, `char_count`, `source_session_id`, `event_date` (nullable unix
micros — when the remembered fact occurred), and `fact_source` (`''`, `user`,
`agent`, or `extraction` — provenance of the fact). A `memory_entries_fts` FTS5
virtual table SHALL mirror `(name, trigger, content)`, kept in sync via AFTER
INSERT/UPDATE/DELETE triggers, mirroring the `session-archive` mechanism.

#### Scenario: Entry persisted with metadata

- **WHEN** `memory_write` creates an entry
- **THEN** a `memory_entries` row is written with `created_at`/`updated_at` set,
  `hit_count=0`, `last_used_at` NULL, and `char_count` equal to the content's
  code-point count

#### Scenario: FTS mirror stays in sync

- **WHEN** an entry is inserted, updated, or deleted
- **THEN** the corresponding `memory_entries_fts` row is created, updated, or removed
  by trigger so a `MemorySearch` MATCH reflects the change

#### Scenario: Unique name enforced

- **WHEN** a write targets an existing `name`
- **THEN** it upserts that row rather than creating a duplicate

#### Scenario: Provenance and event date persisted

- **WHEN** the extraction pipeline stores a fact carrying an event date
- **THEN** the row records `fact_source=extraction` and the resolved
  `event_date`; entries written by `memory_write` keep `fact_source=''` and
  NULL `event_date` unless explicitly provided

### Requirement: MemorySearch tool

The system SHALL expose a read-only, parallel-safe `MemorySearch` tool that runs
hybrid retrieval (three-signal RRF fusion per `memory-hybrid-retrieval`, or its
degraded forms) for a `query`, returning `{name, trigger, snippet}` rows —
each annotated with `[event: <date>]` and `[recorded: <date>]` when available —
up to an optional `top_k` (default 8, maximum 50; the legacy `limit` field
remains accepted as an alias). It is the recall backup when the manifest does
not surface a needed entry.

#### Scenario: Search returns ranked matches

- **WHEN** `MemorySearch` is called with a `query` matching entry text
- **THEN** it returns matching entries as `{name, trigger, snippet}` ranked by
  fused score, and the model can then `LoadMemory` chosen names

#### Scenario: CJK query falls back when MATCH is empty

- **WHEN** a CJK `query` yields no FTS MATCH rows and no other signal is
  available
- **THEN** the tool applies the trigram/LIKE fallback (as in `session_search`)
  before returning

#### Scenario: top_k bounds respected

- **WHEN** `MemorySearch` is called with `top_k: 200`
- **THEN** the tool caps the result set at 50 and reports the cap in its output

### Requirement: memory_delete tool

The system SHALL expose a `memory_delete` tool that removes one entry by `name` in a
DB transaction, cascading to the entry's `memory_embeddings` and
`memory_entities` rows. Pinned entries are deletable but only by an explicit call.

#### Scenario: Delete removes entry and FTS mirror

- **WHEN** `memory_delete` is called with an existing `name`
- **THEN** the row and its `memory_entries_fts` mirror are removed; a subsequent
  session no longer lists it

#### Scenario: Delete cascades to side tables

- **WHEN** `memory_delete` removes an entry that has embedding and entity rows
- **THEN** those `memory_embeddings` and `memory_entities` rows are removed in
  the same operation

### Requirement: memory_merge tool

The system SHALL expose a `memory_merge` tool that atomically writes a merged entry
and deletes the named source entries in a single transaction, cascading source
deletions to `memory_embeddings` and `memory_entities` and enqueueing a
write-behind embed for the merged entry. It is the concrete deduplication action
emitted by curation and available to the agent.

#### Scenario: Merge consolidates sources atomically

- **WHEN** `memory_merge{names:[a,b], into:{...}}` is called
- **THEN** within one transaction the `into` entry is written and entries `a` and
  `b` are deleted; a failure leaves all of `a`, `b`, and `into` in their pre-call
  state

#### Scenario: Merge refreshes derived rows

- **WHEN** a merge completes while embedding is configured
- **THEN** the sources' embedding/entity rows are gone and the merged entry is
  queued for embedding
