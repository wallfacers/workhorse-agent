## 1. Instructions Package — Core Types and File Discovery

- [x] 1.1 Create `internal/instructions/` package with `snapshot.go` — define `File` struct (Path string, Content string) and `Snapshot` struct (Files []File, LoadedAt time.Time)
- [x] 1.2 Create `internal/instructions/loader.go` — implement `Loader` struct with `ProfileDir string` field and `Load(workdir string) (*Snapshot, error)` method
- [x] 1.3 Implement project-level findUp logic in `loader.go` — walk from workdir to git root (nearest `.git` ancestor), collect all instances of the first matching filename (`AGENTS.md` → `CLAUDE.md`), bottom-up order
- [x] 1.4 Implement global-level file check in `loader.go` — check `<profileDir>/AGENTS.md`, add to snapshot if exists
- [x] 1.5 Write tests for `loader.go` — cover: AGENTS.md priority over CLAUDE.md, monorepo nested files, non-git project, global-only, no files found, missing files treated as empty

## 2. Instructions Package — Block Rendering

- [x] 2.1 Create `internal/instructions/block.go` — implement `Block(snapshot *Snapshot) string` rendering `<instructions>` XML block with `Instructions from:` headers and `---` separators, nil-safe and empty-safe
- [x] 2.2 Write tests for `block.go` — cover: single file, multiple files, empty snapshot returns "", nil snapshot returns ""

## 3. Instructions Package — Proximity Injection Resolver

- [x] 3.1 Create `internal/instructions/resolver.go` — implement `Resolver` struct with system paths set, injected paths set (thread-safe), and `Resolve(filePath string, workdir string) []Injection` method
- [x] 3.2 Implement walk-up logic in `Resolver.Resolve` — from file's parent directory to workdir root, find instruction files not in system paths and not already injected, add to injected set, return results
- [x] 3.3 Write tests for `resolver.go` — cover: first injection succeeds, duplicate skipped, system-level path skipped, concurrent calls deduplicate, file in workdir root produces no injection

## 4. Prompt Module — Template Extension

- [x] 4.1 Update `SystemPromptInput` in `internal/prompt/builtins.go` — add `Instructions string` field
- [x] 4.2 Update `SystemPrompt` template — add `"{{if .Instructions}}\n\n{{.Instructions}}{{end}}"` between Environment and Memory segments
- [x] 4.3 Update `BuildSystemPrompt` function — pass `Instructions` field to template data map and fallback join
- [x] 4.4 Update `internal/prompt/boundary_test.go` if needed — verify no new forbidden imports
- [x] 4.5 Write tests for updated `BuildSystemPrompt` — cover: full assembly order (B→Cancel→E→I→M), instructions-only, empty instructions, instructions between env and memory

## 5. Session and Runner Factory Integration

- [x] 5.1 Add `InstructionSnapshot *instructions.Snapshot` field to `session.Session` struct
- [x] 5.2 Add `InstructionResolver *instructions.Resolver` field to `session.Session` struct (or lazy-init)
- [x] 5.3 Update runner factory in `cmd/workhorse-agent/cmd_serve.go` — create `instructions.Loader`, call `Load(sess.Workdir)`, store snapshot on session, create resolver from snapshot's file paths
- [x] 5.4 Update `runTurnLoop` in `internal/agent/loop.go` — pass `instructions.Block(l.Session.InstructionSnapshot)` as `Instructions` field to `BuildSystemPrompt`

## 6. Read Tool — Proximity Injection

- [x] 6.1 Extend `tools.Env` struct (or pass resolver through existing mechanism) to carry the `*instructions.Resolver`
- [x] 6.2 Update Read tool's `Run` method in `internal/tools/builtin/read.go` — after building output string, call resolver's `Resolve` method with the file path and session workdir, append any results as `<system-reminder>` blocks
- [x] 6.3 Write integration test — session with subdirectory AGENTS.md, Read triggers injection, second Read of sibling file skips it

## 7. Prompt-Memory Spec Delta — Assembly Order Update

- [x] 7.1 Verify the assembly order documented in `openspec/specs/prompt-memory/spec.md` reflects the new `base → environment → instructions → memory` ordering after archive
