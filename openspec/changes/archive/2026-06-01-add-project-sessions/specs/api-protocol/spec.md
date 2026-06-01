## ADDED Requirements

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

## MODIFIED Requirements

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
