## MODIFIED Requirements

### Requirement: HTTP REST 端点集合

服务 SHALL 在 `127.0.0.1:7821`（默认，可配）暴露以下 HTTP 端点：

- `GET /v1/sessions` — 列出会话;携带 `?workdir=<path>` 时按项目分桶过滤;不携带 `workdir` 时返回全量持久化会话列表并叠加 live status
- `GET /v1/sessions/{id}` — 会话详情(`SessionMeta`)
- `GET /v1/sessions/{id}/history` — 拉取 transcript(`{ "messages": [HistoryMessage] }`)
- `PATCH /v1/sessions/{id}` — 重命名(`{ "title" }`)
- `DELETE /v1/sessions/{id}` — 删除会话及其 transcript(硬删 + 级联)
- `POST /v1/sessions/{id}/cancel` — 中断当前推理
- `POST /v1/sessions/{id}/compact` — 手动触发上下文压缩
- `POST /v1/sessions/{id}/stream` — Streamable HTTP 客户端消息提交端点
- `GET /v1/sessions/{id}/stream` — Streamable HTTP 服务端事件流（SSE）;SHALL 能为
  已存在、可能 idle(非刚创建)的会话工作
- `GET /v1/fs/list` — 列出目录内容；接受 `?path=<dir>` 和可选 `?root=<project_root>` 参数；`root` 限定可浏览范围
- `GET /v1/projects` — 列出已知项目(`{ "projects": [ProjectMeta] }`)
- `DELETE /v1/projects?workdir=<path>` — 删除某项目记录:硬删该 `workdir` 下全部会话
  (每个会话先取消在跑 turn 再级联硬删 transcript),不触碰磁盘目录;返回
  `{ "deleted": <int> }`。缺省/空 `workdir` SHALL 返回 `400`;无匹配会话时为
  幂等成功(`deleted: 0`)
- `GET /debug/sessions/{id}/events?since=N` — 事件回放（DEBUG 模式）
- `GET /health` — 健康检查，返回 `ok`、`version`、`protocol_version`、`capabilities`、`uptime_sec`、`sessions_active`、`default_workdir`、`platform`
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

#### Scenario: 删除项目记录

- **WHEN** 客户端 DELETE `/v1/projects?workdir=/tmp/proj`,该 workdir 下有 N 个会话
- **THEN** 服务硬删这 N 个会话(先取消在跑 turn,级联清 transcript),返回 `200` 与
  `{ "deleted": N }`
- **AND** 随后 `GET /v1/projects` 不再包含该 workdir,`GET /v1/sessions?workdir=/tmp/proj` 为空
- **AND** 磁盘上的 `/tmp/proj` 目录不被改动或删除

#### Scenario: 删除项目缺省 workdir

- **WHEN** 客户端 DELETE `/v1/projects`(未携带 `workdir`)或 `workdir` 为空
- **THEN** 服务返回 `400 Bad Request`

#### Scenario: 健康检查

- **WHEN** 客户端 GET `/health`
- **THEN** 服务返回 `200 OK` 和 `{ "ok": true, "version": "<semver>", "uptime_sec": <int>, "sessions_active": <int>, "protocol_version": "<string>", "capabilities": [<string>, ...] }`
