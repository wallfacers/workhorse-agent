# prompt-memory delta

## REMOVED Requirements

### Requirement: Memory file location and naming

**Reason**: The two-flat-file model (`MEMORY.md` + `USER.md`) is replaced by a
single per-entry SQLite store (see ADDED "Memory entry store"). Memory is no
longer file-shaped, so file location/naming requirements no longer apply.

### Requirement: Path safety for memory writes

**Reason**: Writes now target SQLite rows, not files under `memories/`, so the
file-path-traversal/`O_NOFOLLOW` requirement is obsolete. The only file output is
the read-only `memory export` artifact, whose path resolves through the standard
`pathguard` like any other write tool.

## MODIFIED Requirements

### Requirement: Snapshot loading at session start

The system SHALL read the memory entry store exactly once when a session is
created and bind the assembled two-layer snapshot to that session as an immutable
value. The snapshot MUST be loaded before the first system prompt is constructed
for that session. The snapshot is assembled, not raw: it contains the full content
of all `pinned` entries plus a manifest (one `name — trigger` line) for all
non-pinned entries.

#### Scenario: New session loads current store state

- **WHEN** a session is created via `POST /sessions`
- **THEN** the server reads `memory_entries` at session-creation time and attaches
  the assembled `(pinned content, manifest)` snapshot to the session as a read-only
  value

#### Scenario: Snapshot is immutable for the session lifetime

- **WHEN** any code path attempts to mutate a session's loaded memory snapshot
  after the session has started
- **THEN** the attempt MUST be rejected and the snapshot returned to callers
  remains byte-identical to what was assembled at session start

#### Scenario: Subsequent session reflects store changes

- **WHEN** entries have been added, edited, evicted, or merged (by tools or by
  curation) between session A finishing and session B starting
- **THEN** session B's snapshot reflects the post-change store, while session A's
  snapshot remained byte-identical for its full duration

### Requirement: System prompt injection

The system SHALL inject the memory snapshot into the system prompt of every
session via the single `{{.Memory}}` template variable rendered by
`internal/prompt`, positioned after the base, `<environment>`, and
`<instructions>` segments (order `base → environment → instructions → memory`).
The rendered block SHALL contain a `PINNED:` region (full content of pinned
entries, sorted by name) followed, when non-pinned entries exist, by a separator
and an `INDEX:` region (one `name — trigger` line per non-pinned entry, sorted by
score desc then name). When the store is empty the variable expands to an empty
string and the system prompt MUST NOT contain any memory framing.

#### Scenario: Non-empty memory rendered with stable delimiters

- **WHEN** a session has at least one entry
- **THEN** the rendered system prompt contains a `<memory>` block whose PINNED and
  INDEX regions are byte-stable across turns of the same session (so prompt-cache
  prefixes remain identical), positioned after base/environment/instructions

#### Scenario: Pinned ordering is deterministic

- **WHEN** the snapshot is assembled
- **THEN** pinned entries appear in `name`-ascending order and non-pinned manifest
  lines appear in `score desc, name asc` order, so the rendered bytes are a pure
  function of store state

#### Scenario: Empty store produces no memory section

- **WHEN** the store contains no entries
- **THEN** the rendered system prompt contains no memory-related text, headers, or
  delimiters

### Requirement: Character-limit enforcement on writes

The system SHALL enforce limits as Unicode code points (NOT bytes) at four
levels: per-entry content (`memory.entry_content_max_chars`, default 1200),
per-entry trigger (`memory.trigger_max_chars`, default 120, and `trigger` MUST be a
single line — embedded newlines are rejected), total pinned content
(`memory.pinned_budget_chars`, default 1500), and the manifest region
(`memory.manifest_budget_chars`, default 2000). A `memory_write` whose entry
content or trigger exceeds its per-entry limit, or whose creation/pinning would
push total pinned content over the pinned budget, MUST be rejected atomically — the
store MUST NOT be modified. There is no whole-store hard cap; total size is
governed by curation.

#### Scenario: Entry within per-entry limit succeeds

- **WHEN** `memory_write` is called with content at or below `entry_content_max_chars`
- **THEN** the entry is upserted and the tool returns `{accepted: true, char_count,
  char_limit}`

#### Scenario: Over-size entry rejected without store mutation

- **WHEN** `memory_write` content would exceed `entry_content_max_chars`
- **THEN** the tool returns a structured error `memory_too_large` reporting `limit`
  and `actual`, and `memory_entries` is unchanged

#### Scenario: Pinning over the pinned budget is rejected

- **WHEN** writing or pinning an entry would push total pinned content over
  `pinned_budget_chars`
- **THEN** the write is rejected with `pinned_budget_exceeded` reporting the budget
  and the resulting total, and no entry is created or pinned

#### Scenario: Over-length or multi-line trigger rejected

- **WHEN** `memory_write` is called with a `trigger` exceeding `trigger_max_chars`
  or containing a newline
- **THEN** the write is rejected (`trigger_invalid`) and the store is unchanged

#### Scenario: Code-point counting handles CJK correctly

- **WHEN** content contains multi-byte CJK characters
- **THEN** counts are Unicode code points (e.g. "你好" counts as 2), not byte length

### Requirement: memory_read tool

The system SHALL expose a `memory_read` tool that returns the current stored
content of a named entry along with metadata, reading directly from the store (NOT
the frozen snapshot) so the agent observes writes made earlier in the same session.
`memory_read` MUST NOT count as a usage hit (it does not bump `hit_count` or
`last_used_at`), so curation review does not pollute eviction scores.

#### Scenario: Read returns current store content

- **WHEN** `memory_read` is called with an existing `name`
- **THEN** the tool returns `{content, char_count, pinned, durability, category,
  hit_count, last_used_at}` read from the store at call time

#### Scenario: Read of missing entry returns not-found

- **WHEN** `memory_read` is called with a `name` that does not exist
- **THEN** the tool returns a structured `not_found` result and creates nothing

#### Scenario: Read does not affect usage score

- **WHEN** `memory_read` returns an entry
- **THEN** that entry's `hit_count` and `last_used_at` are unchanged

### Requirement: memory_write tool

The system SHALL expose a `memory_write` tool that creates or updates **exactly one**
entry (its input schema accepts a single entry, never an array). Fields: `name`
(required, slug key for upsert), `trigger`, `content`, optional `pinned`,
`durability`, `category`, and `mode` (`upsert` default, or `append` to concatenate
onto existing content). The write MUST be performed in a DB transaction and MUST
NOT modify the current session's already-injected snapshot.

#### Scenario: Upsert by name

- **WHEN** `memory_write` is called with a `name` that already exists, `mode:
  upsert`, and accepted content
- **THEN** the existing entry's fields are replaced and `updated_at` advances; a new
  `name` instead inserts a new entry

#### Scenario: Append mode concatenates under transaction

- **WHEN** `memory_write` is called with `mode: append` on an existing entry and the
  combined content is within `entry_content_max_chars`
- **THEN** the new content is concatenated after existing content (with a separating
  newline if needed) within a single transaction, and the entry's `char_count` and
  `updated_at` reflect the combined content

#### Scenario: Append exceeding per-entry limit is rejected

- **WHEN** `memory_write` is called with `mode: append` and the combined
  (existing + new) content would exceed `entry_content_max_chars`
- **THEN** the tool returns `memory_too_large` and the existing entry is left
  byte-identical (the append does not partially apply)

#### Scenario: Schema rejects batch input

- **WHEN** a caller supplies an array of entries instead of a single entry object
- **THEN** the tool rejects the input with a validation error and writes nothing

#### Scenario: Successful write surfaces delayed-effect hint

- **WHEN** `memory_write` succeeds during an active session
- **THEN** the response includes `next_session_effective: true`

### Requirement: Delayed-effect semantics

The system MUST guarantee that successful `memory_write`, `memory_delete`, and
`memory_merge` calls during an active session do NOT modify that session's
already-injected snapshot, nor change the system prompt rendered for any subsequent
turn of the same session.

#### Scenario: Write during session does not change snapshot

- **WHEN** an agent mutates memory mid-session and the call succeeds
- **THEN** subsequent turns of the same session render the system prompt with the
  pre-mutation snapshot

#### Scenario: Mutation effect observable only at next session

- **WHEN** session A mutates memory and then session B is created
- **THEN** session B's snapshot reflects the post-mutation store

## ADDED Requirements

### Requirement: Memory entry store

The system SHALL persist memory as rows in a `memory_entries` SQLite table in the
existing store database, one row per fact, with columns: `id` (ULID), `name`
(unique slug), `trigger`, `content`, `pinned` (bool), `durability`
(`evergreen`|`volatile`), `category`, `hit_count`, `last_used_at`, `created_at`,
`updated_at`, `char_count`, `source_session_id`. A `memory_entries_fts` FTS5
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

### Requirement: Pinned entries always loaded

The system SHALL load the full content of every `pinned` entry into the PINNED
region of the snapshot at session start, subject to `pinned_budget_chars`. Pinned
entries are never eligible for curation eviction.

#### Scenario: Pinned content appears in full

- **WHEN** a session starts and pinned entries exist within the pinned budget
- **THEN** their full content appears verbatim in the PINNED region, name-sorted

#### Scenario: Pinned entry is exempt from eviction

- **WHEN** the curation scorer runs
- **THEN** pinned entries are excluded from eviction candidates and clusters

### Requirement: Manifest layer with non-silent overflow

The system SHALL represent every non-pinned entry in the snapshot as a single
`name — trigger` manifest line, bounded by `manifest_budget_chars`. When the full
manifest exceeds the budget, the system SHALL include the highest-scored entries up
to the budget, append a final visible line stating how many entries are not shown
and to use `MemorySearch`, and log a `WARN` with the dropped count. The system MUST
NOT silently drop entries.

#### Scenario: Manifest within budget lists all entries

- **WHEN** all non-pinned manifest lines fit within `manifest_budget_chars`
- **THEN** every non-pinned entry has exactly one `name — trigger` line in INDEX

#### Scenario: Manifest overflow is surfaced, not silent

- **WHEN** the manifest would exceed `manifest_budget_chars`
- **THEN** the INDEX region contains the highest-scored lines up to the budget plus
  a final line `… N more memories not shown; use MemorySearch`, and a `WARN` records
  N

### Requirement: LoadMemory tool

The system SHALL expose a read-only, parallel-safe `LoadMemory` tool that returns
the full content of an entry by `name`. A successful load records a usage hit
(`hit_count++`, `last_used_at=now`) as an idempotent best-effort side effect that
MUST NOT block or fail the tool result.

#### Scenario: Load returns full content by name

- **WHEN** `LoadMemory` is called with an existing `name`
- **THEN** it returns the entry's full content

#### Scenario: Load records a usage hit best-effort

- **WHEN** `LoadMemory` succeeds
- **THEN** `hit_count` is incremented and `last_used_at` set; if recording the hit
  fails, the tool still returns the content and reports success

#### Scenario: Load of missing entry errors

- **WHEN** `LoadMemory` is called with a `name` that does not exist
- **THEN** it returns a structured `not_found` error

### Requirement: MemorySearch tool

The system SHALL expose a read-only, parallel-safe `MemorySearch` tool that runs an
FTS5 MATCH over `memory_entries_fts` for a `query`, reusing the `session-archive`
CJK-trigram synthesis and LIKE fallback, returning `{name, trigger, snippet}` rows
up to an optional `limit`. It is the recall backup when the manifest does not
surface a needed entry.

#### Scenario: Search returns ranked matches

- **WHEN** `MemorySearch` is called with a `query` matching entry text
- **THEN** it returns matching entries as `{name, trigger, snippet}`, and the model
  can then `LoadMemory` chosen names

#### Scenario: CJK query falls back when MATCH is empty

- **WHEN** a CJK `query` yields no FTS MATCH rows
- **THEN** the tool applies the trigram/LIKE fallback (as in `session_search`) before
  returning

### Requirement: memory_delete tool

The system SHALL expose a `memory_delete` tool that removes one entry by `name` in a
DB transaction. Pinned entries are deletable but only by an explicit call.

#### Scenario: Delete removes entry and FTS mirror

- **WHEN** `memory_delete` is called with an existing `name`
- **THEN** the row and its `memory_entries_fts` mirror are removed; a subsequent
  session no longer lists it

### Requirement: memory_merge tool

The system SHALL expose a `memory_merge` tool that atomically writes a merged entry
and deletes the named source entries in a single transaction. It is the concrete
deduplication action emitted by curation and available to the agent.

#### Scenario: Merge consolidates sources atomically

- **WHEN** `memory_merge{names:[a,b], into:{...}}` is called
- **THEN** within one transaction the `into` entry is written and entries `a` and
  `b` are deleted; a failure leaves all of `a`, `b`, and `into` in their pre-call
  state

### Requirement: Flat-file migration

The system SHALL perform a one-time, idempotent migration of legacy
`MEMORY.md`/`USER.md` into entries on startup when a migration marker is absent and
legacy files exist. `USER.md` becomes a single `pinned` `evergreen` `user` entry;
`MEMORY.md` is split on markdown sections into `volatile` entries (a non-splittable
block becomes one entry). Legacy files are copied to `memories/legacy/` and a marker
is written. Re-running the migration is a no-op.

#### Scenario: USER.md migrates to a pinned entry

- **WHEN** migration runs with a non-empty `USER.md`
- **THEN** one pinned `category=user` `durability=evergreen` entry holds its content

#### Scenario: MEMORY.md splits into entries

- **WHEN** migration runs with a sectioned `MEMORY.md`
- **THEN** each section becomes an entry (trigger defaulting to its first sentence
  when absent), defaulting to `durability=volatile`

#### Scenario: Migration is idempotent and preserves originals

- **WHEN** the server restarts after a completed migration
- **THEN** no entries are re-imported, and the legacy files remain under
  `memories/legacy/`
