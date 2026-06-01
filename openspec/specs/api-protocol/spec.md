# api-protocol Specification

## Purpose
TBD - created by archiving change init-workhorse-agent-mvp. Update Purpose after archive.
## Requirements
### Requirement: 与 MCP 2025-11-25 Streamable HTTP 的关系

服务 SHALL 在 `docs/protocol.md` 与本 spec 顶部明确声明：

- 本服务**借鉴** MCP 2025-11-25 Streamable HTTP 的**传输层模型**（单端点 POST + GET、SSE `id:` 字段 + `Last-Event-ID` 重连、`Origin` 校验、绑定 localhost）
- 本服务**不是** MCP server，**不**使用 JSON-RPC 2.0 信封；消息体是应用层 ClientEvent / ServerEvent JSON 对象
- 通用 MCP 客户端误向本服务发起 `initialize` 等 JSON-RPC 调用时 SHALL 收到 `400 Bad Request` 含 `{ "code": "unknown_message_type" }`，不会触发不可控行为

#### Scenario: MCP 客户端误调用得到清晰错误

- **WHEN** 通用 MCP 客户端向 `POST /v1/sessions/<id>/stream` 提交 JSON-RPC `{ "jsonrpc": "2.0", "method": "initialize", ... }`
- **THEN** 服务返回 `400 Bad Request` 含 `{ "code": "unknown_message_type", "message": "this server uses application-level ClientEvent, not JSON-RPC; see docs/protocol.md" }`

### Requirement: HTTP REST 端点集合

服务 SHALL 在 `127.0.0.1:7821`（默认，可配）暴露以下 HTTP 端点：

- `POST /v1/sessions` — 创建会话，请求体含 `workdir`、`env`、`provider`、`model`、`ephemeral?`，返回 `SessionMeta`(camelCase)
- `GET /v1/sessions` — 列出会话;携带 `?workdir=<path>` 时按项目分桶过滤
- `GET /v1/sessions/{id}` — 会话详情(`SessionMeta`)
- `GET /v1/sessions/{id}/history` — 拉取 transcript(`{ "messages": [HistoryMessage] }`)
- `PATCH /v1/sessions/{id}` — 重命名(`{ "title" }`)
- `DELETE /v1/sessions/{id}` — 删除会话及其 transcript(硬删 + 级联)
- `POST /v1/sessions/{id}/cancel` — 中断当前推理
- `POST /v1/sessions/{id}/compact` — 手动触发上下文压缩
- `POST /v1/sessions/{id}/stream` — Streamable HTTP 客户端消息提交端点
- `GET /v1/sessions/{id}/stream` — Streamable HTTP 服务端事件流（SSE）;SHALL 能为
  已存在、可能 idle(非刚创建)的会话工作
- `GET /v1/projects` — 列出已知项目(`{ "projects": [ProjectMeta] }`)
- `GET /debug/sessions/{id}/events?since=N` — 事件回放（DEBUG 模式）
- `GET /health` — 健康检查
- `GET /ui` — 内嵌参考 Web UI

`GET /health` 响应除既有的 `ok`、`version`、`uptime_sec`、`sessions_active`
字段外，SHALL 额外包含 `protocol_version` 与 `capabilities`。这两个字段向后兼容。

#### Scenario: 创建会话

- **WHEN** 客户端 POST `/v1/sessions` 携带 `{ "workdir": "/tmp/proj", "provider": "anthropic", "model": "claude-sonnet-4-6" }`
- **THEN** 服务返回 `201 Created` 和 camelCase `SessionMeta`，含 `id`、`status: "idle"`、`workdir`、`createdAt`

#### Scenario: 按项目列出会话

- **WHEN** 客户端 GET `/v1/sessions?workdir=/tmp/proj`
- **THEN** 服务返回 `{ "sessions": [SessionMeta] }`,仅含该 `workdir` 的未删除会话

#### Scenario: 销毁不存在的会话

- **WHEN** 客户端 DELETE `/v1/sessions/<不存在的 id>`
- **THEN** 服务返回 `404 Not Found`

#### Scenario: 健康检查

- **WHEN** 客户端 GET `/health`
- **THEN** 服务返回 `200 OK` 和 `{ "ok": true, "version": "<semver>", "uptime_sec": <int>, "sessions_active": <int>, "protocol_version": "<string>", "capabilities": [<string>, ...] }`

### Requirement: HTTP 方法与 Content Negotiation

服务 SHALL 对 `/v1/sessions/{id}/stream` 端点强制以下 HTTP 协商规则（对齐 MCP 2025-11-25）：

- 仅接受 `GET` 与 `POST`；其他方法（PUT/DELETE/PATCH/HEAD/OPTIONS 等）SHALL 返回 `405 Method Not Allowed` 含 `Allow: GET, POST` header
- `POST` 的 `Content-Type` 必须为 `application/json`；否则 SHALL 返回 `415 Unsupported Media Type`
- `GET` 的 `Accept` header 必须包含 `text/event-stream`（或 `*/*`、缺失视为允许）；否则 SHALL 返回 `406 Not Acceptable`

#### Scenario: 拒绝 PUT 方法

- **WHEN** 客户端 PUT `/v1/sessions/<id>/stream`
- **THEN** 服务返回 `405 Method Not Allowed`，响应 header 含 `Allow: GET, POST`

#### Scenario: 拒绝非 JSON POST

- **WHEN** 客户端 POST `/v1/sessions/<id>/stream` 带 `Content-Type: text/plain`
- **THEN** 服务返回 `415 Unsupported Media Type`

#### Scenario: 拒绝不接受 SSE 的 GET

- **WHEN** 客户端 GET `/v1/sessions/<id>/stream` 带 `Accept: application/json`
- **THEN** 服务返回 `406 Not Acceptable`

### Requirement: Streamable HTTP 传输

服务 SHALL 在 `/v1/sessions/{id}/stream` 实现 MCP 2025-11-25 Streamable HTTP 传输模型：

- **POST** SHALL 接受 `Content-Type: application/json` 的请求体（单个 ClientEvent JSON 对象）；正常受理时 SHALL 返回 `202 Accepted` 无 body；session 状态拒绝时按"POST 与会话状态冲突"requirement 处理
- **GET** SHALL 返回 `Content-Type: text/event-stream`，开启长连接 SSE 流；响应 header 还 SHALL 包含 `Cache-Control: no-cache`、`Connection: keep-alive`、`X-Accel-Buffering: no`（对 nginx 透明）
- SSE 帧格式 SHALL 为 `id: <idx>\nevent: <type>\ndata: <json>\n\n`；JSON `data` SHALL 序列化为**紧凑单行**（无嵌入换行符），若值字段含 `\n`，序列化器 SHALL 使用 JSON `\n` 转义而非真换行
- GET SSE 流 SHALL 每 25 秒（可配 `sse_keepalive_seconds`）发送 `: keep-alive\n\n` SSE 注释作为心跳
- 同一 session 同一时刻 SHALL 最多保持一条活跃 GET SSE 流；新 GET 到来时服务器 SHALL 在 session 级写锁保护下：(1) 向旧流发 SSE 注释 `: superseded`、(2) 关闭旧响应 writer、(3) 把控制权交给新 GET handler，整个切换期间 SHALL 不写任何业务事件到 outbox（事件继续入 events 表，新流通过 Last-Event-ID 回放）

#### Scenario: POST 客户端消息默认返回 202

- **WHEN** 客户端 POST `/v1/sessions/<existing-id>/stream` 携带 `{"type":"user_message","content":"hi"}`
- **THEN** 服务返回 `202 Accepted` 无 body；session 状态从 Idle 进入 Thinking；该次推理产生的事件随后在 GET SSE 流上推送

#### Scenario: GET SSE 流推送事件

- **WHEN** 客户端已 GET `/v1/sessions/<id>/stream` 开启 SSE，随后 LLM 产生一个 text delta（含换行符的文本，如 `"line1\nline2"`）
- **THEN** 服务在 SSE 流上写出 `id: <idx>\nevent: assistant_text_delta\ndata: {"delta":"line1\\nline2","session_id":"..."}\n\n`（`\n` 在 JSON 中被转义为字面两字符 `\` + `n`，整个 data 仍是单行）

#### Scenario: SSE 响应头完整

- **WHEN** 客户端 GET `/v1/sessions/<id>/stream` 成功开启 SSE
- **THEN** 响应 header 含 `Content-Type: text/event-stream`、`Cache-Control: no-cache`、`Connection: keep-alive`、`X-Accel-Buffering: no`

#### Scenario: POST 到不存在会话

- **WHEN** 客户端 POST `/v1/sessions/<不存在的 id>/stream`
- **THEN** 服务返回 `404 Not Found`

#### Scenario: 并发 GET 关闭旧流且无事件丢失

- **WHEN** 客户端 A 已对 session X 开启 GET SSE，工具正在产生事件流；客户端 B 对同一 session X 发起新 GET，期间又有 3 个事件被生成
- **THEN** 服务在 session 级写锁内向 A 写 `: superseded` 并关闭；新 GET 进入后通过 `Last-Event-ID`（B 提供）回放包括切换期间的 3 个事件，B 不漏事件

### Requirement: POST 与会话状态冲突

服务 SHALL 在 session 处于以下状态时拒绝 `user_message` / `permission_decision` / `context_update` POST：

| Session 状态 | 接受的 POST type | 拒绝的 POST type | 拒绝响应 |
|---|---|---|---|
| `Idle` | 全部 | 无 | - |
| `Thinking` | `interrupt`、`ping` | `user_message`、`permission_decision`（无待决）、`context_update` | `409 Conflict` |
| `AwaitPerm` | `permission_decision`（匹配 request_id）、`interrupt`、`ping` | 其他 | `409 Conflict` |
| `Executing` | `interrupt`、`ping` | 其他 | `409 Conflict` |
| `Compacting` | `interrupt`、`ping` | 其他 | `409 Conflict` |
| `Cancelled` | `interrupt`（幂等，返 `202 Accepted` 无副作用）、`ping` | 其他 | `409 Conflict` |

注：`Cancelled` 是收尾中状态（见 session-management "取消收尾超时"），通常在 `cancel_drain_timeout_seconds` 内转为 `Idle`；期间重复 interrupt 按幂等返 `202`，其他类型按 `session_busy` 拒绝。

拒绝时 SHALL 返回 `409 Conflict` 含 body `{ "code": "session_busy", "message": "...", "state": "<current>" }`；同时服务 SHALL 在 GET SSE 流上 emit 一条 `error` 事件 `{ "code": "session_busy", "state": "<current>" }`（双通道一致），确保不看 POST 响应的 SSE-only 客户端也能感知。

#### Scenario: Compacting 期间拒绝 user_message

- **WHEN** session 处于 `Compacting`，客户端 POST `{"type":"user_message","content":"foo"}`
- **THEN** 服务返回 `409 Conflict` 含 `{"code":"session_busy","state":"Compacting"}`；同时在 GET SSE 流上 emit 一条 `error` 事件 `{"code":"session_busy","state":"Compacting"}`

#### Scenario: Thinking 期间允许 interrupt

- **WHEN** session 处于 `Thinking`，客户端 POST `{"type":"interrupt"}`
- **THEN** 服务返回 `202 Accepted`，开始取消流程

### Requirement: 中断到达时清空 SSE 积压

服务 SHALL 在收到 `interrupt` POST 后：

1. 立即触发 session ctx 取消
2. 在写入合成 cancelled tool_result 与 `interrupted` 事件之前，**清空 session outbox channel**（任何尚未推送到 SSE 流但属于"被中断那一轮"的事件 SHALL 不再推送）
3. 已写入 `events` 表的事件 SHALL **保留**（客户端通过 `Last-Event-ID` 重连可拉回，确保 events 表是完整审计日志）
4. SSE 流上立即 emit `interrupted` 事件作为该轮终点

#### Scenario: 中断后 SSE 不再推积压

- **WHEN** LLM 正在快速 stream 大量 text_delta，客户端 POST interrupt 时 outbox 中还积压了 15 条 text_delta 未推
- **THEN** 服务停止推送这 15 条 text_delta；SSE 流直接 emit `interrupted` 事件；events 表中保留全部 15 条 text_delta + 1 条 interrupted

#### Scenario: 中断后 Last-Event-ID 重连能拉回被丢的事件

- **WHEN** 上述中断后客户端用 `Last-Event-ID: <被丢前最后 idx>` 重新 GET
- **THEN** 服务按 idx 顺序回放包括"实时被丢"的 15 条 text_delta 与 interrupted 事件

### Requirement: Origin 校验

服务 SHALL 在所有 `/v1/sessions/{id}/stream` 的 POST 与 GET 请求上校验 `Origin` header：

- 校验算法 SHALL 用标准 URL parser 解析 Origin，取 `scheme` + `hostname` + `port` 三元组做 **exact match**，禁止前缀/子串/正则匹配（防 `http://127.0.0.1.evil.com` 同形异义攻击）
- 默认白名单（精确 host）：
  - 缺失 `Origin`（curl、Node fetch、同源 file://）—— 仅在服务绑定 `127.0.0.1` 时允许（公网绑定时 SHALL 拒绝）
  - `http://127.0.0.1:<任意端口>`、`http://localhost:<任意端口>`、`https://` 同上
  - `null`（sandboxed iframe / file:// 触发）—— 仅在配置 `allow_null_origin: true` 时允许（默认 false）
- 白名单 SHALL 可通过 `configuration` capability 中的 `allowed_origins` 字段精确扩展（列表中每项均为完整 origin 字符串如 `http://localhost:5173`）

校验目的是防 DNS rebinding 攻击（MCP 2025-11-25 spec MUST 要求）。

#### Scenario: 拒绝跨站 Origin

- **WHEN** 浏览器从 `https://evil.com` 通过 XHR 调 `POST http://127.0.0.1:7821/v1/sessions/<id>/stream`
- **THEN** 服务返回 `403 Forbidden`，不处理请求

#### Scenario: 拒绝同形异义 Origin

- **WHEN** 请求 Origin 为 `http://127.0.0.1.evil.com`
- **THEN** 服务用 URL parser 解析，hostname 是 `127.0.0.1.evil.com`，不命中白名单的精确 `127.0.0.1`，返回 `403 Forbidden`

#### Scenario: 同源与本地允许

- **WHEN** 请求 Origin 为 `http://localhost:5173`（用户自定义 UI 开发服）
- **THEN** 服务正常处理请求

#### Scenario: 缺失 Origin 在 localhost 绑定下允许

- **WHEN** 服务绑定 `127.0.0.1:7821`，客户端 `curl` 不发 `Origin` header
- **THEN** 服务允许请求

#### Scenario: 缺失 Origin 在公网绑定下拒绝

- **WHEN** 服务被配置绑定 `0.0.0.0:7821`（非默认），客户端 `curl` 不发 `Origin`
- **THEN** 服务返回 `403 Forbidden`

### Requirement: Client → Server 消息类型

服务 SHALL 接受以下 5 种 Client → Server 消息：

| `type` | 字段 | 语义 |
|---|---|---|
| `user_message` | `content: string`, `attachments?: []` | 新一轮用户消息 |
| `permission_decision` | `request_id`, `decision: "allow_once"\|"allow_session"\|"allow_permanent"\|"deny"\|"deny_permanent"` | 响应权限询问 |
| `interrupt` | （无） | 中断当前推理 |
| `ping` | （无） | 心跳 |
| `context_update` | `workdir?`, `files?` | 元数据更新 |

未知 `type` SHALL 被 POST 端点直接拒绝并返回 `400 Bad Request`，body 含 `{"code":"unknown_message_type","message":"..."}`；不影响该 session 的 GET SSE 流。

#### Scenario: 未知消息类型被拒绝

- **WHEN** 客户端 POST 携带 `{"type":"frobnicate"}`
- **THEN** 服务返回 `400 Bad Request` 含 `{"code":"unknown_message_type","message":"..."}`；该 session 已开启的 GET SSE 流不受影响

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

### Requirement: error 事件 JSON schema

`error` 事件 SHALL 遵循以下完整 schema：

```json
{
  "type": "error",
  "idx": <int64>,
  "session_id": "<ULID>",
  "code": "<machine-readable code from enum below>",
  "message": "<human-readable message, safe for display>",
  "details": { /* optional, code-specific structured fields */ },
  "recoverable": <bool>
}
```

`code` 字段 SHALL 是以下枚举之一（实现 SHALL 持续维护此清单；新增 code 必须先入 spec）：

| code | 触发场景 | recoverable | details 字段 |
|---|---|---|---|
| `session_busy` | session 在 Compacting/Executing/AwaitPerm 等状态拒收 POST | true | `{ "state": "<current session state>" }` |
| `unknown_message_type` | POST body 含未知 ClientEvent type | false | `{ "received_type": "<what client sent>" }` |
| `history_token_limit` | history 超过 `agent.max_history_tokens` 硬上限 | false | `{ "limit": <int>, "current": <int> }` |
| `tool_not_allowed` | LLM 试图调用 AllowedTools 之外的工具 | true | `{ "tool": "<name>" }` |
| `permission_denied` | 权限规则 deny 或用户拒绝 | true | `{ "tool": "<name>", "pattern": "<rule pattern>" }` |
| `provider_auth_failed` | provider 返 401 | false | `{ "provider": "anthropic" }` |
| `provider_invalid_request` | provider 返 400 | false | `{ "provider": "...", "upstream_message": "..." }` |
| `provider_context_length_exceeded` | LLM 拒绝过长请求 | false | `{ "provider": "...", "tokens": <int> }` |
| `provider_insufficient_quota` | OpenAI 配额耗尽 | false | `{ "provider": "openai" }` |
| `provider_unrecoverable` | 其他不可重试 provider 错 | false | `{ "provider": "...", "upstream_code": "..." }` |
| `cancel_timeout` | 取消收尾超时（见 session-management） | true | `{ "phase": "...", "elapsed_ms": <int> }` |
| `internal_panic` | session goroutine 顶层 recover | true | `{}` (stack trace 仅入日志，不暴露给客户端) |
| `server_shutdown` | graceful shutdown 触发 | false | `{}` |
| `request_too_large` | POST body 超 `max_request_body_bytes` | true | `{ "limit": <int> }` |

`recoverable: true` 表示客户端可以继续在该 session 上发新 user_message；`false` 表示 session 已不可继续使用（多数情况下 session 已转 Idle 但实质功能受阻，客户端通常应 DELETE 该 session 或修复底层问题——如换 provider key——后新建 session）。

#### Scenario: session_busy 含完整 details

- **WHEN** Compacting 状态时 POST user_message
- **THEN** SSE 流 emit `{"type":"error","idx":42,"session_id":"...","code":"session_busy","message":"session is currently compacting","details":{"state":"Compacting"},"recoverable":true}`

#### Scenario: provider_auth_failed 标 unrecoverable

- **WHEN** Anthropic 返 401
- **THEN** SSE emit `{"type":"error","code":"provider_auth_failed","details":{"provider":"anthropic"},"recoverable":false}`；客户端 UI 应提示"修复 API key 后新建 session"

### Requirement: 事件排序保证

同一 session 的所有 Server→Client 事件 SHALL 按 `idx` 全局单调递增顺序写入 SSE 流，**绝不并发交错**。实现层面 SHALL 用 session 级 mutex / 单消费者 channel 保护事件发布，避免多 goroutine 同时写同一 `http.ResponseWriter` 触发 panic 或乱序。

`idx` SHALL 在事件**写入 events 表的同一事务**中分配（用 SQLite `INTEGER PRIMARY KEY AUTOINCREMENT`），保证 events 表与 SSE 流上看到的 `idx` 完全一致。

#### Scenario: 多 goroutine 并发产生事件

- **WHEN** Agent 主 goroutine 在 stream text_delta，同时一个工具 goroutine 完成发 tool_call_done
- **THEN** 两个事件按发起顺序串行进入 outbox；SSE 流上看到的 idx 严格递增；事件不交错

### Requirement: 断线重连按 Last-Event-ID 增量同步

服务 SHALL 在 GET `/v1/sessions/{id}/stream` 请求 header 中接受标准 SSE `Last-Event-ID: <idx>`；若给定，服务 SHALL 按以下原子流程处理：

1. 在 session 级写锁内打开新 SSE 流
2. 记录当前 events 表的 `max_idx_snapshot = MAX(idx)`
3. 按 idx 升序查询 `idx > Last-Event-ID AND idx <= max_idx_snapshot` 的所有事件，依次写到 SSE 流
4. **回放期间**，session 实时产生的新事件正常写入 events 表，但**不**写到该 SSE 流（仍受 session 写锁保护）
5. 回放完成后释放写锁，切换到实时模式：从 outbox channel 消费新事件写到 SSE 流（包括回放期间产生的 idx > max_idx_snapshot 的事件）

为兼容部分客户端（如 curl 无法设 header），服务 SHALL 同时接受查询参数 `?last_event_id=N` 作为 header 缺失时的备选；header 与 query 同时给定时以 header 为准。

LLM 推理 SHALL 在客户端断开期间继续进行，不被打断。

#### Scenario: 浏览器 EventSource 自动重连

- **WHEN** 浏览器用 `new EventSource('/v1/sessions/<id>/stream')` 接收事件，期间网络抖动断开 3 秒；EventSource 自动重连并在 header 中带 `Last-Event-ID: 42`
- **THEN** 服务先按序推 `idx > 42` 的所有事件，再继续实时事件；浏览器 onmessage 不漏一条事件

#### Scenario: curl 客户端通过 query 重连

- **WHEN** curl 客户端无法设 `Last-Event-ID` header，改用 `curl -N "http://127.0.0.1:7821/v1/sessions/<id>/stream?last_event_id=42"`
- **THEN** 服务行为同上，先回放后接续

#### Scenario: 客户端断线期间推理继续

- **WHEN** 客户端触发 `user_message` 后立即关闭 GET SSE 流，5 秒后再次 GET 并带 `Last-Event-ID`
- **THEN** 在客户端断开的 5 秒内，session 继续 LLM 推理与工具执行；事件正常写入 `events` 表；重连后客户端按 `idx` 顺序拿到完整事件流

#### Scenario: 回放与实时事件无重复无遗漏

- **WHEN** 客户端重连带 `Last-Event-ID: 42`，max_idx 当前为 50；回放 idx=43..50 期间 session 又产生 idx=51,52
- **THEN** 客户端先收到 43..50，回放完成后立即收到 51,52；不丢、不重

### Requirement: ID 格式

服务 SHALL 区分以下两类 ID：

- **session/message/request/agent ID** SHALL 为 ULID（128-bit，时间有序，Crockford Base32 编码 26 字符）
- **事件 idx** SHALL 为 int64 单调递增整数（SQLite `INTEGER PRIMARY KEY AUTOINCREMENT`）

SSE 流上 `id:` 字段携带的是事件 `idx`（十进制字符串），不是 ULID。

#### Scenario: 创建的 session ID 是 ULID

- **WHEN** POST `/v1/sessions` 创建一个会话
- **THEN** 响应的 `id` 字段匹配正则 `^[0-9A-HJKMNP-TV-Z]{26}$`

#### Scenario: SSE id 是整数

- **WHEN** GET SSE 流推送任意事件
- **THEN** SSE 帧 `id:` 字段值匹配 `^[1-9][0-9]*$`（int64 十进制）

### Requirement: 客户端断开检测

服务 SHALL 通过 `r.Context().Done()` 或 `http.ResponseController.SetWriteDeadline` 检测 GET SSE 客户端断开：

- 断开后 SHALL 停止向该响应写事件
- session goroutine SHALL **继续**运行（推理继续，事件继续入 events 表）
- 服务 SHALL 释放对应的 session-stream 写锁，允许后续 GET 重连

#### Scenario: 客户端关闭浏览器但推理继续

- **WHEN** 客户端关闭浏览器（HTTP 连接断开），此时 LLM 推理仍在进行
- **THEN** 服务在 1 秒内检测到 `r.Context().Done()`，停止 SSE 写；session goroutine 继续；新事件正常入 events 表

<!-- 来源：AI #2 复审 (2026-05-24) M-3：POST 请求体大小限制缺失，恶意/异常客户端可发超大 JSON 导致 OOM。 -->

### Requirement: 请求体大小限制

所有 POST 端点 SHALL 在读取 body 前强制大小上限 `server.max_request_body_bytes`（默认 1 MiB = 1048576）：

- 用 `http.MaxBytesReader` 包裹 `r.Body`，读取超限时立刻返回 `413 Payload Too Large`
- 响应 body 含 `{ "code": "request_too_large", "limit": <N>, "message": "..." }`
- 大附件场景（如未来 attachments 字段）SHALL 经独立的 chunked upload 端点（V2），不在 `/v1/sessions/{id}/stream` POST 上塞大 body

#### Scenario: 拒绝超大 POST

- **WHEN** 客户端 POST `/v1/sessions/<id>/stream` body 为 5 MiB JSON
- **THEN** 服务读到 1 MiB 上限即停，返回 `413 Payload Too Large` 含 `{"code":"request_too_large","limit":1048576}`

#### Scenario: 默认上限可配

- **WHEN** 配置 `server.max_request_body_bytes: 524288`（512 KiB），客户端 POST 1 MiB body
- **THEN** 服务返回 `413` 在 512 KiB 处

### Requirement: HTTP server 超时配置

服务 HTTP server SHALL 配置以下超时：

- `ReadHeaderTimeout`：10 秒（防 slowloris）
- `ReadTimeout`：60 秒（POST body 必须 60 秒内完整收完）
- `WriteTimeout`：**0**（SSE 长连接禁用写超时；非 SSE 端点由 handler 自行管理）
- `IdleTimeout`：120 秒
- `MaxHeaderBytes`：1 MiB

非 SSE 的 REST 端点 SHALL 在 handler 内用 `http.ResponseController.SetWriteDeadline` 设置 30 秒写超时。

#### Scenario: SSE 长连接不被超时砍

- **WHEN** GET SSE 连接保持 10 分钟无任何事件（仅 keep-alive 注释）
- **THEN** 连接不被服务端超时关闭；客户端持续收到 25 秒一次的 `: keep-alive`

<!-- 来源：AI #1 复审 (2026-05-24)：原步骤 2 先关 SSE 导致步骤 3 产生的 cancelled tool_result 与 interrupted 事件无法送达；ephemeral session 事件永久丢失。改为先取消产生事件、再关 SSE。 -->

### Requirement: Graceful Shutdown

服务收到 `SIGTERM` 或 `SIGINT` 时 SHALL 按以下严格顺序执行（不可重排，关键是先产生 cancelled 事件、再关 SSE 流）：

1. 立即停止接受新的 HTTP 连接（`http.Server.Shutdown` 但保留已建立的 SSE 流）
2. 对所有 Thinking/Executing/AwaitPerm/Compacting 状态的 session 触发取消（与用户 interrupt 同流程，含合成 cancelled tool_result 与 emit `interrupted` 事件），按"取消收尾超时"requirement 限时收尾
3. 等待 cancelled tool_result 与 `interrupted` 事件全部写入 events 表与 outbox channel（持久化 session 的事件至此已不可丢失，ephemeral session 通过仍开着的 SSE 流推送到客户端）
4. 对所有活跃 GET SSE 流，emit 一条 `error { code: "server_shutdown", recoverable: false }` 事件并 flush，然后关闭响应 writer
5. 等待所有 session goroutine 退出（步骤 2-4 总时长不超过 `server.graceful_shutdown_timeout_seconds` 秒，默认 30；超时强制终止）
6. 关闭 SQLite 连接、MCP host 子进程
7. 进程退出码 0；超时强制退出码 1

#### Scenario: SIGTERM 优雅退出含取消事件可见

- **WHEN** 进程收到 SIGTERM，当前有 3 个 Thinking session（其中 1 个 ephemeral，2 个持久化）、1 个 Idle session，客户端均在 GET SSE
- **THEN** 3 个 Thinking session 先收到取消、合成 cancelled tool_result 入 history、emit `interrupted`；客户端通过仍开着的 SSE 流收到这些事件；最后 SSE 流再 emit `error { code: "server_shutdown" }` 并关闭；进程在 30 秒内退出码 0

#### Scenario: Ephemeral session 取消事件不丢

- **WHEN** SIGTERM 触发时存在 1 个 ephemeral Thinking session，客户端正在监听 SSE
- **THEN** 步骤 2-3 产生的 cancelled tool_result 与 `interrupted` 事件在步骤 4 关 SSE 前推送给客户端；客户端 UI 能看到"被中断"而非突然连接断开

### Requirement: Bearer Token 鉴权（可选）

服务 SHALL 支持可选的 Bearer Token 鉴权（开关与 token 值见 `configuration` capability）：

- 鉴权启用时 SHALL 对所有 `/v1/*` 端点（含 REST 与 Streamable HTTP 的 POST/GET）验证 `Authorization: Bearer <token>` header
- `/health` SHALL **不**鉴权（监控用）
- `/ui` SHALL **不**鉴权（静态资源；UI 内部调 API 仍需 token，通过 query 或自定义机制传递）
- `/debug/*` SHALL 鉴权
- 缺失或错误 token SHALL 返回 `401 Unauthorized` 含 body `{ "code": "auth_required" }` 或 `{ "code": "invalid_token" }`
- Token 比较 SHALL 使用 constant-time 比较（`crypto/subtle.ConstantTimeCompare`）防 timing attack
- Token 值 SHALL **不**写入任何日志（即使 DEBUG 级别）

#### Scenario: 启用鉴权后无 token POST 被拒

- **WHEN** 配置启用 bearer auth，客户端 POST `/v1/sessions` 不带 `Authorization` header
- **THEN** 服务返回 `401 Unauthorized` 含 `{"code":"auth_required"}`

#### Scenario: 错误 token

- **WHEN** 客户端 GET `/v1/sessions/<id>/stream` 带 `Authorization: Bearer wrong-token`
- **THEN** 服务返回 `401 Unauthorized` 含 `{"code":"invalid_token"}`

#### Scenario: health 不鉴权

- **WHEN** 启用鉴权后，客户端 GET `/health` 不带 token
- **THEN** 服务返回 `200 OK`

### Requirement: Frontend-tool client messages

`POST /v1/sessions/{id}/stream` SHALL accept two additional `ClientMessage`
types carrying the frontend-tool round-trip, decoded by the protocol package
like the existing types.

#### Scenario: publish_frontend_tools accepted while Idle

- **WHEN** a `publish_frontend_tools` message arrives while the session is `Idle`
- **THEN** the server accepts it (202) and registers the catalog for the session

#### Scenario: publish_frontend_tools rejected outside Idle

- **WHEN** a `publish_frontend_tools` message arrives while the session is not `Idle`
- **THEN** the server responds `409` with a `session_busy`-shaped body
- **AND** mirrors an `error` event to the SSE stream (matching the compact/POST
  conflict rule)

#### Scenario: frontend_tool_result accepted while Executing

- **WHEN** a `frontend_tool_result` message arrives while the session is `Executing`
- **THEN** the server accepts it (202) and routes it to the session's frontend
  bridge, resolving the matching suspended tool call

#### Scenario: frontend_tool_result for an unknown id is inert

- **WHEN** a `frontend_tool_result` carries an id with no suspended call (e.g. it
  already timed out)
- **THEN** the server accepts it (202) and drops it without error

### Requirement: Frontend-tool server events

The agent SHALL emit two additional server events through the session Outbox →
SSE path, wrapped with the standard `{type, idx, session_id}` envelope so
ordering and Last-Event-ID replay behave like every other event.

#### Scenario: frontend_tool_use emitted on invocation

- **WHEN** a frontend tool's `Run` begins
- **THEN** a `frontend_tool_use` event is emitted with `{tool_use_id, name, input}`

#### Scenario: frontend_tools_published emitted after registration

- **WHEN** a `publish_frontend_tools` message is processed
- **THEN** a `frontend_tools_published` event is emitted with
  `{registered:[...], rejected:[{name, reason}]}`

### Requirement: 会话与项目的 camelCase 线缆形状

会话相关端点 SHALL 以 **camelCase** 字段命名返回 JSON。统一的会话投影
`SessionMeta` SHALL 至少含 `id`、`workdir`、`title`、`status`(`'idle'|'running'`),
并 SHOULD 含 `createdAt`、`updatedAt`、`messageCount`、`lastMessagePreview`。
为保持单一 API 命名风格,create / get / list 端点 SHALL 同样返回 camelCase 的
`SessionMeta`(消除 snake_case 双轨)。

`ProjectMeta` SHALL 至少含 `path`,并 SHOULD 含 `sessionCount`、`updatedAt`。

`HistoryMessage` SHALL 含 `id`、`role`(`'user'|'assistant'`)与有序的 `parts[]`;
`parts[].type` ∈ { `text`、`reasoning`、`tool_call` },字段名 SHALL 与下游 SSE 事件
词汇及前端消费侧逐字一致(消费侧对 history 不做字段重映射):

- `text`:`content`
- `reasoning`:`text`、`status`(SHALL 物理出现且为 `"done"`)、`redacted?`
- `tool_call`:`id`、`name`、`input`、`status` ∈ `'done'|'error'`、`output?`

`tool_call` 的 `id`/`name` SHALL 是工具调用的对外标识与工具名(即下游事件里的
`id`/`name`),而非内部存储字段名。`status`(SessionMeta)SHALL 严格 ∈
`{ 'idle', 'running' }`;`title`(SessionMeta)SHALL 始终出现(可为空串,不得省略)。

#### Scenario: 会话投影为 camelCase

- **WHEN** 客户端 GET `/v1/sessions/{id}`
- **THEN** 服务返回 camelCase 的 `SessionMeta`(如 `createdAt`、`messageCount`),
  含 `status` ∈ `{ "idle", "running" }`、始终存在的 `title`

#### Scenario: history 的 reasoning part 携带 done 状态

- **WHEN** 客户端 GET `/v1/sessions/{id}/history`,某助手消息含一个 thinking 块
- **THEN** 对应 `parts[]` 元素为 `{ "type":"reasoning", "text":..., "status":"done", "redacted":<bool> }`,
  其中 `status` 字段物理存在

#### Scenario: history 的 tool_call 用对外字段名

- **WHEN** 客户端 GET `/v1/sessions/{id}/history`,某消息含一次工具调用及其结果
- **THEN** 对应 `parts[]` 元素为 `{ "type":"tool_call", "id":..., "name":..., "input":..., "status":"done"|"error", "output":... }`,
  使用 `id`/`name`/`input`/`output` 而非内部存储字段名

### Requirement: 项目会话管理端点

服务 SHALL 暴露以下端点以支撑项目/会话工作模型:

- `GET /v1/sessions?workdir=<path>` — 列出某项目会话 → `{ "sessions": [SessionMeta] }`
- `GET /v1/sessions/{id}/history` — 拉取 transcript → `{ "messages": [HistoryMessage] }`
- `PATCH /v1/sessions/{id}` — 重命名,body `{ "title": "<新标题>" }` → 更新后的 `SessionMeta`
- `DELETE /v1/sessions/{id}` — 删除会话及其 transcript → `2xx`(空体)
- `GET /v1/projects` — 列出已知项目 → `{ "projects": [ProjectMeta] }`

`GET …/history` 的 `messages[]` 缺失字段时消费者按容错处理;最小实现 SHALL 至少
产出 `text` part。

#### Scenario: 拉取 history 供 UI 重建

- **WHEN** 客户端 GET `/v1/sessions/{id}/history`
- **THEN** 服务返回 `{ "messages": [HistoryMessage] }`,`parts[]` 顺序与该会话
  下行事件的分段语义一致

#### Scenario: 重命名返回更新后的 SessionMeta

- **WHEN** 客户端 PATCH `/v1/sessions/{id}` 携带 `{ "title": "重构登录流程" }`
- **THEN** 服务返回 `200 OK` 和更新后 `title` 的 `SessionMeta`

#### Scenario: 删除会话返回空体 2xx

- **WHEN** 客户端 DELETE `/v1/sessions/{id}`(存在的会话)
- **THEN** 服务返回 `2xx`(空体),该会话及其 transcript 被删除

#### Scenario: 列出项目

- **WHEN** 客户端 GET `/v1/projects`
- **THEN** 服务返回 `{ "projects": [ProjectMeta] }`,每项至少含 `path`

