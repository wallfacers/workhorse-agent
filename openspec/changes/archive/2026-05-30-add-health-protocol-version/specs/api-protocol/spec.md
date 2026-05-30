## MODIFIED Requirements

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

`GET /health` 响应除既有的 `ok`、`version`、`uptime_sec`、`sessions_active`
字段外，SHALL 额外包含 `protocol_version`（标识服务所讲 wire 协议版本的字符串）
与 `capabilities`（列出受支持具名特性的字符串数组）。这两个字段向后兼容：仅读取
既有字段的消费者不受影响。

#### Scenario: 创建会话

- **WHEN** 客户端 POST `/v1/sessions` 携带 `{ "workdir": "/tmp/proj", "provider": "anthropic", "model": "claude-sonnet-4-6" }`
- **THEN** 服务返回 `201 Created` 和 JSON `{ "id": "<ULID>", "status": "idle", "workdir": "/tmp/proj", ... }`

#### Scenario: 销毁不存在的会话

- **WHEN** 客户端 DELETE `/v1/sessions/<不存在的 id>`
- **THEN** 服务返回 `404 Not Found`

#### Scenario: 健康检查

- **WHEN** 客户端 GET `/health`
- **THEN** 服务返回 `200 OK` 和 `{ "ok": true, "version": "<semver>", "uptime_sec": <int>, "sessions_active": <int>, "protocol_version": "<string>", "capabilities": [<string>, ...] }`
- **AND** `protocol_version` 标识服务所讲的 wire 协议版本（如 `"1"`），`capabilities` 列出受支持的具名特性且包含 `frontend_tools`

#### Scenario: Debug events 回放

- **WHEN** 客户端 GET `/debug/sessions/<id>/events?since=42`
- **THEN** 服务返回 `200 OK` + `application/x-ndjson` 流，每行一条 idx>42 的事件 JSON
