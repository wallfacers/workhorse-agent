## ADDED Requirements

### Requirement: HTTP REST 端点集合

服务 SHALL 在 `127.0.0.1:7821`（默认，可配）暴露以下 HTTP 端点：

- `POST /v1/sessions` — 创建会话，请求体含 `workdir`、`env`、`provider`、`model`、`ephemeral?`，返回 session id
- `GET /v1/sessions` — 列出所有会话
- `GET /v1/sessions/{id}` — 会话详情
- `DELETE /v1/sessions/{id}` — 销毁会话
- `POST /v1/sessions/{id}/cancel` — 中断当前推理
- `POST /v1/sessions/{id}/compact` — 手动触发上下文压缩
- `GET /v1/sessions/{id}/stream` — WebSocket 升级
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

### Requirement: WebSocket 全双工事件流

服务 SHALL 在 `/v1/sessions/{id}/stream` 接受 WebSocket 升级请求；所有 Server↔Client 消息均为 JSON 对象，含 `type` 字段。

#### Scenario: WebSocket 升级成功

- **WHEN** 客户端发起 HTTP Upgrade 到 `/v1/sessions/<existing-id>/stream`，含合法 `Sec-WebSocket-Key`
- **THEN** 服务返回 `101 Switching Protocols` 并完成 WebSocket 握手

#### Scenario: WebSocket 升级失败（会话不存在）

- **WHEN** 客户端发起升级到 `/v1/sessions/<不存在的 id>/stream`
- **THEN** 服务返回 `404 Not Found`，不进行升级

### Requirement: Client → Server 消息类型

服务 SHALL 接受以下 5 种 Client → Server 消息：

| `type` | 字段 | 语义 |
|---|---|---|
| `user_message` | `content: string`, `attachments?: []` | 新一轮用户消息 |
| `permission_decision` | `request_id`, `decision: "allow_once"\|"allow_session"\|"allow_permanent"\|"deny"\|"deny_permanent"` | 响应权限询问 |
| `interrupt` | （无） | 中断当前推理 |
| `ping` | （无） | 心跳 |
| `context_update` | `workdir?`, `files?` | 元数据更新 |

未知 `type` SHALL 被忽略并 emit `error` 事件 `{code:"unknown_message_type"}`，不断连接。

#### Scenario: 未知消息类型被拒绝

- **WHEN** 客户端发送 `{"type":"frobnicate"}`
- **THEN** 服务 emit `{"type":"error", "code":"unknown_message_type", "message":"..."}` 但不关闭 WebSocket

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

### Requirement: 断线重连按事件序号增量同步

服务 SHALL 在客户端重连请求中接受 `?since_event_idx=N` 查询参数；若给定，服务先按序重发 `idx > N` 的所有 `events`，再继续转发实时事件。

LLM 推理 SHALL 在客户端断开期间继续进行，不被打断。

#### Scenario: 客户端断线期间推理继续

- **WHEN** 客户端 A 触发一次推理，期间断开 WebSocket，3 秒后用 `?since_event_idx=N` 重连
- **THEN** 服务在 A 断开期间继续 LLM 推理与工具执行，重连后先按序推 `idx > N` 的所有事件再继续实时事件

### Requirement: ID 格式

所有外部可见的 ID（`session_id`、`message_id`、`request_id`、`agent_id`、事件 `id`）SHALL 为 ULID（128-bit，时间有序，Crockford Base32 编码 26 字符）。

#### Scenario: 创建的 session ID 是 ULID

- **WHEN** POST `/v1/sessions` 创建一个会话
- **THEN** 响应的 `id` 字段匹配正则 `^[0-9A-HJKMNP-TV-Z]{26}$`
