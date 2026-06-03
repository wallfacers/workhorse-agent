## MODIFIED Requirements

### Requirement: 项目会话管理端点

服务 SHALL 暴露以下端点以支撑项目/会话工作模型:

- `GET /v1/sessions` — 不带 `workdir` 时,列出**全部项目**的持久会话(跨项目)
  → `{ "sessions": [SessionMeta] }`。该列表 SHALL 取自持久存储
  (`store.ListSessions`,而非仅内存活跃会话),并叠加实时状态:正在进行回合的
  会话报告 `running`,其余持久会话报告 `idle`。每个 `SessionMeta` SHALL 携带其
  `workdir`,以便客户端跨项目展示并按项目分列。
- `GET /v1/sessions?workdir=<path>` — 列出某项目会话(供应用内切换器,行为不变)
  → `{ "sessions": [SessionMeta] }`
- `GET /v1/sessions/{id}/history` — 拉取 transcript → `{ "messages": [HistoryMessage] }`
- `PATCH /v1/sessions/{id}` — 重命名,body `{ "title": "<新标题>" }` → 更新后的 `SessionMeta`
- `DELETE /v1/sessions/{id}` — 删除会话及其 transcript → `2xx`(空体)
- `GET /v1/projects` — 列出已知项目 → `{ "projects": [ProjectMeta] }`

`GET …/history` 的 `messages[]` 缺失字段时消费者按容错处理;最小实现 SHALL 至少
产出 `text` part。

#### Scenario: 不带 workdir 列出全部项目的持久会话

- **WHEN** 存在项目 `P`、`Q` 各自的持久会话(含仅空闲、未活跃的),客户端
  GET `/v1/sessions`(无 `workdir` 参数)
- **THEN** 服务返回 `{ "sessions": [SessionMeta] }`,包含 `P` 与 `Q` 两个项目的
  会话,每项携带其 `workdir`
- **AND** 非活跃项目中仅空闲的持久会话也出现在列表中(取自持久存储而非内存活跃集)

#### Scenario: 带 workdir 仍按项目过滤

- **WHEN** 客户端 GET `/v1/sessions?workdir=P`
- **THEN** 服务仅返回项目 `P` 的会话(行为与既有一致)

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
