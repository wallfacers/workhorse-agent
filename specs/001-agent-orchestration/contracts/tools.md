# Tool Contracts: Agent 编排能力升级

**Feature**: 001-agent-orchestration

7 个新内置工具，全部实现 `internal/tools.Tool` 接口、经 `registerBuiltinTools`（`cmd_serve.go`）注册、受 `Registry.Filtered`/`allowed_tools` 过滤与 `permission.Manager.Check` 检查。`Description()` 必须为英文（`TestLocalToolDescriptionsAreEnglish` 门禁）。输出统一为 `tools.Result{Output, IsError}` 文本。

## 委派工具组（internal/tools/delegation）

### delegate

- **IsReadOnly**: false（产生持久副作用：委派记录）/ **CanRunInParallel**: true
- **不出现在委派子会话的 AllowedTools 中**（嵌套禁止第一道防线）

Input schema:

```json
{
  "type": "object",
  "properties": {
    "description": {
      "type": "string",
      "description": "Short label for the delegated task (shown in lists), e.g. 'Research auth flow'"
    },
    "prompt": {
      "type": "string",
      "description": "Full detailed instructions for the background read-only sub-agent"
    }
  },
  "required": ["description", "prompt"]
}
```

Output（成功，立即返回，不等待完成）:

```text
Delegation started: brisk-amber-fox
The sub-agent is read-only and runs in the background.
You will see a notification in this session when it completes.
Use delegation_read("brisk-amber-fox") to retrieve the full result later.
```

错误输出（IsError=true）：并发上限 `Too many running delegations (4). Wait for one to finish.`；嵌套 `Nested delegations are not allowed.`；参数校验失败按字段说明。

### delegation_read

- **IsReadOnly**: true / **CanRunInParallel**: true

```json
{
  "type": "object",
  "properties": {
    "id": { "type": "string", "description": "Delegation ID, e.g. 'brisk-amber-fox'" }
  },
  "required": ["id"]
}
```

Output：complete → 结果全文；running → `Delegation "<id>" is still running. Continue other work; you will be notified.`（非阻塞，IsError=false）；error → 失败详情；未知 ID → `Delegation "<id>" not found. Use delegation_list to see available delegations.`（IsError=true）。

### delegation_list

- **IsReadOnly**: true / **CanRunInParallel**: true
- Input: `{"type":"object","properties":{}}`（无参数）

Output（每委派一行 + 摘要行）:

```text
- brisk-amber-fox [complete] Research auth flow
  <summary ≤180 chars>
- calm-teal-owl [running] Map SSE protocol
  Running in the background.
```

空列表 → `No delegations found for this session.`

## 调度工具组（internal/tools/scheduletool）

### schedule_create

- **IsReadOnly**: false / **CanRunInParallel**: false

```json
{
  "type": "object",
  "properties": {
    "name": { "type": "string", "description": "Human-readable schedule name" },
    "instruction": { "type": "string", "description": "Full instruction the unattended session will execute on each trigger" },
    "cron": { "type": "string", "description": "5-field cron expression (min hour dom month dow), local timezone. Mutually exclusive with run_at" },
    "run_at": { "type": "string", "description": "RFC 3339 timestamp for a one-shot run. Mutually exclusive with cron" },
    "workdir": { "type": "string", "description": "Working directory for the scheduled session. Defaults to the current session workdir" }
  },
  "required": ["name", "instruction"]
}
```

Output：`Created schedule "dep-audit" (id: dep-audit). Next run: 2026-07-21 09:00 (local).`
校验错误（IsError=true）：`cron`/`run_at` 缺失或同时提供；cron 语法非法（指明字段）；workdir 不存在。

### schedule_list

- **IsReadOnly**: true / **CanRunInParallel**: true；Input 无参数

Output（每计划一行）: `- dep-audit [enabled] cron "0 9 * * 1-5" last run: 2026-07-18 09:00 — Run dependency security audit…`

### schedule_remove

- **IsReadOnly**: false / **CanRunInParallel**: false

```json
{
  "type": "object",
  "properties": { "id": { "type": "string" } },
  "required": ["id"]
}
```

Output：`Removed schedule "dep-audit".` / 未知 ID → IsError。删除立即生效（同 tick 内不再触发）。

### schedule_read_log

- **IsReadOnly**: true / **CanRunInParallel**: true

```json
{
  "type": "object",
  "properties": {
    "id": { "type": "string" },
    "limit": { "type": "integer", "description": "Max runs to return, default 5, cap 20" }
  },
  "required": ["id"]
}
```

Output：最近 N 次运行，每次含触发时间、状态、session_id（可回放）、output 尾部。无运行记录 → `No runs recorded for schedule "<id>" yet.`

## 权限语义

- 7 个工具均走 `Manager.Check`（tool=工具名，resource=主要参数），可被 preset 规则 allow/deny，`deny_permanent` 优先。
- 委派子会话内的每次工具调用照常走权限检查（只读集合通常被默认放行策略覆盖）。
- 无人值守调度会话内的权限提示无人应答 → 既有 `permission_request_timeout_seconds` 超时 → Deny（`source: timeout`），运行继续（该工具调用得到 denied 结果）。
