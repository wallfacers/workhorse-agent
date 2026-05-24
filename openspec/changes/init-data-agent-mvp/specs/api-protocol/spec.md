## ADDED Requirements

### Requirement: HTTP REST 端点集合

服务 SHALL 在 `127.0.0.1:7821`（默认，可配）暴露以下 HTTP 端点：

- `POST /v1/sessions` — 创建会话，请求体含 `workdir`、`env`、`provider`、`model`、`ephemeral?`，返回 session id
- `GET /v1/sessions` — 列出所有会话
- `GET /v1/sessions/{id}` — 会话详情
- `DELETE /v1/sessions/{id}` — 销毁会话
- `POST /v1/sessions/{id}/cancel` — 中断当前推理
- `POST /v1/sessions/{id}/compact` — 手动触发上下文压缩
- `POST /v1/sessions/{id}/stream` — Streamable HTTP 客户端消息提交端点
- `GET /v1/sessions/{id}/stream` — Streamable HTTP 服务端事件流（SSE）
- `GET /debug/sessions/{id}/events?since=N` — 事件回放（DEBUG 模式）
- `GET /health` — 健康检查
- `GET /ui` — 内嵌参考 Web UI

#### Scenario: 创建会话

- **WHEN** 客户端 POST `/v1/sessions` 携带 `{ "workdir": "/tmp/proj", "provider": "anthropic", "model": "claude-sonnet-4-6" }`
- **THEN** 服务返回 `201 Created` 和 JSON `{ "id": "<ULID>", "status": "idle", "workdir": "/tmp/proj", ... }`

#### Scenario: 销毁不存在的会话

- **WHEN** 客户端 DELETE `/v1/sessions/<不存在的 id>`
- **THEN** 服务返回 `404 Not Found`

#### Scenario: 健康检查

- **WHEN** 客户端 GET `/health`
- **THEN** 服务返回 `200 OK` 和 `{ "ok": true, "version": "<semver>", "uptime_sec": <int>, "sessions_active": <int> }`

### Requirement: Streamable HTTP 传输

服务 SHALL 在 `/v1/sessions/{id}/stream` 实现 MCP 2025-11-25 Streamable HTTP 传输模型：

- `POST /v1/sessions/{id}/stream` SHALL 接受 `Content-Type: application/json` 的请求体（单个 ClientEvent JSON 对象，见下一 Requirement）
- `GET /v1/sessions/{id}/stream` SHALL 返回 `Content-Type: text/event-stream`，开启长连接 SSE 流，按 SSE 格式（`id: <idx>\nevent: <type>\ndata: <json>\n\n`）推送该 session 的所有 Server→Client 事件
- POST 默认 SHALL 以 `202 Accepted` 无 body 响应；事件流走 GET 通道
- GET SSE 流 SHALL 每 25 秒（可配）发送 `: keep-alive\n\n` SSE 注释作为心跳
- 同一 session 同一时刻 SHALL 最多保持一条活跃 GET SSE 流；新的 GET 请求到来时服务器 SHALL 关闭旧流（向旧流发 SSE comment `: superseded`）

#### Scenario: POST 客户端消息默认返回 202

- **WHEN** 客户端 POST `/v1/sessions/<existing-id>/stream` 携带 `{"type":"user_message","content":"hi"}`
- **THEN** 服务返回 `202 Accepted` 无 body；session 状态从 Idle 进入 Thinking；该次推理产生的事件随后在 GET SSE 流上推送

#### Scenario: GET SSE 流推送事件

- **WHEN** 客户端已 GET `/v1/sessions/<id>/stream` 开启 SSE，随后 LLM 产生一个 text delta
- **THEN** 服务在 SSE 流上写出 `id: <idx>\nevent: assistant_text_delta\ndata: {"delta":"...","session_id":"..."}\n\n`

#### Scenario: POST 到不存在会话

- **WHEN** 客户端 POST `/v1/sessions/<不存在的 id>/stream`
- **THEN** 服务返回 `404 Not Found`

#### Scenario: 并发 GET 关闭旧流

- **WHEN** 客户端 A 已对 session X 开启 GET SSE，客户端 B 对同一 session X 发起新 GET
- **THEN** 服务向 A 的 SSE 流写 `: superseded` 注释并关闭该响应；新流服务客户端 B

### Requirement: Origin 校验

服务 SHALL 在所有 `/v1/sessions/{id}/stream` 的 POST 与 GET 请求上校验 `Origin` header：

- 默认白名单：缺失 `Origin`（同源 / file:// / curl 等）、`http://127.0.0.1:*`、`http://localhost:*`、`https://127.0.0.1:*`、`https://localhost:*`
- 不在白名单的 `Origin` SHALL 拒绝 `403 Forbidden`
- 白名单 SHALL 可通过 config.yaml 的 `allowed_origins` 字段扩展

此校验目的是防 DNS rebinding 攻击（MCP 2025-11-25 spec 强制要求）。

#### Scenario: 拒绝跨站 Origin

- **WHEN** 浏览器从 `https://evil.com` 通过 XHR 调 `POST http://127.0.0.1:7821/v1/sessions/<id>/stream`
- **THEN** 服务返回 `403 Forbidden`，不处理请求

#### Scenario: 同源与本地允许

- **WHEN** 请求 Origin 为 `http://localhost:5173`（用户自定义 UI 开发服）
- **THEN** 服务正常处理请求

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

服务 SHALL 以以下 10 种事件向客户端推送：

`assistant_text_delta`、`assistant_text_done`、`tool_call_start`、`tool_call_done`、`permission_request`、`subagent_event`、`compaction`、`error`、`interrupted`、`pong`。

所有事件 SHALL 包含 `idx`（事件序号，单调递增）、`session_id`，并在持久化模式下写入 `events` 表。

#### Scenario: 文本流式推送

- **WHEN** LLM 输出 "hello world" 的 token 流
- **THEN** 服务依次 emit 多个 `assistant_text_delta`，最后 emit 一个 `assistant_text_done` 含 `message_id`

#### Scenario: 工具调用事件配对

- **WHEN** Agent 调用 `Bash` 工具执行 `"ls"`
- **THEN** 服务先 emit `tool_call_start { id, tool:"Bash", input }`，工具完成后 emit `tool_call_done { id, output, ok: true, took_ms }`

### Requirement: 断线重连按 Last-Event-ID 增量同步

服务 SHALL 在 GET `/v1/sessions/{id}/stream` 请求 header 中接受标准 SSE `Last-Event-ID: <idx>`；若给定，服务在打开新 SSE 流后 SHALL 先按 `idx` 升序回放 `events` 表中 `idx > Last-Event-ID` 的所有事件，再切入实时事件转发。

每条 SSE event 的 `id:` 字段 SHALL 等于该事件在 `events` 表中的 `idx`（单调递增整数）。

为兼容部分客户端，服务 SHALL 同时接受查询参数 `?last_event_id=N` 作为 header 缺失时的备选；header 与 query 同时给定时以 header 为准。

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

### Requirement: ID 格式

所有外部可见的 ID（`session_id`、`message_id`、`request_id`、`agent_id`、事件 `id`）SHALL 为 ULID（128-bit，时间有序，Crockford Base32 编码 26 字符）。

#### Scenario: 创建的 session ID 是 ULID

- **WHEN** POST `/v1/sessions` 创建一个会话
- **THEN** 响应的 `id` 字段匹配正则 `^[0-9A-HJKMNP-TV-Z]{26}$`
