## MODIFIED Requirements

### Requirement: Server → Client 事件类型

服务 SHALL 以以下 **15 种核心事件**向客户端推送：

`assistant_text_delta`、`assistant_text_done`、`reasoning_start`、`reasoning_delta`、`reasoning_end`、`tool_call_start`、`tool_call_done`、`permission_request`、`permission_resolved`、`subagent_event`、`compaction`、`provider_retry`、`error`、`interrupted`、`pong`。

此处仅列**核心会话事件**（在本次变更中由 14 增至 15，新增 `permission_resolved`）。能力专属事件由各自能力的 spec 定义其 requirement，**不**在此列表重复：`frontend_tool_use` / `frontend_tools_published`（frontend-tools 能力）、`adapter_approval_request` / `adapter_approval_resolved` / `adapter_approval_expired`（adapter-generation 能力）。所有事件共享下述 envelope 与排序保证。

所有事件 SHALL 包含 `idx`（事件序号，**int64 单调递增**）、`session_id`，并在持久化模式下写入 `events` 表。

SSE 帧的 `id:` 字段值 SHALL 为 `idx` 的十进制字符串表示（如 `id: 42`）；客户端 `Last-Event-ID` SHALL 同样为 int64 字符串。

reasoning 事件 SHALL 携带 thinking 正文增量，供客户端实时展示（展示方式由客户端决定，默认折叠）；thinking 块的 `signature` 与 redacted 块的 `data` SHALL NOT 出现在任何 reasoning 事件中。事件形状：

- `reasoning_start { block_index, type }`：一个 thinking 块开始；`type` 为 `"thinking"` | `"redacted"`，使客户端可区分明文推理与 redacted 块（redacted 块不会有后续 `reasoning_delta`）。
- `reasoning_delta { block_index, delta }`：thinking 正文增量（仅 `type:"thinking"` 块产生）。
- `reasoning_end { block_index }`：该 thinking 块结束。

权限事件形状（语义详见 permission-control 能力的"权限决议可观察性"要求）：

- `permission_request { request_id, tool, resource, dangerous, reason, expires_at }`：仅当权限检查实际进入 prompt 时发射一次；`request_id` SHALL 等于触发检查的 tool_use id；`expires_at` 为 RFC3339 超时时刻。规则/default 自动决议的调用 SHALL NOT 产生此事件。
- `permission_resolved { request_id, tool, decision, source }`：每次权限检查结束发射；`source` ∈ `{ "rule", "default", "prompt", "timeout", "none" }`。

#### Scenario: 文本流式推送

- **WHEN** LLM 输出 "hello world" 的 token 流
- **THEN** 服务依次 emit 多个 `assistant_text_delta`，最后 emit 一个 `assistant_text_done` 含 `message_id`

#### Scenario: 工具调用事件配对

- **WHEN** Agent 调用 `Bash` 工具执行 `"ls"`
- **THEN** 服务先 emit `tool_call_start { id, tool:"Bash", input }`，工具完成后 emit `tool_call_done { id, output, ok: true, took_ms }`

#### Scenario: provider_retry 事件

- **WHEN** provider 返回 429，Agent 触发指数退避
- **THEN** 服务 emit `provider_retry { attempt: 1, after_ms: 500 }`；若仍失败再 emit `provider_retry { attempt: 2, after_ms: 2000 }`

#### Scenario: thinking 流式推送

- **WHEN** LLM 产出一个 thinking 块（含 signature）后再产出文本
- **THEN** 服务依次 emit `reasoning_start { type:"thinking" }`、若干 `reasoning_delta`（仅正文，无 signature）、`reasoning_end`，随后才是 `assistant_text_delta` / `assistant_text_done`

#### Scenario: redacted_thinking 推送可区分

- **WHEN** LLM 产出一个 redacted_thinking 块
- **THEN** 服务 emit `reasoning_start { type:"redacted" }` 后直接 emit `reasoning_end`，中间无 `reasoning_delta`；客户端据 `type` 区分 redacted 块与空 thinking

#### Scenario: 自动放行调用无审批请求事件

- **WHEN** 一次工具调用被预设规则免打扰放行
- **THEN** SSE 流中无该调用的 `permission_request` 事件，仅有 `permission_resolved { source: "rule" }` 与 `tool_call_start`/`tool_call_done`

## ADDED Requirements

### Requirement: 会话创建定制字段（instructions 与 metadata）

`POST /v1/sessions` 请求体 SHALL 额外接受以下可选字段：

- `instructions`（string，≤16384 字节，超限返回 400）：会话级附加指令。SHALL 注入 system prompt 的 Instructions 动态段（位于 AGENTS.md 内容之后），SHALL NOT 进入静态缓存前缀。
- `metadata`（object，string→string，≤32 键、单键/单值各 ≤1024 字节，超限返回 400）：调用方自定义元数据。服务 SHALL 原样持久化、不解释其内容。

`SessionMeta`（create / get / list 端点返回）SHALL 在 `metadata` 非空时包含 camelCase 字段 `metadata`，内容与创建时一致。两字段 SHALL 随会话持久化，水合重开后继续生效。

#### Scenario: 创建带定制字段的会话

- **WHEN** 客户端 POST `/v1/sessions` 携带 `{ "workdir": "/srv/dw", "provider": "anthropic", "model": "claude-sonnet-4-6", "instructions": "当前页面 taskId=T-1024", "metadata": { "dataweave_conversation_id": "conv-7f3a" } }`
- **THEN** 返回 `201` 且 `SessionMeta.metadata.dataweave_conversation_id == "conv-7f3a"`；该会话首次推理的 system prompt Instructions 段包含 `当前页面 taskId=T-1024`

#### Scenario: metadata 跨水合保留

- **WHEN** 携带 metadata 的会话被驱逐后通过 `GET /v1/sessions/{id}` 再次读取
- **THEN** 响应仍含创建时的完整 `metadata`

#### Scenario: 超限拒绝

- **WHEN** `instructions` 超过 16384 字节
- **THEN** 返回 `400`，错误信息指明字段与上限
