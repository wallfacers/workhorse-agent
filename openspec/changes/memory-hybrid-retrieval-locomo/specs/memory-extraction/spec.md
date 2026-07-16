# memory-extraction

## ADDED Requirements

### Requirement: ADD-only single-pass fact extraction

The system SHALL provide an extraction pipeline (`internal/memory/pipeline`)
that distills a batch of conversation messages into memory entries using exactly
one LLM call per batch. The LLM SHALL return strict JSON: an array of
`{fact, entities[], event_date?, category, durability}` objects. Each fact is
stored as a NEW entry (name = content-derived slug + short ULID suffix); the
pipeline SHALL never update or delete existing entries (ADD-only). Extracted
entities are stored in `memory_entities`; `fact_source` and `event_date` are
persisted on the entry. Facts stated or confirmed by the assistant (agent
actions) SHALL be extracted with the same weight as user statements.
Deduplication of accumulated near-duplicates is the responsibility of the
existing curation engine; pipeline writes SHALL fire the same curation pressure
hook as `memory_write`.

#### Scenario: Facts extracted from a message batch

- **WHEN** the pipeline ingests a batch containing "I moved from Sweden four
  years ago" (session dated 2023-05-20)
- **THEN** one LLM call produces at least one entry whose content captures the
  move, with entities including "Sweden", `event_date` resolved to ≈2019,
  `fact_source=extraction`, and the entry is immediately searchable

#### Scenario: ADD-only never mutates prior entries

- **WHEN** a later batch contradicts an earlier stored fact
- **THEN** the pipeline stores the new fact as a new entry with its own
  `event_date` and does not modify or delete the earlier entry

#### Scenario: Malformed LLM output is a no-op

- **WHEN** the extraction LLM returns unparseable or schema-violating output
- **THEN** the batch is skipped with a WARN, no entries are written, and the
  session is unaffected

### Requirement: Pipeline triggering and configuration

The pipeline SHALL be configured under `memory.pipeline.{enabled, extract_model}`
with `enabled` defaulting to `true` and `extract_model` defaulting to the value
of `memory.curation.judge_model` (`provider:model` form, resolved through the
same provider-caller mechanism as the curation judge). In production the
pipeline SHALL run at session end over messages accrued since the last ingest,
asynchronously (never blocking session teardown); failures are WARN + no-op.
Trivially short batches (no user/assistant content) SHALL be skipped without an
LLM call. A Go API SHALL allow direct invocation with an explicit session date
(used by the LoCoMo bench).

#### Scenario: Session end triggers extraction

- **WHEN** a session with substantive dialogue ends and `memory.pipeline.enabled`
  is true
- **THEN** extraction runs in the background and new entries become visible to
  the NEXT session (delayed-effect semantics preserved)

#### Scenario: Kill switch

- **WHEN** `memory.pipeline.enabled` is false
- **THEN** no extraction LLM calls are made and behavior matches the
  pre-feature system

#### Scenario: Missing extract model provider

- **WHEN** `extract_model` references an unconfigured provider
- **THEN** the pipeline is inert (mirrors the curation worker), logged once at
  startup
