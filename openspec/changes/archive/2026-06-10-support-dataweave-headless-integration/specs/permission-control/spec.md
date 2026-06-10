## MODIFIED Requirements

### Requirement: 权限规则数据结构与匹配语义

服务 SHALL 维护权限规则集合，每条规则含：

```go
type Permission struct {
    Tool     string  // 工具名 glob 模式："Bash" / "dataweave__query_*" / "*"（匹配任意工具名）
    Pattern  string  // 工具特定 glob 模式
    Decision string  // "allow" | "deny" | "ask"
    Scope    string  // "once" | "session" | "permanent"
}
```

`Tool` 字段 SHALL 用 **glob 匹配**（与 `Pattern` 同一套 `MatchGlob` 语法）。不含 glob 元字符（`*` `?` `[`）的值与字面比较等价，既有精确规则语义不变；空字符串与 `"*"` 均匹配任意工具名。MCP 工具按其注册名 `<server>__<tool>` 参与匹配，因此 `dataweave__query_*` 可一次覆盖该 server 下所有 `query_` 前缀工具。

`Pattern` 字段 SHALL 用 **glob 匹配**，语法基于 Go `path/filepath.Match` 扩展：

- `*` 匹配单段内任意字符（不跨 `/`、不跨 `:`）
- `**` 匹配任意多段（含 `/` 与 `:`）
- `?` 匹配单个字符
- 字面字符必须 exact match
- 大小写敏感

每个工具自定义 Pattern 的语义：

| 工具 | Pattern 含义 | 示例 |
|---|---|---|
| `Bash` | 命令字符串（trim 后），与 `Pattern` glob 匹配 | `"git status:*"` 匹配 `git status` / `git status -s`；`"npm *"` 匹配 `npm install` 等 |
| `Read` / `Write` / `Edit` / `Grep` | 主路径参数（如 `Read.path`），与 `Pattern` glob 匹配 | `"/proj/**"` 匹配 `/proj` 及任何子路径；`/proj/*.go` 匹配单层 .go 文件 |
| MCP 工具 / Dispatch / LoadSkill | 工具特定主参数；缺省时 Pattern `*` 匹配所有调用 | - |

`permanent` 作用域 SHALL 持久化到 SQLite `permissions` 表；`session`/`once` 仅在内存。

多条永久规则同时命中时既有优先级不变：`deny_permanent` SHALL 优先于任何 allow，与创建顺序无关——因此 `tool: "dataweave__*" allow_permanent` 与 `tool: "dataweave__node_exec" deny_permanent` 并存时，`node_exec` 被拒绝。

#### Scenario: permanent 规则跨会话生效

- **WHEN** 会话 A 中用户选 `allow_permanent` 同意 `Bash: "git status:*"`，会话 A 结束后新建会话 B
- **THEN** 会话 B 中 LLM 调用 `Bash { command: "git status" }`，不再触发权限询问

#### Scenario: tool glob 免打扰放行 MCP 只读工具

- **WHEN** 存在预设规则 `{ tool: "dataweave__query_*", decision: allow_permanent }`，一次会话内 LLM 连续调用 `dataweave__query_tasks`、`dataweave__query_instances` 等 20 个 `query_` 前缀 MCP 工具
- **THEN** 0 次权限询问、0 个 `permission_request` 事件，全部直接执行

#### Scenario: tool glob 下 deny 仍优先

- **WHEN** 同时存在 `{ tool: "dataweave__*", decision: allow_permanent }` 与 `{ tool: "dataweave__node_exec", decision: deny_permanent }`，LLM 调用 `dataweave__node_exec`
- **THEN** 命中 deny_permanent，返回 `tool_result { is_error: true }`，不执行

#### Scenario: 无元字符规则语义不变

- **WHEN** 既有规则 `{ tool: "Bash", pattern: "git *", decision: allow_permanent }`（tool 不含 glob 元字符）
- **THEN** 其匹配行为与 glob 化之前完全一致：仅匹配工具名恰为 `Bash` 的调用

## ADDED Requirements

### Requirement: 权限决议可观察性

权限检查的请求与决议 SHALL 以结构化事件出现在会话 SSE 流中，供外部审批系统与审计落库使用：

- `permission_request` SHALL **仅在**权限检查实际进入 prompt（无规则命中、无 default、或危险升级强制询问）时发射**一次**，payload 含 `request_id`（SHALL 等于触发该检查的 tool_use id）、`tool`、`resource`、`dangerous`、`reason`、`expires_at`（RFC3339，= 发射时刻 + `agent.permission_request_timeout_seconds`）。规则或 `default_permission` 自动决议的调用 SHALL NOT 产生 `permission_request`。
- 每次权限检查结束后服务 SHALL 发射 `permission_resolved`，payload 含 `request_id`（= tool_use id）、`tool`、`decision`（5 级决策值之一）、`source` ∈ `{ "rule", "default", "prompt", "timeout", "none" }`。`timeout` 表示 prompt 超时按 deny 处理；`none` 表示无 prompt 回调可用时的兜底 deny。
- 上述两事件 SHALL 与其他会话事件一样持久化进 `events` 表并参与 `Last-Event-ID` 续传。

#### Scenario: 外部审批闭环

- **WHEN** LLM 调用无任何规则覆盖的 `dataweave__publish_workflow`（tool_use id 为 `toolu_X`），外部系统在超时前 POST `permission_decision { request_id: "toolu_X", decision: "allow_once" }`
- **THEN** SSE 流依次出现 `permission_request { request_id: "toolu_X", expires_at: ... }`、`permission_resolved { request_id: "toolu_X", decision: "allow_once", source: "prompt" }`、`tool_call_start { id: "toolu_X" }`

#### Scenario: 超时决议可观察

- **WHEN** `permission_request` 发射后超过 `agent.permission_request_timeout_seconds` 无应答
- **THEN** SSE 流出现 `permission_resolved { decision: "deny", source: "timeout" }`，随后的 `tool_call` 结果为 `is_error: true`

#### Scenario: 规则放行不产生审批请求

- **WHEN** 预设规则命中使调用自动放行
- **THEN** 无 `permission_request` 事件；SSE 流出现 `permission_resolved { decision: "allow_permanent", source: "rule" }` 后直接 `tool_call_start`
