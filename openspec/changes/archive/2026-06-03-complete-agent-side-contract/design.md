## Context

The `decouple-project-from-launch-cwd` change (archived in workhorse-assistant) defined 3 sidecar-side contract changes that were documented but never implemented. The existing code at `internal/api/health.go:28-37` uses `os.Getwd()` as the `defaultWorkdir()` fallback — an accident of how the sidecar was launched, never a meaningful project. The `/v1/fs/list` handler (`fs.go:58`) confines paths to the global `cfg.DefaultWorkdir` rather than the request's scoped project root. And `GET /v1/sessions` without a `?workdir=` query returns only live in-memory sessions instead of the full persisted set across projects.

These three items complete the assistant's project-decoupling work at the wire level.

## Goals / Non-Goals

**Goals:**
- `default_workdir` in `/health` resolves to home directory, never the launch cwd
- `/v1/fs/list` confinement follows an explicit `?root=` query parameter
- `GET /v1/sessions` (no `workdir`) returns full persisted list with live overlay
- All existing tests updated; new tests for the changed branches

**Non-Goals:**
- Not changing the `?workdir=P` filter path (stays as-is)
- Not adding new endpoints or changing the wire protocol version

## Decisions

### D1: `defaultWorkdir()` resolution order (health + session creation)

**选择**: `cfg.DefaultWorkdir` > `os.UserHomeDir()` > omit/empty。同一优先级同时适用于 `/health` 的 `default_workdir` 字段和 session 创建的默认 workdir。

**替代方案**: 保留 `os.Getwd()` 作为最后 fallback → 被拒绝，因为 launch cwd 从不是有意义的 project。

**原因**: 用户 home 是一个稳定的、有意义的默认项目目录；当无法解析时返回空让 assistant 走 project picker。Launch cwd 对 health 和 session creation 同样无意义——两个路径的 `os.Getwd()` 是同源问题，应一起修复。

**实施**: `health.go` 的 `defaultWorkdir()` 和 `session/workdir.go` 的 `ValidateWorkdir("")` 都改。`ValidateWorkdir` 的空值 fallback 是全局行为（影响所有 session 创建路径，不止 HTTP POST），但 launch cwd 对所有路径都是同等无意义的默认值，全局改为 home 是正确行为。

### D2: `/v1/fs` request-scoped root via query param

**选择**: `GET /v1/fs/list?path=<dir>&root=<project_root>` — 显式 query param

**替代方案**: 从 session cookie/header 推导 workdir → 被拒绝，因为 project browser 在 session 创建前就需要浏览文件系统。

**原因**: 显式 query param 解耦了文件浏览和 session 生命周期；assistant 在打开项目的浏览器阶段即可使用。

### D3: No-workdir session list source

**选择**: `GET /v1/sessions` (no `?workdir=`) → `store.ListSessions(ctx, false)` (全量持久化) + live status overlay

**替代方案**: 保持 `manager.ListSessions()` (in-memory only) → 被拒绝，因为 idle session 不在内存中。

**原因**: `SessionProvider` 的 session-management 面板需要跨项目列出所有 sessions（rename/delete 操作需要全量视图）。

## Risks / Trade-offs

- **Home dir 不可解析**: 在极少数环境中 `os.UserHomeDir()` 可能失败（无 HOME、无 passwd entry） → **Mitigation**: 返回 empty/omit `default_workdir`，assistant 显示 project picker。
- **`/v1/fs` root 绕过**: 客户端可能传任意 `root` 值来浏览任意目录 → **Mitigation**: 这本来就是设计意图——用户可以选择任意目录作为 project；virtual-FS guard 仍然阻止 `/proc` 等敏感路径。
- **全量 session 列表性能**: 大量 session 时 `store.ListSessions` 可能返回很多行 → **Mitigation**: 当前按 project 分页的 session 数量有限；后续可加分页参数。
