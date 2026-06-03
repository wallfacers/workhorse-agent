# api-protocol Delta Spec

## MODIFIED Requirements

### Requirement: HTTP REST 端点集合

服务 SHALL 在 `127.0.0.1:7821`（默认，可配）暴露以下 HTTP 端点：

- `POST /v1/sessions` — 创建会话，请求体含 `workdir`、`env`、`provider`、`model`、`ephemeral?`，返回 `SessionMeta`(camelCase)
- `GET /v1/sessions` — 列出会话;携带 `?workdir=<path>` 时按项目分桶过滤;不携带 `workdir` 时返回全量持久化会话列表并叠加 live status
- `GET /v1/sessions/{id}` — 会话详情(`SessionMeta`)
- `GET /v1/sessions/{id}/history` — 拉取 transcript(`{ "messages": [HistoryMessage] }`)
- `PATCH /v1/sessions/{id}` — 重命名(`{ "title" }`)
- `DELETE /v1/sessions/{id}` — 删除会话及其 transcript(硬删 + 级联)
- `POST /v1/sessions/{id}/cancel` — 中断当前推理
- `POST /v1/sessions/{id}/compact` — 手动触发上下文压缩
- `GET /v1/sessions/{id}/stream` — SSE 事件流（GET 打开流，POST 发送消息）
- `GET /v1/fs/list` — 列出目录内容；接受 `?path=<dir>` 和可选 `?root=<project_root>` 参数；`root` 限定可浏览范围
- `GET /health` — 健康检查，返回 `ok`、`version`、`protocol_version`、`capabilities`、`uptime_sec`、`sessions_active`、`default_workdir`、`platform`

#### Scenario: 不携带 workdir 列出所有会话

- **WHEN** 客户端 `GET /v1/sessions` 不携带 `?workdir=` 参数
- **THEN** 服务返回全量持久化会话列表（来自 store），每条包含 `workdir` 字段
- **AND** 对每个持久化会话叠加 live status：在 manager 中存活的标记 `running`/`idle`，不在内存中的标记 `idle`

#### Scenario: 携带 workdir 按项目过滤（行为不变）

- **WHEN** 客户端 `GET /v1/sessions?workdir=<P>`
- **THEN** 服务仅返回 workdir 为 `P` 的会话（现有行为保持不变）

### Requirement: /v1/fs/list 请求范围限定

`GET /v1/fs/list` SHALL 接受可选 `?root=<project_root>` 参数。当提供 `root` 时，路径限定 SHALL 以 `root` 为边界（而非全局 `server.default_workdir`）。当未提供 `root` 时，回退到全局 `server.default_workdir` 作为限定边界。无论哪种情况，虚拟文件系统路径（`/proc`、`/sys`、`/dev`、`/run`）SHALL 始终被拒绝。

#### Scenario: 使用显式 root 浏览项目目录

- **WHEN** 客户端 `GET /v1/fs/list?path=/src&root=/home/user/myproject`
- **AND** `/home/user/myproject/src` 存在且在 `root` 范围内
- **THEN** 服务返回该目录的条目列表

#### Scenario: 路径逃逸显式 root 被拒绝

- **WHEN** 客户端 `GET /v1/fs/list?path=/etc&root=/home/user/myproject`
- **AND** `/etc` 不在 `/home/user/myproject` 范围内
- **THEN** 服务返回 `403 forbidden`

#### Scenario: 未提供 root 时回退到全局默认

- **WHEN** 客户端 `GET /v1/fs/list?path=/src` 未携带 `?root=`
- **THEN** 服务使用全局 `server.default_workdir` 作为限定边界（现有行为）

### Requirement: /health default_workdir 解析

`GET /health` 的 `default_workdir` 字段 SHALL 按以下优先级解析：
1. `server.default_workdir` 配置覆盖（若设置）
2. `os.UserHomeDir()`（用户 home 目录）
3. 若以上均不可解析，省略该字段（或返回空字符串）

`default_workdir` SHALL NOT 回退到 sidecar 进程的启动目录（`os.Getwd()`）。

#### Scenario: 无配置覆盖时回退到 home

- **WHEN** sidecar 未配置 `server.default_workdir`
- **THEN** `GET /health` 返回 `default_workdir` 为用户 home 目录
- **AND** 该值 SHALL NOT 为 sidecar 进程的启动目录

#### Scenario: 配置覆盖优先生效

- **WHEN** `server.default_workdir` 设置为 `P`
- **THEN** `GET /health` 返回 `default_workdir = P`

#### Scenario: 无覆盖且无法解析 home

- **WHEN** sidecar 无配置覆盖且无法解析用户 home 目录
- **THEN** `GET /health` 省略 `default_workdir`（或返回空字符串）
- **AND** SHALL NOT 报告 sidecar 进程的启动目录
