## 1. Protocol types (`internal/api/protocol`, `internal/session`)

- [x] 1.1 Add `ClientMessageType` constants `publish_frontend_tools` and `frontend_tool_result` in `internal/session` (alongside the existing five) and register them in the protocol package's client-message decoder so unknown-type handling still works
- [x] 1.2 Define payload structs: `PublishFrontendToolsPayload{ Catalog []FrontendToolEntry }` where `FrontendToolEntry{ Name, Description string; InputSchema, OutputSchema json.RawMessage; ParallelSafety string }` (empty `ParallelSafety` defaults to `"unsafe"`), and `FrontendToolResultPayload{ ToolUseID string; Result json.RawMessage }`
- [x] 1.3 Add server event-type constants `frontend_tool_use` and `frontend_tools_published`

## 2. Frontend bridge + proxy tool (`internal/tools/frontend`, new package)

- [x] 2.1 `Bridge`: `pending map[string]chan *tools.Result` under a mutex + an emit func bound to the session; `Call(ctx, name, input)` mints a ULID (via `internal/idgen`), registers a channel, emits `frontend_tool_use {tool_use_id, name, input}`, then `select` on the channel and `ctx.Done()` (cleanup pending on ctx path)
- [x] 2.2 `Bridge.Resolve(id string, envelope json.RawMessage)`: parse the `{ok,value}|{ok:false,error}` envelope → `*tools.Result` (ok→Output=json(value),IsError=false; !ok→Output=error.message,IsError=true); non-blocking send to the pending channel; drop if id unknown
- [x] 2.3 `Tool` implementing `tools.Tool`: `Name/InputSchema` from the catalog entry; `CanRunInParallel()` = `parallelSafety=="safe"` (empty → false); `IsReadOnly()` **always false** (independent dimension; agent can't verify client side effects; only `CanRunInParallel` drives `BatchTools`); `DefaultTimeout()` returns 0; `Run` = `bridge.Call`. `OutputSchema` is retained on the struct and folded into `Description()` (no `Tool` interface method exists for it) so the model sees the output shape
- [x] 2.4 Unit tests: resolve-by-id, ok/error envelope mapping, ctx-cancel cleanup, unknown-id drop, parallel-safety flags

## 3. Session plumbing (`internal/session`)

- [x] 3.1 Add `Frontend FrontendResolver` interface to `Session` (nil until first publish); construct lazily so existing sessions are unaffected — Bridge takes an emit closure (no import cycle). Per-session registry clone happens lazily in `ensureClonedRegistry` on first publish (not a runner-factory precondition for regular sessions — only adapter-generator sessions are pre-cloned)
- [x] 3.2 Track per-session registered frontend tool names so re-publish can unregister the prior set

## 4. Loop + registration (`internal/agent/loop.go`)

- [x] 4.1 On `publish_frontend_tools`: handle **in `dispatchIdle`** (the `Run`→Inbox case, same level as `ClientUserMessage`) — register and return to the select loop **without starting a turn**; build proxy `Tool`s, `Registry.Register` each into the session's cloned tool surface, collecting `registered`/`rejected` (duplicate-name error → rejected, server-side retained); unregister the previous frontend set first (track the prior names per §3.2)
- [x] 4.2 Emit `frontend_tools_published {registered, rejected}` after registration
- [x] 4.3 Ensure the per-session tool surface is a clone (so frontend tools don't leak across sessions) — `ensureClonedRegistry` lazily clones `l.Registry` and creates a new `Orchestrator` pointing at the clone on first publish
- [x] 4.4 Confirm the orchestrator already yields `is_error` on timeout/cancel/panic for frontend `Run` (no new code; covered by an integration test)

## 5. HTTP routing (`internal/api/stream_post.go`)

- [x] 5.1 Extend `stateAccepts`: admit `frontend_tool_result` while `Executing`; admit `publish_frontend_tools` while `Idle`; reject `frontend_tool_result` while `Idle`
- [x] 5.2 Route `frontend_tool_result` in a **dedicated `case`** that calls `sess.Frontend.Resolve(payload.ToolUseID, payload.Result)` directly — **does NOT go through `Inbox`** (mirrors the `permission_decision` case at `stream_post.go:117-143`, which writes its channel directly); 202 always (unknown id is inert)
- [x] 5.3 Route `publish_frontend_tools` to `sess.Inbox` (the default path, so the loop's `dispatchIdle` handles it per §4.1) when Idle; 409 + SSE-mirror when not Idle (mirror the compact handler)

## 6. Verification

- [x] 6.1 `gofmt`/`gofumpt` clean; `golangci-lint run` clean
- [x] 6.2 Unit tests for §2 and the protocol decode pass
- [x] 6.3 Integration test (mirroring existing stream/permission tests): publish a 1-tool catalog → drive a turn where the model calls it → assert `frontend_tool_use` emitted, POST a `frontend_tool_result`, assert the turn completes with the value; and a timeout case asserting `is_error`
- [x] 6.4 Collision test: publish an entry colliding with a server-side tool → assert it lands in `rejected` and the server-side tool still runs
- [x] 6.5 End-to-end with workhorse-assistant: client connects, publishes its catalog, model opens a tab / reads a button (cross-repo; coordinate the `/stream` alignment on the client side)

## 7. Cross-repo follow-up (workhorse-assistant, separate repo)

> Tracked here for coordination; lands in the client repo on its own cadence.

- [x] 7.1 Align the Rust bridge (`src-tauri/src/agent/mod.rs`) to the real protocol: `POST /v1/sessions` with `{provider, model, workdir}` and read `id`; use `POST /v1/sessions/{id}/stream` with `ClientMessage{type,payload}` envelopes for `publish_frontend_tools` and `frontend_tool_result`; subscribe `GET …/stream` and relay `frontend_tool_use`
- [x] 7.2 Update `contract.md` to the real endpoint/message/event names
