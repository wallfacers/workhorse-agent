# session-management Delta Spec

## MODIFIED Requirements

### Requirement: 会话创建与销毁

服务 SHALL 支持创建、查询、列表、删除会话；每个会话有唯一 ULID。

创建会话 SHALL 接受可选参数：`workdir`、`env`、`provider`、`model`、`ephemeral`、`parent_id`、`agent_type`。缺省 `workdir` 取 `server.default_workdir` 配置（若设置），否则为 `os.UserHomeDir()`；不再回退到进程启动目录。缺省 `provider`/`model` 取全局默认。

删除会话 SHALL 为**硬删除**:在取消任何在跑 turn(级联到工具、子进程、子 session)
后,从内存与持久化中移除该会话记录,并**级联删除该会话的 transcript**
(messages / events / tool_calls)。删除后该会话不再出现在任何列举中。

#### Scenario: 创建会话默认 workdir

- **WHEN** 客户端 POST `/v1/sessions` 仅传 `{}`（不指定 workdir）
- **THEN** 服务用 `server.default_workdir`（若配置）或 `os.UserHomeDir()` 作为 workdir 创建会话
- **AND** SHALL NOT 使用进程启动目录作为默认 workdir

#### Scenario: 销毁运行中的会话

- **WHEN** 会话正在 `Thinking` 状态时被 DELETE
- **THEN** 服务先取消正在进行的推理（级联到工具、子进程、子 session），再从内存与持久化中删除该会话记录

#### Scenario: 删除会话级联清除 transcript

- **WHEN** 客户端 DELETE 一个已落盘多条消息的会话
- **THEN** 服务硬删该会话行,并级联删除其全部 messages / events / tool_calls;
  随后 `GET /v1/sessions?workdir=<该项目>` 不再包含该会话,且其 transcript 已不可恢复

### Requirement: 会话列表跨项目视图

`GET /v1/sessions`（不携带 `?workdir=`）SHALL 返回全量持久化会话列表（通过 `store.ListSessions`），而非仅返回内存中的活跃会话。每条返回的会话 SHALL 包含其 `workdir` 字段。对每个持久化会话 SHALL 叠加 live status：在 `session.Manager` 中存活且处于 running 状态的标记 `running`，否则标记 `idle`。携带 `?workdir=P` 时的过滤行为 SHALL 保持不变。

#### Scenario: 无 workdir 列出跨项目全量会话

- **WHEN** 客户端 `GET /v1/sessions` 不携带 `?workdir=` 参数
- **AND** store 中有项目 A（workdir=/a）的 2 个会话和项目 B（workdir=/b）的 1 个会话
- **THEN** 返回 3 个会话，每条包含 `workdir` 字段（分别为 `/a`、`/a`、`/b`）

#### Scenario: 无 workdir 列表包含非活跃会话

- **WHEN** 客户端 `GET /v1/sessions` 不携带 `?workdir=`
- **AND** store 中有一个 idle 会话（不在 manager 内存中）
- **THEN** 该会话出现在返回列表中，status 为 `idle`，且包含其 `workdir`

#### Scenario: 携带 workdir 过滤行为不变

- **WHEN** 客户端 `GET /v1/sessions?workdir=/a`
- **THEN** 仅返回 workdir 为 `/a` 的会话（现有行为保持不变）
