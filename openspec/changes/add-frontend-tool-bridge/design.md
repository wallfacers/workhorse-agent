## Context

All current tools execute in-process via the orchestrator
(`internal/agent/orchestrator.go`): `RunAll` batches by `CanRunInParallel`,
wraps each `Tool.Run` in `context.WithTimeout`, and maps timeout/cancel/panic to
an `is_error` `tools.Result`. The session state machine
(`internal/session/session.go`) and the `POST|GET /v1/sessions/{id}/stream`
protocol (`internal/api/stream_post.go`, `stream_get.go`) already implement one
**blocking round-trip to the client**: permission approval. The loop transitions
to `AwaitPerm`, emits an event, and `permission.Manager.Check()` blocks until the
client POSTs a `permission_decision` that `startInboxWatcher`
(`loop.go:320`) forwards into `Session.PermissionAnswers`.

A frontend (proxy) tool is the same shape: block in `Tool.Run`, emit an event,
await a client POST. This change adds that round-trip and the per-session
registration that feeds it, reusing the orchestrator's existing timeout/error
machinery rather than inventing new ones.

## Goals / Non-Goals

**Goals**
- Let the model call client-defined UI tools as ordinary tools, with results fed
  back through the normal `tool_result` path.
- Keep all UI tool definitions on the client; the agent hardcodes none.
- Reuse orchestrator timeout/cancel/panic → `is_error` (no parallel mechanism).

**Non-Goals**
- No new session state (frontend tools run inside `Executing`; no `AwaitPerm`
  analogue is needed because the orchestrator already blocks there).
- No persistence of catalogs beyond session lifetime.
- No change to how server-side tools execute.

## Decisions

### D1 — Proxy tool reuses the orchestrator's timeout/error handling
The frontend `Tool.Run(ctx, env, input)` blocks on a `select` over its result
channel and `ctx.Done()`. The orchestrator already wraps `Run` in
`context.WithTimeout` and maps `DeadlineExceeded`/`Canceled`/panic to
`is_error` (`orchestrator.go:184-228`). So **task 4.2 (timeout → is_error) needs
no new code** — Run just honours `ctx`.

### D2 — Correlate by a bridge-generated id, not the LLM `tool_use_id`
`Tool.Run` receives `(ctx, env, input)` — **not** the `tool_use_id` (the
orchestrator keeps it in `ToolCall.ID` and never passes it down). So each `Run`
mints its own round-trip id (ULID via `idgen`), emits it in `frontend_tool_use`,
and awaits a `frontend_tool_result` carrying the same id. The client treats the
id as opaque and echoes it. This id is independent of the model's
`tool_use_id`.

*Naming note*: the wire field is called `tool_use_id` in both
`frontend_tool_use` and `frontend_tool_result` for client-contract continuity
(the client sees exactly one opaque id and echoes it). It is **deliberately not**
the Anthropic-API `tool_use_id` — it is this bridge correlation id. Readers
debugging the agent should treat the two as distinct namespaces.

### D3 — Per-session `Bridge` on the Session, guarded by `mu`
`Session` gains a `frontend FrontendResolver` field (unexported, guarded by `mu`,
accessed via `SetFrontend`/`Frontend` accessors) — nil until a catalog is
published. The Bridge holds `pending map[string]chan *tools.Result` under a
mutex and an emit closure bound to the session. `Run` registers a channel under
its id, emits, and selects on channel/`ctx`; on `ctx.Done()` it removes its
pending entry to avoid leaks. The `stream_post` handler resolves directly:
`sess.Frontend().Resolve(id, envelope)` does a map lookup + non-blocking send —
no extra goroutine or watcher edge needed (simpler than the permission channel
because there is no shared single answer channel; each call has its own).

### D4 — Proxy Tool instances are built per session, bound to the Bridge
Catalog → `Tool` construction happens per session, so each `frontend.Tool`
captures the session Bridge directly. `Tool` implements `tools.Tool`:
`Name/Description/InputSchema` from the catalog entry; `CanRunInParallel()` =
`parallelSafety == "safe"`; `DefaultTimeout()` returns 0 (inherit config); `Run`
delegates to `bridge.Call`.

`IsReadOnly()` is **always false** — it is an independent dimension from
parallel-safety (cf. Dispatch: `IsReadOnly=false, CanRunInParallel=true`), it
means "produces no state changes" (`tool.go:21`), and the agent cannot verify
whether a client-side tool mutates UI state. Only `CanRunInParallel()` drives
`BatchTools` (`orchestrator.go:82`); `IsReadOnly()` is metadata, so a
conservative `false` is correct and loses nothing. `parallelSafety` therefore
maps to `CanRunInParallel()` alone; `outputSchema` (no `Tool` interface method
exists for it) is retained on the struct and folded into `Description` so the
model still sees the output shape, rather than adding an interface method (YAGNI).

### D5 — Dynamic registration via `publish_frontend_tools`, per-session registry
A new client message `publish_frontend_tools` (payload: the catalog array) is
accepted while **Idle** (before a turn, so registry mutation never races a
running batch), routed through `Session.Inbox` and handled in the loop's
`dispatchIdle` (`loop.go` Run→Inbox case) — register, emit, return to the select
loop **without** starting a turn. The runner factory only clones the registry
for adapter-generator sessions (`cmd_serve.go:554`); regular sessions share the
global registry. So `ensureClonedRegistry` lazily clones on first publish
(tracked by `registryCloned bool` on Loop) and creates a per-session
Orchestrator pointing at the clone, ensuring frontend tools never leak across
sessions. `parallelSafety` defaults to `"unsafe"` when an entry omits it
(conservative). Collisions with an existing server-side tool are
rejected (`Registry.Register` returns an error on duplicate names —
`registry.go:34`), the server-side tool is retained, and the rejection is
reported. Re-publishing **replaces** the prior frontend set: the loop tracks the
names it registered last time and unregisters them first
(`Registry.Unregister`). Outcome is emitted as `frontend_tools_published`
`{registered:[...], rejected:[{name,reason}]}`.

### D6 — Protocol additions on the existing `/stream` endpoint
No new routes. On `POST /v1/sessions/{id}/stream`:
- `publish_frontend_tools` — accepted while `Idle`; rejected with `409` +
  SSE-mirror otherwise (same pattern as compact).
- `frontend_tool_result` — accepted while `Executing` (extend `stateAccepts`,
  `stream_post.go:164`); routed to `sess.Frontend.Resolve`.
Server events added: `frontend_tool_use` `{tool_use_id, name, input}` and
`frontend_tools_published` `{registered, rejected}`. Both flow through
`Session.Emit` → Outbox → SSE with the standard `idx` wrapper, so ordering and
Last-Event-ID replay come for free (no extra `seq` needed; that was a client-side
concern on the Tauri event bus, not here).

### D7 — Result envelope → `tools.Result`
The client posts `{ok:true, value}` or `{ok:false, error:{kind,message}}`. The
Bridge maps: `ok:true` → `Result{Output: json(value), IsError:false}`;
`ok:false` → `Result{Output: error.message, IsError:true}`. The model then sees
a normal (possibly `is_error`) `tool_result`.

## Wire contract (authoritative for the client)

`POST /v1/sessions` already returns the session view with the id under **`id`**
(`sessions.go:26`); it **requires** `provider`, `model`, `workdir`. The client's
Rust bridge must send those (correcting an earlier assumption of an empty body).

Client → server (`POST …/stream`, `ClientMessage{type,payload}`):
- `publish_frontend_tools` → `{ "catalog": [ {name, description, inputSchema, outputSchema, parallelSafety} ] }`
- `frontend_tool_result` → `{ "tool_use_id": "<bridge id>", "result": {ok,...} }`

Server → client (SSE events):
- `frontend_tool_use` → `{ "tool_use_id", "name", "input" }`
- `frontend_tools_published` → `{ "registered":[...], "rejected":[{name,reason}] }`

## Risks / Trade-offs

- **Client disconnects mid-call** → no `frontend_tool_result` arrives → the
  orchestrator's per-tool timeout fires → `is_error` result (D1). Pending entry
  is cleaned up in `Run`'s `ctx.Done()` branch.
- **Catalog published mid-turn** → disallowed (Idle-only, D5), so no registry
  mutation races a running batch.
- **A parallel batch of frontend tools** → each `Run` has its own pending
  channel keyed by its own id, so concurrent calls don't collide; `parallelSafety`
  controls whether the orchestrator batches them at all.
- **Cross-repo wire drift** → the client (workhorse-assistant) had assumed
  `/events` and `/tools` endpoints; this design fixes the names to the real
  `/stream` protocol. The client repo must align (coordinated follow-up there).

## Migration Plan

Additive. Sessions that never receive `publish_frontend_tools` behave exactly as
today. Rollback = stop registering the message/event types; no server-side tool
behaviour changes.

## Open Questions

- Whether to also accept `publish_frontend_tools` while `Thinking` (to let the
  client refresh mid-turn for the *next* batch). V1: Idle-only for safety.
- A late-arriving `frontend_tool_result` (after the tool timed out and the turn
  transitioned from `Executing` to `Thinking`) gets 409 instead of the spec's
  "unknown id is inert 202" behaviour, because `stateAccepts` only admits it in
  `Executing`. Low impact (client would need to retry), tracked for V2.
