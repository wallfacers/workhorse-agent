# Repo guidance for AI coding assistants

## Project shape

Go 1.22+, single binary, local single-user multi-session AI agent server. Specs
live under `openspec/changes/init-data-agent-mvp/`. Treat those specs as the
source of truth: proposal, design, the per-capability `specs/*/spec.md`, and
the implementation backlog in `tasks.md`.

When implementing a task from `tasks.md`, mark its checkbox `[x]` immediately
after the work lands, and keep the surrounding spec text untouched unless the
task explicitly says to edit it.

## Compliance — read first, then code

This project is built under "path C": independent reimplementation guided by a
published reference architecture. The license and the public framing depend on
the following invariants holding everywhere in the repo, in every commit:

1. **No verbatim copies.** Do not paste code, identifiers, error strings,
   on-disk paths, schema column names, or directory layouts from any other
   agent runtime, including but not limited to Claude Code's source.
2. **No transliteration.** Renaming `foo_bar` to `fooBar` does not make a copy
   original. If you find yourself reproducing a structure piece by piece, stop
   and re-derive from the spec.
3. **Public protocols are fine.** MCP 2025-11-25 Streamable HTTP, SSE,
   Anthropic Messages API, OpenAI Chat Completions API — these are
   vendor-published interfaces and we follow them exactly. The internal
   `Message` / `ContentBlock` types are this project's own and bridge to those
   APIs.
4. **No vendor SDKs.** Hand-write thin HTTP clients against the published
   request/response shapes. Specifically: do not import
   `github.com/anthropics/anthropic-sdk-go` or
   `github.com/openai/openai-go`.
5. **No WebSocket library.** The transport is MCP Streamable HTTP (POST + GET
   SSE) over `net/http`. SSE only.

## Code style

- Default to no comments. Add one only when the *why* would surprise a future
  reader (a non-obvious invariant, a workaround for a specific OS quirk, a
  reference to a spec scenario by name).
- Don't preface comments with "added for task 5.13" or similar. The git history
  carries that.
- Follow `gofmt`/`gofumpt` output. `golangci-lint run` must stay clean.
- Avoid `panic` outside `main` and `init`. Agent loop has a top-level
  `recover()` and synthesizes a cancelled tool_result + emits
  `error{code:"internal_panic"}` instead of crashing the session.

## Network posture

- Bind `127.0.0.1` by default. Never bind `0.0.0.0` unless an operator
  explicitly sets `server.host`.
- Bearer-token comparison uses `crypto/subtle.ConstantTimeCompare`. The token
  value must never appear in logs, traces, or error messages.
- Origin enforcement is exact-host match via `url.Parse`, not string contains.

## Dangerous-command guard (Bash tool)

Eight pattern families force a permission prompt regardless of any
`allow_permanent` rule:

`rm -rf /`, `rm -rf ~`, `dd of=/dev/`, `mkfs.*`, redirect-to-block-device,
fork bomb, `chmod -R 777 /`, `shutdown`/`reboot`/`halt`/`poweroff`,
`base64 -d | sh` / `curl | bash`.

Known bypasses (hex escapes, absolute paths, alias indirection, base64
decoding into `sh`) are documented and explicitly **not** caught by MVP. Tests
must assert the bypasses are not caught — that's the spec, not a regression.

## Bash env isolation

The Bash tool strips a precise set of environment variables before exec:

- Exact match: `LD_PRELOAD`, `LD_LIBRARY_PATH`, `LD_AUDIT`,
  `DYLD_INSERT_LIBRARIES`, `DYLD_LIBRARY_PATH`,
  `DYLD_FALLBACK_LIBRARY_PATH`, `DYLD_FORCE_FLAT_NAMESPACE`,
  `PYTHONPATH`, `PYTHONSTARTUP`.
- Prefix: any variable starting with `DYLD_`.
- `NODE_OPTIONS` is shlex-tokenized; if any token starts with `--require`,
  `--import`, `--experimental-loader`, `--inspect`, or `--inspect-brk`, the
  variable is dropped. Other tokens pass through.

This logic lives in `internal/tools/bash/envfilter.go` and is shared by every
session-level env merge.

## Path traversal

All file-touching tools (Read, Write, Edit, plus any MCP adapter that touches
the filesystem) MUST resolve user-supplied paths via
`internal/tools/pathguard`:

1. `filepath.Clean`
2. `filepath.EvalSymlinks` (with a parent-directory fallback if the leaf does
   not exist yet — Write/Edit case)
3. `filepath.Rel` against the session workdir; reject if it escapes
4. Open with `O_NOFOLLOW` on Linux/macOS; on other platforms re-check with
   `os.Lstat` after open

## Persistence

`modernc.org/sqlite` only. No CGO. Events table uses `INTEGER PRIMARY KEY
AUTOINCREMENT` and is append-only; the `idx` value is the SSE `id:` field.
Session/message/agent IDs are ULIDs.

## Hot reload

`config.yaml` does NOT hot-reload (requires restart). Only
`~/.dataagent/agents/*.yaml` and `~/.dataagent/skills/*/skill.yaml` are
re-scanned dynamically.
