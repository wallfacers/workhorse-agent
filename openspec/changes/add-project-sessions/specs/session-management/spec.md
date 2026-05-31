## ADDED Requirements

### Requirement: 会话 transcript 持久化

服务 SHALL 把每个非 ephemeral 会话的对话消息(`provider.Message` 序列)持久化,
使其在进程重启后仍可被读取与重建。持久化的消息表示 SHALL 无损保留全部内容块类型,
**包括 thinking 块的 `signature`**,以保证续聊时把历史回灌为模型上下文后,与
provider 的 API round-trip 仍然有效。ephemeral 会话 SHALL NOT 持久化消息。

当会话上下文被压缩(替换历史)时,服务 SHALL 同步更新持久化表示,使持久化的
transcript 与喂给模型的上下文保持一致。

#### Scenario: 重启后消息可重建

- **WHEN** 一个非 ephemeral 会话发生若干轮对话后 sidecar 进程重启
- **THEN** 该会话的消息仍可从持久化存储读取,用于重建 UI 历史与模型上下文

#### Scenario: thinking 签名随消息持久化

- **WHEN** 一个启用 extended thinking 的会话产生含 thinking 块的助手消息并被持久化,
  之后会话被水合并继续对话
- **THEN** 回灌的历史保留 thinking 块的 `signature`,对 provider 的后续请求不因缺失
  签名而被拒绝

#### Scenario: ephemeral 会话不落盘

- **WHEN** 一个 `ephemeral: true` 的会话产生消息
- **THEN** 服务不持久化其消息

### Requirement: 项目分桶的会话列举

服务 SHALL 以持久化存储为 source-of-truth 列举会话,并支持按项目路径
(`workdir`)分桶。列举结果 SHALL 在进程**重启后**仍可返回(不依赖会话是否在
内存中加载)。

每个列举条目 SHALL 至少包含 `id`、`workdir`、`title`、`status`,并 SHOULD 包含
`createdAt`、`updatedAt`、`messageCount`、`lastMessagePreview`。

#### Scenario: 按 workdir 列出某项目会话

- **WHEN** 客户端 GET `/v1/sessions?workdir=/home/user/proj`
- **THEN** 服务返回 `200 OK` 和 `{ "sessions": [SessionMeta] }`,仅含 `workdir`
  等于该路径且未删除的会话

#### Scenario: 重启后仍可列出持久化会话

- **WHEN** 某项目下已有会话落盘,sidecar 进程重启后客户端 GET
  `/v1/sessions?workdir=<该项目>`
- **THEN** 服务从持久化存储返回这些会话(无需它们在内存中处于活动状态)

### Requirement: 会话状态对外投影(idle|running)

服务 SHALL 把内部 6 态状态机投影为对外二元 `status`:`Idle` 与 `Cancelled`
投影为 `idle`;`Thinking`、`AwaitPerm`、`Executing`、`Compacting` 投影为
`running`。持久化但未加载到内存的会话 SHALL 投影为 `idle`。

#### Scenario: 在跑的会话标记为 running

- **WHEN** 某会话有一轮 turn 正在服务端推进时被列举或查询
- **THEN** 其 `status` 为 `running`

#### Scenario: 空闲会话标记为 idle

- **WHEN** 某会话无在跑 turn(含刚重启后尚未加载的会话)被列举或查询
- **THEN** 其 `status` 为 `idle`

### Requirement: 持久化会话的按需水合与重开

服务 SHALL 能为一个"已持久化但当前未加载到内存"的会话按需**水合**出活动会话:
在 `GET /v1/sessions/{id}/stream`、`POST /v1/sessions/{id}/stream` 或
`GET /v1/sessions/{id}` 命中此类会话时,从存储重建会话对象,登记后与活动会话
走同一推进路径。水合 SHALL 并发安全(并发请求只水合一次)。`GET …/stream`
SHALL 能为一个已存在、可能处于 idle(非刚创建)的会话工作,重开后续 turn 的
事件正常下发。

#### Scenario: 重开一个 idle 会话继续对话

- **WHEN** 客户端对一个已存在、处于 idle 的会话重开 `GET /v1/sessions/{id}/stream`
  并随后 POST 一条 `user_message`
- **THEN** 服务水合该会话、推进新一轮 turn,并通过该流正常下发事件

#### Scenario: 重开不存在的会话

- **WHEN** 客户端对一个不存在或已删除的会话 id 重开 `GET …/stream`
- **THEN** 服务返回 `404 Not Found`

### Requirement: 续聊的模型上下文连续性

服务 SHALL 在水合一个会话时,用持久化的 transcript 重建该会话的模型上下文,
使切回旧会话继续对话在**模型记忆层面**也连续,而不仅是 UI 层面的历史展示。

#### Scenario: 切回旧会话带历史续答

- **WHEN** 客户端切回一个 idle 旧会话并发送 `user_message`
- **THEN** 模型基于该会话的完整历史上下文作答(而非从空上下文开始)

### Requirement: 会话标题派生与重命名

每个会话 SHALL 有一个 `title` 字段。服务 SHALL 在落该会话第一条用户消息时,
从首条用户文本派生 `title`(做长度截断与单行化)。`title` 可由后续重命名覆盖,
可为空串(由前端显示为"未命名会话")。

#### Scenario: 首条消息派生标题

- **WHEN** 一个新会话收到其第一条 `user_message`
- **THEN** 服务从该消息文本派生并持久化 `title`

#### Scenario: 重命名会话

- **WHEN** 客户端 PATCH `/v1/sessions/{id}` 携带 `{ "title": "新标题" }`
- **THEN** 服务更新该会话标题并在后续列举/查询中反映新 `title`

### Requirement: 项目派生

服务 SHALL 由已持久化(未删除)会话的 `workdir` 派生项目列表。每个项目 SHALL
至少含 `path`,并 SHOULD 含 `sessionCount`、`updatedAt`。本能力 SHALL NOT 包含
没有任何会话的路径。

#### Scenario: 列出已知项目

- **WHEN** 客户端 GET `/v1/projects`
- **THEN** 服务返回 `{ "projects": [ProjectMeta] }`,每项对应一个有未删除会话的
  `workdir`,含其会话数

## MODIFIED Requirements

### Requirement: 会话创建与销毁

服务 SHALL 支持创建、查询、列表、删除会话；每个会话有唯一 ULID。

创建会话 SHALL 接受可选参数：`workdir`、`env`、`provider`、`model`、`ephemeral`、`parent_id`、`agent_type`。缺省 `workdir` 取服务启动目录；缺省 `provider`/`model` 取全局默认。

删除会话 SHALL 为**硬删除**:在取消任何在跑 turn(级联到工具、子进程、子 session)
后,从内存与持久化中移除该会话记录,并**级联删除该会话的 transcript**
(messages / events / tool_calls)。删除后该会话不再出现在任何列举中。

#### Scenario: 创建会话默认值

- **WHEN** 客户端 POST `/v1/sessions` 仅传 `{}`
- **THEN** 服务用全局默认 provider/model/workdir 创建会话，返回新 session id 和初始状态 `idle`

#### Scenario: 销毁运行中的会话

- **WHEN** 会话正在 `Thinking` 状态时被 DELETE
- **THEN** 服务先取消正在进行的推理（级联到工具、子进程、子 session），再从内存与持久化中删除该会话记录

#### Scenario: 删除会话级联清除 transcript

- **WHEN** 客户端 DELETE 一个已落盘多条消息的会话
- **THEN** 服务硬删该会话行,并级联删除其全部 messages / events / tool_calls;
  随后 `GET /v1/sessions?workdir=<该项目>` 不再包含该会话,且其 transcript 已不可
  通过 `GET …/history` 读到
