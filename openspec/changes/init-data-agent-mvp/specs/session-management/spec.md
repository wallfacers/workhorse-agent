## ADDED Requirements

### Requirement: 会话创建与销毁

服务 SHALL 支持创建、查询、列表、删除会话；每个会话有唯一 ULID。

创建会话 SHALL 接受可选参数：`workdir`、`env`、`provider`、`model`、`ephemeral`、`parent_id`、`agent_type`。缺省 `workdir` 取服务启动目录；缺省 `provider`/`model` 取全局默认。

#### Scenario: 创建会话默认值

- **WHEN** 客户端 POST `/v1/sessions` 仅传 `{}`
- **THEN** 服务用全局默认 provider/model/workdir 创建会话，返回新 session id 和初始状态 `idle`

#### Scenario: 销毁运行中的会话

- **WHEN** 会话正在 `Thinking` 状态时被 DELETE
- **THEN** 服务先取消正在进行的推理（级联到工具、子进程、子 session），再从内存与持久化中删除该会话记录

### Requirement: 会话状态机

每个会话 SHALL 有以下状态之一：`Idle`、`Thinking`、`AwaitPerm`、`Executing`、`Compacting`、`Cancelled`、`Crashed`。

状态转换 SHALL 遵循：

- `Idle` → `Thinking` ：收到 `user_message`
- `Thinking` → `AwaitPerm` ：LLM 返回 tool_use 且无适配权限规则
- `Thinking` → `Executing` ：LLM 返回 tool_use 且权限已允许
- `AwaitPerm` → `Executing` ：收到 `allow_*` 决策
- `AwaitPerm` → `Thinking` ：收到 `deny_*` 决策（带 deny 的 tool_result 回灌）
- `Executing` → `Thinking` ：tool 执行完成回灌 LLM
- 任意 → `Cancelled` ：收到 `interrupt`
- `Cancelled` → `Idle` ：收尾完成
- `Thinking` → `Compacting` ：触发压缩
- `Compacting` → `Idle` ：压缩完成

#### Scenario: Idle 状态接收 user_message 进入 Thinking

- **WHEN** 会话处于 `Idle` 状态时收到 `{"type":"user_message", "content":"hi"}`
- **THEN** 会话进入 `Thinking` 状态，开始调用 LLM

#### Scenario: Compacting 期间拒绝新消息

- **WHEN** 会话处于 `Compacting` 状态时收到 `user_message`
- **THEN** 服务 emit `error { code:"session_busy" }`，消息被丢弃；会话保持 `Compacting` 直至压缩完成

### Requirement: 持久化与 ephemeral 模式

服务 SHALL 默认将会话状态持久化到 SQLite（`~/.dataagent/state.db`）；含 `sessions`、`messages`、`events`、`tool_calls`、`permissions` 5 张表。

若创建会话时设 `ephemeral: true`，服务 SHALL 跳过所有持久化（仅在内存中维护）。

#### Scenario: 默认持久化

- **WHEN** 创建非 ephemeral 会话并发送一条 `user_message`，服务进程随后重启
- **THEN** 服务启动时从 SQLite 恢复该会话；GET `/v1/sessions/<id>` 返回该会话记录，含原始 history

#### Scenario: ephemeral 不入库

- **WHEN** 创建 `ephemeral: true` 会话并完成一轮对话，服务进程重启
- **THEN** 服务启动后该会话不存在；GET `/v1/sessions/<id>` 返回 `404`

### Requirement: 会话隔离

每个会话 SHALL 独立维护 `workdir`、`env map`、`history`、`context.CancelFunc`。

工具调用中的相对路径 SHALL 被 resolve 到会话的 `workdir`；默认拒绝访问 `workdir` 之外的路径（可通过会话配置加入 `allowed_paths` 白名单）。

#### Scenario: 跨会话 history 不互通

- **WHEN** 会话 A 进行 3 轮对话，会话 B 新建后查询 history
- **THEN** 会话 B 的 history 为空，与 A 完全隔离

#### Scenario: 工具拒绝 workdir 外路径

- **WHEN** 会话 workdir 为 `/home/u/proj`，工具被调用读取 `/etc/passwd`
- **THEN** 工具返回 `tool_result { is_error: true, output: "path outside workdir" }`，不读文件

### Requirement: 会话取消级联

调用 `POST /v1/sessions/{id}/cancel` 或客户端发 `interrupt` 消息 SHALL 触发会话级 `context.CancelFunc`，级联取消：

- 当前 provider HTTP 请求
- 所有正在执行的工具（Bash 子进程 SIGTERM 1.5s 后 SIGKILL；其他工具按 ctx.Done() 响应）
- 所有正在跑的子 session

取消 SHALL 是幂等的，重复调用无副作用。

#### Scenario: 取消正在跑的 Bash

- **WHEN** Bash 正在跑 `sleep 60` 时会话被 cancel
- **THEN** Bash 子进程在 1.5s 内被 SIGTERM 终止；emit `interrupted` 事件；会话状态回 `Idle`

#### Scenario: 取消的幂等性

- **WHEN** 客户端连续调用 `/cancel` 3 次
- **THEN** 服务仅执行一次取消流程，后续 2 次返回 `200 OK` 但无副作用
