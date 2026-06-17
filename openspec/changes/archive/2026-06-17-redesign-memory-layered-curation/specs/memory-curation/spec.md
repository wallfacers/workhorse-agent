# memory-curation delta

## ADDED Requirements

### Requirement: Pressure-triggered curation

The system SHALL enqueue a background curation pass after a successful entry write
when any water line is crossed: non-pinned `entry_count > memory.curation.entry_count_high`,
estimated manifest size > `memory.manifest_budget_chars`, or time since the last
completed pass > `memory.curation.min_interval_minutes`. Enqueue MUST be idempotent
and debounced — when a pass is already pending or running, no duplicate is enqueued.
Curation MUST NOT run on the `memory_write` request path; the write returns without
waiting for curation.

#### Scenario: Crossing a water line enqueues a pass

- **WHEN** a `memory_write` succeeds and pushes non-pinned `entry_count` above
  `entry_count_high`
- **THEN** a curation task is enqueued and the write returns immediately without
  performing curation inline

#### Scenario: Enqueue is debounced

- **WHEN** a curation task is already pending or running and another water line is
  crossed
- **THEN** no second task is enqueued

#### Scenario: Time-based fallback trigger

- **WHEN** no water line is crossed by count or size but the time since the last
  completed pass exceeds `min_interval_minutes` at the next write
- **THEN** a pass is enqueued so stale `volatile` entries are still reviewed

### Requirement: Deterministic eviction scoring

The system SHALL compute, with no LLM call, an eviction score per non-pinned entry
(higher = keep) as
`score = w_hit·norm(hit_count) + w_recency·recency(last_used_at) −
w_age·age_penalty(created_at, durability) − w_volatility·volatility_penalty(durability)`
with configurable weights `memory.curation.weights` (defaults
`hit=1.0, recency=1.0, age=0.5, volatility=0.5`) and the following component
definitions:

- `norm(hit_count) = hit_count / (hit_count + 1)` (∈ [0,1)).
- `recency(last_used_at) = 1 / (1 + days_since_last_use)`; a NULL `last_used_at`
  yields `0`.
- `age_penalty(created_at, durability) = min(days_since_created / D, 1.0)`, with
  `D = 365` for `evergreen` and `D = 90` for `volatile`.
- `volatility_penalty(durability) = 0.0` for `evergreen`, `0.3` for `volatile`.

`evergreen` entries therefore decay with age far more slowly than `volatile` ones.
Pinned entries SHALL be excluded. The scorer SHALL produce (a) an ordered list of
low-score eviction candidates and (b) near-duplicate clusters formed by an FTS5
pre-filter followed by exact character-trigram **Jaccard similarity ≥ 0.7** over the
entry's normalized `name + trigger + content` text, unioned into clusters.

#### Scenario: Score is an explainable function of metadata

- **WHEN** the scorer runs over the non-pinned entries
- **THEN** each entry's score is `w_hit·norm(hit_count) + w_recency·recency(last_used_at)
  − w_age·age_penalty(created_at) − w_volatility·penalty(durability)` using the
  configured weights, and is reproducible given the same store state

#### Scenario: Durability changes age sensitivity

- **WHEN** two entries are identical except one is `evergreen` and one is `volatile`
- **THEN** the `volatile` entry receives a steeper age penalty and ranks as a
  stronger eviction candidate

#### Scenario: Pinned entries are never candidates

- **WHEN** the scorer runs
- **THEN** no pinned entry appears in the eviction candidates or near-duplicate
  clusters

### Requirement: LLM curation judgment

The system SHALL run an LLM judgment step in a background maintenance worker that
receives the scorer's eviction candidates and near-duplicate clusters and decides
keep/evict/merge per entry or cluster, emitting `memory_delete` and `memory_merge`
operations. The step SHALL use the configurable `memory.curation.judge_model`
(defaulting to a small cheap model, not the main agent model) and SHALL send at most
`memory.curation.max_candidates_per_pass` (default 20) candidates per pass, deferring
the remainder to a later pass (logged), so judge cost is bounded independent of store
size. The step MUST NOT run on any user-facing request path. On any error the worker
SHALL log a `WARN` and leave the store unchanged, retrying at the next trigger
(fail-safe).

#### Scenario: LLM merges a near-duplicate cluster

- **WHEN** the scorer surfaces a cluster of near-duplicate entries and the LLM judges
  them mergeable
- **THEN** the worker emits a `memory_merge` consolidating them into one entry

#### Scenario: LLM evicts a low-value candidate

- **WHEN** the LLM judges a low-score candidate no longer useful
- **THEN** the worker emits a `memory_delete` for it

#### Scenario: Worker failure leaves the store intact

- **WHEN** the LLM call or an emitted operation fails mid-pass
- **THEN** the store is left in a consistent pre-failure state, a `WARN` is logged,
  and the next trigger retries

### Requirement: Single-curator leader lease

The system SHALL ensure at most one process performs curation at a time, even when
multiple `workhorse-agent` processes share a profile, via a `memory_curation_lease`
row acquired with an optimistic conditional update (`WHERE expires_at < now()` CAS).
A process that loses the CAS SHALL skip curation. The holder SHALL heartbeat to
extend `expires_at`; on holder crash the lease SHALL expire after
`memory.curation.lease_ttl_seconds` and another process MAY then acquire it. An
in-process mutex SHALL additionally prevent concurrent passes within one process.

#### Scenario: Only one process acquires the lease

- **WHEN** two processes attempt curation simultaneously
- **THEN** exactly one wins the CAS and runs the pass; the other observes a held,
  unexpired lease and skips

#### Scenario: Crashed holder's lease is taken over after TTL

- **WHEN** the lease holder crashes without releasing the lease
- **THEN** after `lease_ttl_seconds` the `expires_at` is in the past and another
  process can acquire the lease on its next attempt

#### Scenario: Concurrent writes during curation are transactionally safe

- **WHEN** a `memory_write` from another process commits while a curation pass runs
- **THEN** both proceed via DB transactions without corrupting `memory_entries`, and
  curation operates on a consistent committed view

### Requirement: Entry mutations are transactional

The system SHALL perform all curation entry mutations (`memory_delete`,
`memory_merge`) within DB transactions so that a partial or failed pass never leaves
a half-merged or orphaned state, and the FTS mirror remains consistent with
`memory_entries`.

#### Scenario: Failed merge rolls back fully

- **WHEN** a `memory_merge` emitted by curation fails after writing the merged entry
  but before deleting a source
- **THEN** the transaction rolls back so neither the merged entry nor the deletion
  persists, and the FTS mirror matches the rolled-back state
