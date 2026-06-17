# configuration delta

## ADDED Requirements

### Requirement: Memory configuration block

The system SHALL accept a `memory` configuration block validated at load time, with
the following keys and defaults: `pinned_budget_chars` (1500),
`manifest_budget_chars` (2000), `entry_content_max_chars` (1200),
`trigger_max_chars` (120), and a `curation` sub-block: `entry_count_high` (80),
`min_interval_minutes` (30), `lease_ttl_seconds` (60), `judge_model` (a small
cheap model id), `max_candidates_per_pass` (20), and `weights` (`hit`, `recency`,
`age`, `volatility`, defaults 1.0/1.0/0.5/0.5). All numeric values are code-point
counts or plain numbers. Invalid values (negative, non-numeric, or a
`lease_ttl_seconds` ≤ 0) MUST fail validation at startup.

The hot-reloadable subset is exactly `curation.entry_count_high`,
`curation.min_interval_minutes`, and `curation.lease_ttl_seconds`. All other keys —
`pinned_budget_chars`, `manifest_budget_chars`, `entry_content_max_chars`,
`trigger_max_chars`, `curation.judge_model`, `curation.max_candidates_per_pass`, and
`curation.weights` — are restart-only; a reload that changes them MUST be ignored
with a `WARN`. `weights` is deliberately restart-only so that an in-progress pass,
which snapshots its parameters at pass start, cannot have its scoring shift
mid-pass.

#### Scenario: Defaults applied when block omitted

- **WHEN** `config.yaml` contains no `memory` block
- **THEN** the documented defaults are used and the server starts normally

#### Scenario: Invalid value fails fast at load

- **WHEN** `memory.curation.lease_ttl_seconds` is set to 0 or a negative number
- **THEN** config validation rejects it at startup with a clear error and the server
  does not start

#### Scenario: Hot-reloadable curation values apply without restart

- **WHEN** a running server's `memory.curation.entry_count_high` is changed and a
  reload is triggered (directory watch or `SIGHUP`)
- **THEN** the new water line takes effect for subsequent curation triggers without
  a restart, while a changed `pinned_budget_chars` is ignored with a `WARN` until
  restart
