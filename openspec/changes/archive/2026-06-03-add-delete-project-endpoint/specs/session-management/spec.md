## ADDED Requirements

### Requirement: 项目级删除(按 workdir 级联硬删会话)

服务 SHALL 支持按 `workdir` 删除一个"项目记录"。由于项目是会话的派生视图(distinct
`workdir` 且有未删除会话),删除项目 SHALL 等价于硬删该 `workdir` 下的**全部**会话:
对每个会话先取消任何在跑 turn(级联到工具、子进程、子 session),再从内存与持久化
中移除该会话并级联删除其 transcript(messages / events / tool_calls),复用与
`DELETE /v1/sessions/{id}` 相同的优雅停 + 硬删路径。该操作 SHALL NOT 改动或删除磁盘
上的 `workdir` 目录。删除后该 `workdir` 不再出现在 `GET /v1/projects`。

#### Scenario: 删除项目级联硬删其全部会话

- **WHEN** `workdir` `P` 下有若干已落盘会话,客户端 DELETE `/v1/projects?workdir=P`
- **THEN** 服务逐个取消在跑 turn 并硬删这些会话(级联清 messages / events / tool_calls)
- **AND** 返回 `{ "deleted": <数量> }`
- **AND** 之后 `GET /v1/projects` 不含 `P`,`GET /v1/sessions?workdir=P` 为空

#### Scenario: 不触碰磁盘目录

- **WHEN** 删除项目 `P` 完成
- **THEN** 仅会话记录被移除;`P` 对应的文件系统目录仍存在,可被重新作为新项目打开

#### Scenario: 无会话的 workdir 为幂等成功

- **WHEN** 客户端 DELETE `/v1/projects?workdir=Q`,而 `Q` 没有任何未删除会话
- **THEN** 服务返回 `200` 与 `{ "deleted": 0 }`,不报错

#### Scenario: 删除运行中项目会话先取消再删

- **WHEN** `P` 下某会话正处于 `Thinking`,客户端 DELETE `/v1/projects?workdir=P`
- **THEN** 服务先取消该会话的推理(级联收尾),再硬删,行为与单会话删除一致
