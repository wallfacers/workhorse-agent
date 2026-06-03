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

- **WHEN** 客户端 DELETE `/v1/sessions/{id}`
- **THEN** 服务返回 `2xx`(空体),该会话不再出现在任何列举中,其 transcript
  级联删除

### Requirement: HistoryMessage 含 interrupted 标志

`HistoryMessage` SHALL 含 `id`、`role`(`'user'|'assistant'`)与有序的 `parts[]`;
`parts[].type` ∈ { `text`、`reasoning`、`tool_call` },字段名 SHALL 与下游 SSE 事件
词汇及前端消费侧逐字一致(消费侧对 history 不做字段重映射):

- `text`:`content`
- `reasoning`:`text`、`status`(SHALL 物理出现且为 `"done"`)、`redacted?`
- `tool_call`:`id`、`name`、`input`、`status` ∈ `'done'|'error'`、`output?`

`tool_call` 的 `id`/`name` SHALL 是工具调用的对外标识与工具名(即下游事件里的
`id`/`name`),而非内部存储字段名。`status`(SessionMeta)SHALL 严格 ∈
`{ 'idle', 'running' }`;`title`(SessionMeta)SHALL 始终出现(可为空串,不得省略)。

`HistoryMessage` SHALL 包含可选的 `interrupted` 字段（`boolean`）。该字段 SHALL
在消息级出现（非 `parts[]` 内），仅对 `role: "assistant"` 的消息有意义。消费者
SHALL 容忍该字段缺失（`undefined` 视为 `false`）。

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

#### Scenario: 中断消息的 history 含 interrupted 标志

- **WHEN** 客户端 GET `/v1/sessions/{id}/history`,某助手消息在中断回合中被标记为 interrupted
- **THEN** 该消息的 JSON 对象 SHALL 包含 `"interrupted": true` 字段
- **AND** 客户端重建消息列表时该消息 SHALL 显示中断标记

#### Scenario: 未中断消息不含 interrupted 字段

- **WHEN** 客户端 GET `/v1/sessions/{id}/history`,某消息未被中断
- **THEN** 该消息的 JSON 对象 SHALL NOT 包含 `"interrupted": true`（可为 `false` 或缺省）
