# locomo-bench

## ADDED Requirements

### Requirement: Reproducible LoCoMo evaluation harness

The system SHALL provide a standalone `cmd/locomo-bench` binary that, per LoCoMo
conversation: creates a throwaway SQLite store, ingests each conversation
session through the extraction pipeline (with the session's date), answers each
question in a single LLM pass using only the top-k results from
`retrieval.Search` (default 50, configurable up to 200), and scores answers with
an LLM-as-a-Judge prompt aligned with the open mem0ai/memory-benchmarks
methodology. It SHALL report accuracy per LoCoMo category (single-hop,
multi-hop, temporal, open-domain; adversarial excluded) and overall. The
`--retrieval fts|hybrid` flag SHALL switch the retrieval backend so uplift is
measured A-B under identical extraction, answering, and judging.

#### Scenario: A-B uplift measurement

- **WHEN** the bench runs twice on the same sample with `--retrieval fts` and
  `--retrieval hybrid`
- **THEN** both runs share extraction/answer/judge configuration and produce
  per-category scores directly comparable for uplift

#### Scenario: Sampled cost-controlled run

- **WHEN** `--conversations 1 --questions 20` is passed
- **THEN** only that sample is ingested and answered, and the report labels the
  run as sampled

### Requirement: Resumable runs and credential hygiene

The bench SHALL persist per-question results as JSONL run artifacts and skip
already-answered questions on re-invocation with the same run directory
(resume). Model credentials SHALL come only from environment variables
(`LOCOMO_BASE_URL`, `LOCOMO_API_KEY`, `LOCOMO_MODEL`; embedding via
`memory.embedding.*` or its env equivalents) and MUST never be written to run
artifacts, logs, or reports.

#### Scenario: Resume after interruption

- **WHEN** a run is interrupted after 120 of 200 questions and re-invoked with
  the same run directory
- **THEN** the 120 recorded results are reused and only the remaining 80 are
  executed

#### Scenario: Key never persisted

- **WHEN** any run completes
- **THEN** no file under the run directory contains the API key
