## MODIFIED Requirements

### Requirement: Server → Client 事件类型

服务 SHALL 以以下 **14 种核心事件**向客户端推送：

`assistant_text_delta`、`assistant_text_done`、`reasoning_start`、`reasoning_delta`、`reasoning_end`、`tool_call_start`、`tool_call_done`、`permission_request`、`subagent_event`、`compaction`、`provider_retry`、`error`、`interrupted`、`pong`。

此处仅列**核心会话事件**（在本次变更中由 11 增至 14，新增 3 个 reasoning 事件）。能力专属事件由各自能力的 spec 定义其 requirement，**不**在此列表重复：`frontend_tool_use` / `frontend_tools_published`（frontend-tools 能力）、`adapter_approval_request` / `adapter_approval_resolved` / `adapter_approval_expired`（adapter-generation 能力）。所有事件共享下述 envelope 与排序保证。

所有事件 SHALL 包含 `idx`（事件序号，**int64 单调递增**）、`session_id`，并在持久化模式下写入 `events` 表。

SSE 帧的 `id:` 字段值 SHALL 为 `idx` 的十进制字符串表示（如 `id: 42`）；客户端 `Last-Event-ID` SHALL 同样为 int64 字符串。

reasoning 事件 SHALL 携带 thinking 正文增量，供客户端实时展示（展示方式由客户端决定，默认折叠）；thinking 块的 `signature` 与 redacted 块的 `data` SHALL NOT 出现在任何 reasoning 事件中。事件形状：

- `reasoning_start { block_index, type }`：一个 thinking 块开始；`type` 为 `"thinking"` | `"redacted"`，使客户端可区分明文推理与 redacted 块（redacted 块不会有后续 `reasoning_delta`）。
- `reasoning_delta { block_index, delta }`：thinking 正文增量（仅 `type:"thinking"` 块产生）。
- `reasoning_end { block_index }`：该 thinking 块结束。

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
