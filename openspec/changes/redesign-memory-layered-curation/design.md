# Design

## Context

This redesigns the `prompt-memory` capability from two flat files to a per-entry,
layered, self-curating store, and adds a `memory-curation` capability. It reuses
two existing patterns verbatim where possible: the `skills` subsystem's
manifest + on-demand-load shape (`internal/skills/{loader,injector,loadtool}.go`)
and the `session-archive` FTS5 approach (`messages_fts` + triggers, CJK trigram
synthesis + LIKE fallback in `internal/tools/sessionsearch/`).

Load-bearing invariants carried over unchanged from the current design:
- The per-session memory snapshot is **immutable** for the session lifetime.
- Mid-session writes update the store but **not** the snapshot (delayed effect →
  next session), to preserve the Anthropic prompt-cache prefix.
- Code points (not bytes) are the unit for all char limits.

## D1: Single per-entry store replaces two flat files

`memory_entries` (SQLite, in the existing store DB; `modernc.org/sqlite`, no CGO):

| column | type | purpose |
|---|---|---|
| `id` | TEXT (ULID) | primary key |
| `name` | TEXT UNIQUE | slug; `LoadMemory` key; manifest left column |
| `trigger` | TEXT | "when relevant" hook; manifest right column |
| `content` | TEXT | full body |
| `pinned` | INTEGER (0/1) | always-loaded full content |
| `durability` | TEXT | `evergreen` \| `volatile` (eviction + review cadence) |
| `category` | TEXT | facet: `user`/`feedback`/`project`/`reference` |
| `hit_count` | INTEGER | LFU signal |
| `last_used_at` | INTEGER (unix) | LRU signal; NULL until first load |
| `created_at` / `updated_at` | INTEGER | age signal / audit |
| `char_count` | INTEGER | cached code-point count of `content` |
| `source_session_id` | TEXT | provenance |

`memory_entries_fts` is an FTS5 virtual table mirroring `(name, trigger, content)`,
kept in sync by AFTER INSERT/UPDATE/DELETE triggers — identical mechanism to
`messages_fts`. Backfilled on migration.

**Why SQLite over file-per-entry:** the requested capabilities — metadata-driven
eviction scoring, FTS recall, and multi-process locking — all want a transactional
store with columns and a native FTS index. Files would force a directory scan for
scoring, a separate FTS index to maintain, and file-lock-only concurrency. The one
cost (entries are not hand-editable as files) is mitigated by a `memory export`
command (D7).

## D2: Two-layer snapshot assembly

At session start the `Loader` reads the store **once** and builds an immutable
snapshot with two regions, in a fixed order chosen for cache-prefix stability:

```
<memory>
PINNED:
{full content of every pinned entry, sorted by name}
---
INDEX:
- {name} — {trigger}      (every non-pinned entry)
...
</memory>
```

- **PINNED region**: full content of all `pinned` entries (user identity/prefs,
  evergreen rules). Sorted by `name` for byte-stability.
- **INDEX region (manifest)**: one `name — trigger` line per non-pinned entry,
  sorted by **score desc, then name** so that if the manifest must be truncated
  the highest-value entries survive deterministically.

Empty store → empty `<memory>` block omitted entirely (current behavior).

Injection happens at the existing `internal/agent/loop.go` call site; the
`internal/prompt` `{{.Memory}}` variable and `base → environment → instructions →
memory` ordering are unchanged.

## D3: Two prompt budgets, never silent truncation

- `memory.pinned_budget_chars` (P): total PINNED content ≤ P. Exceeding P is a
  **write-time rejection** when creating/pinning an entry would push pinned total
  over P (`memory_too_large`-style error) — forces an explicit trade-off, never a
  silent drop at assembly.
- `memory.manifest_budget_chars` (M): INDEX region ≤ M. If all non-pinned manifest
  lines exceed M, include highest-scored lines up to M and append a final visible
  line `… N more memories not shown; use MemorySearch`, plus a `slog.Warn` with the
  dropped count. The model is always told coverage is partial and has FTS recall as
  backup.

Per-entry: `memory.entry_content_max_chars` (C, default 1200) bounds a single
entry's `content`, and `memory.trigger_max_chars` (default 120) bounds `trigger` to
a single short line (newlines in `trigger` are rejected). Both are enforced at
`memory_write` time. There is **no** whole-store hard cap — total size is governed
by curation (D5), not by rejecting writes.

## D4: Tools (entry-shaped; registered through existing tool registry)

Read-only + parallel-safe: `LoadMemory`, `MemorySearch`, `memory_read`.
Write (serial): `memory_write`, `memory_delete`, `memory_merge`.

- `LoadMemory{name}` — returns full content; **side effect**: `hit_count++`,
  `last_used_at=now`. Mirrors `skills/loadtool.go`. The scoring side effect is an
  idempotent async usage bump that must not block the tool result nor break the
  read-only contract (see D8).
- `MemorySearch{query, limit?}` — FTS5 MATCH over `memory_entries_fts` (reusing
  `sessionsearch` CJK-trigram + LIKE fallback); returns `{name, trigger, snippet}`
  rows; the model then `LoadMemory`s the ones it wants.
- `memory_read{name}` — direct full read by name; **does not** count as a usage hit
  (for curation review without polluting scores).
- `memory_write{name, trigger, content, pinned?, durability?, category?, mode:
  upsert|append}` — writes **exactly one** entry (the tool schema does not accept
  arrays — closes the "batch create silently fails" footgun). Enforces C; updates
  the store, not the live snapshot.
- `memory_delete{name}` — removes one entry (pinned deletable, but only explicitly).
- `memory_merge{names:[...], into:{name, trigger, content, ...}}` — atomically
  writes the merged entry and deletes the sources. This is the concrete action the
  curation LLM step (and a human) emit to deduplicate.

## D5: Curation engine — pressure trigger → score → LLM judge

1. **Pressure trigger (deterministic, "when"):** after a successful `memory_write`,
   if `entry_count > entry_count_high` OR estimated manifest bytes > M OR time
   since last pass > `min_interval_minutes`, enqueue a curation task. Enqueue is
   debounced/idempotent: a pending task suppresses duplicates.
2. **Eviction scoring (deterministic, zero-token, explainable):** per non-pinned
   entry, higher score = keep, lower = evict:
   `score = w_hit·norm(hit_count) + w_recency·recency(last_used_at)
           − w_age·age_penalty(created_at, durability) − w_volatility·volatility_penalty(durability)`.
   The four component functions are **defined**, not left to the implementer:
   - `norm(hit_count) = hit_count / (hit_count + 1)` → saturating, ∈ [0,1).
   - `recency(last_used_at) = 1 / (1 + days_since_last_use)`; a NULL `last_used_at`
     (never loaded) → `0`.
   - `age_penalty(created_at, durability) = min(days_since_created / D, 1.0)` where
     `D = 365` for `evergreen` and `D = 90` for `volatile` (≈4× steeper decay for
     volatile, within the 3–5× target).
   - `volatility_penalty(durability) = {evergreen: 0.0, volatile: 0.3}` (flat
     baseline distinct from the age term).

   Default weights `w_hit=1.0, w_recency=1.0, w_age=0.5, w_volatility=0.5`
   (`memory.curation.weights`). `pinned` entries are never scored/evicted. Produces
   (a) low-score eviction candidates and (b) near-duplicate clusters.

   **Near-duplicate detection (defined):** for each entry, take the normalized text
   `name + "\n" + trigger + "\n" + content` (lowercased, whitespace-collapsed).
   Use FTS5 as a cheap **pre-filter** (each entry's trigram-tokenized text as a
   MATCH query against the rest) to get candidate pairs without O(n²) full
   comparison, then compute the **exact Jaccard similarity over character
   trigrams** for each candidate pair; pairs with Jaccard ≥ `0.7` are unioned
   (union-find) into clusters. FTS alone is substring matching, not similarity, so
   it is only the pre-filter — the Jaccard threshold is the decision.
3. **LLM judgment ("how", background only):** a maintenance worker (separate
   goroutine, its own short model call) is handed candidates + clusters and decides
   keep/evict/merge, emitting `memory_delete`/`memory_merge`. **Never on the
   `memory_write` hot path.** On error it logs `WARN` and leaves the store
   untouched (fail-safe); the next water-line crossing retries.

   **Cost & bounds (so the judge prompt cannot blow up):**
   - **Model**: a configurable `memory.curation.judge_model`, defaulting to a small
     cheap model (e.g. Haiku-class), *not* the main agent model. Judgment is a
     bounded classification task, not open-ended reasoning.
   - **Candidate cap**: at most `memory.curation.max_candidates_per_pass` (default
     20) lowest-scored candidates + their clusters are sent per pass. Excess is
     deferred to the next pass (logged). With ~20 entries each being a one-line
     trigger + short content, a judge prompt is on the order of a few thousand input
     tokens and a small structured output — well under one cent per pass on a
     small model. The cap makes cost O(1) in store size, not O(n).

Weights and water lines are configurable (D6).

## D6: Multi-process safety — DB leader lease

`workhorse-agent` may run as multiple processes against the same profile.

- **WAL is already on** (`internal/store/sqlite/sqlite.go`: `PRAGMA journal_mode =
  WAL`, `PRAGMA busy_timeout`, writes via `BEGIN IMMEDIATE`), so multi-process
  concurrent read + single-writer is already the store's posture — this design
  relies on it, no change needed.
- **Concurrent writes**: all entry mutations are DB transactions; SQLite serializes
  writers and `busy_timeout` handles contention. This supersedes the current flock
  approach and removes a class of file-lock races.
- **Single curator**: table `memory_curation_lease(id=1, holder, expires_at,
  heartbeat_at)`.
  - **holder identity**: a process-unique token = `idgen.NewULID()` generated once
    at process startup, prefixed with `hostname:pid:` for debuggability (e.g.
    `host-a:4123:01J…`). This lets takeover logic distinguish "my lease" from
    "someone else's".
  - **acquire (CAS)**: `UPDATE memory_curation_lease SET holder=?, expires_at=now+ttl,
    heartbeat_at=now WHERE id=1 AND (expires_at < now OR holder=?self)`. The caller
    MUST check `rowsAffected == 1` — only then is the lease held. `rowsAffected == 0`
    means another process holds an unexpired lease → skip this pass.
  - **renewal**: while running a pass the holder renews on a `heartbeat_interval =
    lease_ttl_seconds / 3` (default 20s for the 60s TTL) timer, advancing
    `expires_at` and stamping `heartbeat_at=now`. Renewal uses the same CAS guarded
    by `holder=?self`; a failed renewal (rowsAffected 0 → lease was stolen after
    expiry) aborts the pass.
  - **`heartbeat_at` column**: observability/diagnostics only (last successful
    renewal wall-clock, surfaced in `GET /v1/diagnostics`); `expires_at` alone
    drives all acquire/takeover logic.
  - **takeover**: if the holder crashes, no renewals occur, `expires_at` falls into
    the past after at most one TTL, and the next process's acquire CAS succeeds.
    No deadlock — waiters never hold resources; takeover is pure expiry.
- An in-process `sync.Mutex` still guards against multiple goroutines in the same
  process curating concurrently (analogous to today's `writeMu`).

## D7: Migration (one-time, idempotent)

On startup, if a migration marker is absent and legacy files exist:
- `USER.md` content → one `pinned` entry (`category=user`, `durability=evergreen`).
- `MEMORY.md` → split on markdown sections/separators into entries; a block that
  cannot be split imports as a single entry left for later `memory_merge`/split.
  Missing `trigger` falls back to the first sentence; imported entries default to
  `durability=volatile` pending review.
- Legacy files are copied to `memories/legacy/` (not deleted); marker written.
Schema objects are created via the existing versioned migration framework
(`IF NOT EXISTS`, forward-only), mirroring the `messages_fts` migration.

## D8: Read-only tool with a usage-count side effect

`LoadMemory`/`MemorySearch` are declared read-only and parallel-safe, yet bump
`hit_count`/`last_used_at`. Resolution: the bump is an **idempotent best-effort
usage log**, mechanically a single **usage-logger goroutine** draining a small
**buffered channel**. After producing its result, the tool enqueues
`{name, ts}` onto the channel (non-blocking); the logger goroutine performs the
`UPDATE memory_entries SET hit_count = hit_count + 1, last_used_at = ? WHERE name = ?`
using the shared DB pool. If the channel is full (burst), the send is dropped and a
`DEBUG` line is logged — scoring is approximate by design, so a dropped bump is
acceptable. A failed UPDATE is logged at `DEBUG` and never surfaced. This keeps the
read-only/parallel contract honest (no write on the tool's own goroutine, no error
path to the caller) while still feeding the scorer, and bounds concurrency to one
writer goroutine. The channel-drain model is deterministically testable (flush +
assert).

## Alternatives considered

- **Keep flat files, add a third entry layer** — rejected: two coexisting memory
  mechanisms, conceptual redundancy, double maintenance.
- **Make the single flat file "smarter" (auto-compact) without per-entry split** —
  rejected: abandons layered loading (prompt stays whole-file).
- **Manifest-only recall (no FTS) or FTS-only recall (no manifest)** — rejected in
  favor of hybrid: manifest gives cheap always-on discovery; FTS backs up overflow
  and unanticipated queries, reusing infra that already exists.
- **Pure-code eviction (no LLM)** — rejected as the sole mechanism: cannot merge
  near-duplicates or judge semantic value; used here only to *select candidates*.
- **File-per-entry storage** — rejected: see D1.
