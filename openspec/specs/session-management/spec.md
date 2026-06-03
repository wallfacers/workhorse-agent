# session-management Specification

## Purpose
TBD - created by archiving change init-workhorse-agent-mvp. Update Purpose after archive.
## Requirements
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
  随后 `GET /v1/sessions?workdir=<该项目>` 不再包含该会话,且其 transcript 已不可
  通过 `GET …/history` 读到

### Requirement: 会话状态机

每个会话 SHALL 有以下 6 种状态之一：`Idle`、`Thinking`、`AwaitPerm`、`Executing`、`Compacting`、`Cancelled`。

状态转换 SHALL 遵循：

- `Idle` → `Thinking` ：收到 `user_message`
- `Thinking` → `AwaitPerm` ：LLM 返回 tool_use 且无适配权限规则
- `Thinking` → `Executing` ：LLM 返回 tool_use 且权限已允许
- `AwaitPerm` → `Executing` ：收到 `allow_*` 决策
- `AwaitPerm` → `Thinking` ：收到 `deny_*` 决策（带 deny 的 tool_result 回灌）
- `Executing` → `Thinking` ：tool 执行完成回灌 LLM
- 任意 → `Cancelled` ：收到 `interrupt` 或顶层 recover 捕获 panic
- `Cancelled` → `Idle` ：收尾完成（含 panic 恢复路径）；超时上限见下方"取消收尾超时"requirement
- `Thinking` → `Compacting` ：触发压缩
- `Compacting` → `Idle` ：压缩完成

`POST` 在各状态下的接受/拒绝规则与对应响应码（`409 Conflict` + `error` SSE 事件双通道）由 `api-protocol` capability 的 "POST 与会话状态冲突" requirement 定义。

#### Scenario: Idle 状态接收 user_message 进入 Thinking

- **WHEN** 会话处于 `Idle` 状态时收到 `{"type":"user_message", "content":"hi"}`
- **THEN** 会话进入 `Thinking` 状态，开始调用 LLM

#### Scenario: Compacting 期间 POST user_message 被拒

- **WHEN** 会话处于 `Compacting` 状态时客户端 POST `{"type":"user_message"}`
- **THEN** 服务按 api-protocol 规则返回 `409 Conflict` 且 SSE 流 emit `error { code:"session_busy", state:"Compacting" }`；会话保持 `Compacting` 直至压缩完成

### Requirement: Panic 恢复

服务 SHALL 在 session goroutine 顶层（含其 agent loop、工具执行 wrapper、子 agent dispatch）包裹 `recover()`，捕获任何 panic 后：

1. 把 panic 值与 stack trace 写入结构化日志
2. emit `error { code: "internal_panic", message: "<sanitized message>" }` 事件（不暴露 stack trace 给客户端）
3. 对任何"已发出 tool_use 但未收到 tool_result"的工具调用，合成 `tool_result { is_error: true, output: "[CANCELLED] Tool execution was interrupted by user" }` 追加 history（与正常 interrupt 同路径）
4. 状态转为 `Cancelled`，按正常收尾流程进入 `Idle`
5. 会话**可继续使用**——下一次 POST `user_message` 正常进入 `Thinking`

panic SHALL **不**使整个服务进程崩溃；SHALL **不**让其他 session 受影响。

#### Scenario: 工具内部 panic 不影响会话可用性

- **WHEN** Bash 工具内部因解析失败 panic
- **THEN** 服务 recover；emit `error { code: "internal_panic", message: "tool execution failed" }`；为该 Bash 调用合成 cancelled tool_result；状态回 `Idle`；客户端可立即发新 `user_message` 正常进入 `Thinking`

#### Scenario: panic 不影响其他 session

- **WHEN** session A 内部 panic 时 session B 正在 Thinking
- **THEN** session A 走 panic 恢复流程；session B 完全不受影响，继续推理与事件流

### Requirement: 持久化与 ephemeral 模式

服务 SHALL 默认将会话状态持久化到 SQLite（`~/.workhorse-agent/state.db`）；含 `sessions`、`messages`、`events`、`tool_calls`、`permissions` 5 张表。

若创建会话时设 `ephemeral: true`，服务 SHALL 跳过所有持久化（仅在内存中维护）。

#### Scenario: 默认持久化

- **WHEN** 创建非 ephemeral 会话并发送一条 `user_message`，服务进程随后重启
- **THEN** 服务启动时从 SQLite 恢复该会话；GET `/v1/sessions/<id>` 返回该会话记录，含原始 history

#### Scenario: ephemeral 不入库

- **WHEN** 创建 `ephemeral: true` 会话并完成一轮对话，服务进程重启
- **THEN** 服务启动后该会话不存在；GET `/v1/sessions/<id>` 返回 `404`

#### Scenario: ephemeral 会话压缩正常工作

- **WHEN** ephemeral 会话累积 token 达到 0.85 阈值触发压缩
- **THEN** Agent 从内存中读取 history，用 Fast 模型生成 summary，新 history = `[summary] + [recent K]`；压缩结果不写 SQLite，仅内存生效；emit `compaction` 事件正常推送

#### Scenario: ephemeral 进程崩溃数据不可恢复

- **WHEN** ephemeral 会话进行中服务进程崩溃
- **THEN** 进程重启后该会话不可恢复；客户端调 `/v1/sessions/<id>` 返回 `404`；这是 ephemeral 模式的已知契约

### Requirement: 会话隔离

每个会话 SHALL 独立维护 `workdir`、`env map`、`history`、`context.CancelFunc`。

工具调用中的相对路径 SHALL 被 resolve 到会话的 `workdir`；默认拒绝访问 `workdir` 之外的路径（可通过会话配置加入 `allowed_paths` 白名单）。路径校验算法细节见下方"路径越界防护算法"requirement。

#### Scenario: 跨会话 history 不互通

- **WHEN** 会话 A 进行 3 轮对话，会话 B 新建后查询 history
- **THEN** 会话 B 的 history 为空，与 A 完全隔离

#### Scenario: 工具拒绝 workdir 外路径

- **WHEN** 会话 workdir 为 `/home/u/proj`，工具被调用读取 `/etc/passwd`
- **THEN** 工具返回 `tool_result { is_error: true, output: "path outside workdir" }`，不读文件

<!-- 来源：AI #2 复审 (2026-05-24) H-6：路径穿越实现规范缺失。补强为可实现的算法步骤 + symlink 解析 + TOCTOU 处理。 -->

### Requirement: 路径越界防护算法

所有读写文件系统的工具（Read / Write / Edit / Grep / Bash 中含路径的命令构造、MCP 工具中路径类参数）在使用路径前 SHALL 执行以下校验序列：

1. **规范化**：`abs := filepath.Clean(filepath.Join(workdir, userPath))`，把 `.` 和 `..` 段消解
2. **符号链接解析**：`resolved, err := filepath.EvalSymlinks(abs)`；若 `err == os.ErrNotExist` 且工具语义允许（如 Write 创建新文件），SHALL 退而对**父目录**执行 EvalSymlinks 再拼回文件名
3. **白名单成员判定**：用 `filepath.Rel(allowedRoot, resolved)` 判断 `resolved` 是否在 `workdir` 或 `allowed_paths` 任一根下；判定 SHALL 拒绝 Rel 返回值以 `..` 开头的情况
4. **TOCTOU 缓解**：实际打开文件 SHALL 用 `os.OpenFile` 配合 `O_NOFOLLOW`（Linux/macOS）防止"检查后被替换为 symlink"的竞态；不支持 `O_NOFOLLOW` 的平台 SHALL 在操作完成后再 `os.Lstat` 一次验证目标未变（fd-based 复检 inode）
5. **跨设备拒绝**（可选硬化）：`os.Stat` 检查目标设备 ID 与 workdir 设备 ID 一致；不一致时按 `allowed_paths` 显式声明判定

校验失败 SHALL 返回 `tool_result { is_error: true, output: "path outside workdir" }`，不进行任何文件 IO。

#### Scenario: 拒绝符号链接逃逸

- **WHEN** workdir 为 `/home/u/proj`，proj 内含 `linky -> /` 符号链接；工具被调用读取 `linky/etc/passwd`
- **THEN** EvalSymlinks 解析为 `/etc/passwd`，filepath.Rel 返回 `../../etc/passwd`（以 `..` 开头），工具拒绝并返回 error；不读文件

#### Scenario: 拒绝 .. 跨段穿越

- **WHEN** workdir 为 `/home/u/proj`，工具被调用读取 `../../../etc/passwd`
- **THEN** filepath.Clean 消解为 `/etc/passwd`，不在白名单内，工具拒绝并返回 error

#### Scenario: TOCTOU 防护

- **WHEN** 工具校验 `proj/data.txt` 通过后、打开文件前，`proj/data.txt` 被恶意替换为指向 `/etc/passwd` 的 symlink
- **THEN** `os.OpenFile(..., O_NOFOLLOW)` 在 Linux/macOS 上直接失败（`ELOOP`）；工具返回 error 不读到 `/etc/passwd`

<!-- 来源：AI #2 复审 (2026-05-24) H-9：Cancelled→Idle 收尾完成判定模糊。加超时上限 + checklist + 超时降级语义。 -->

### Requirement: 取消收尾超时

`Cancelled → Idle` 的收尾流程 SHALL 在 `agent.cancel_drain_timeout_seconds`（默认 5，可配）内完成，依次执行以下 checklist：

1. ctx 取消已触发（同步）
2. 等待 provider HTTP 请求中止（HTTP client 内部 ctx 传播；通常 < 50ms）
3. 等待所有正在跑的工具 Run 返回（含 Bash 进程组 SIGTERM → SIGKILL 兜底；含 MCP `notifications/cancelled` 转发；含子 session 级联取消）
4. 合成 cancelled tool_result 并追加 history（同步）
5. 持久化最终 history（如非 ephemeral）
6. 持久化 interrupted 标志：通过 `Session.LastAssistantMessageID()` 获取最后一条 `role='assistant'` 消息的 ULID，调用 `store.MarkMessageInterrupted(ctx, messageID)` 将 `interrupted` 列设为 `1`；非 ephemeral 且 store 非 nil 且 messageID 非空时执行
7. emit `interrupted` 事件，payload SHALL 包含 `message_id` 字段
8. 清空 outbox 中属于被中断那一轮的事件（见 api-protocol "中断到达时清空 SSE 积压"）

若整个 checklist 在超时内完成 SHALL 正常转入 `Idle`。

若超时仍未完成 SHALL：

- emit 一条 `error { code: "cancel_timeout", details: { phase: "<当前 step>", elapsed_ms: <N> } }` 事件
- 对未返回的工具/子 session 启动**强制丢弃**：把它们的 ctx 标 done 后不再等待，goroutine 自然在下次 select ctx.Done() 时退出（可能短暂残留）
- 强制转入 `Idle`，会话可继续接受新 user_message
- 日志 `warn` 级别记录 phase + 元数据供排查

#### Scenario: 正常收尾在超时内完成

- **WHEN** session 处于 Executing，Bash 工具跑 `sleep 60` 时被 cancel
- **THEN** ctx 取消 → SIGTERM Bash 进程组 → 1.5s 后 SIGKILL → 合成 cancelled tool_result → 持久化 interrupted 标志 → emit interrupted → 状态转 Idle；整个过程 < 5s；最后一条 assistant 消息的 `interrupted` 列 SHALL 为 `1`

#### Scenario: 中断消息持久化后 rehydration 保留标志

- **WHEN** 某会话在中断后从 history 端点重建（如项目切换后重新打开）
- **THEN** `GET /v1/sessions/{id}/history` 返回的最后一条 assistant 消息 SHALL 包含 `"interrupted": true`
- **AND** 客户端 UI SHALL 显示中断标记（如"（已中断）"）

#### Scenario: 超时强制 Idle

- **WHEN** 某个 MCP 工具 ctx 取消后 5 秒内仍未返回（MCP server 卡死不响应 cancelled notification）
- **THEN** 服务 emit `error { code: "cancel_timeout", details: { phase: "tool_drain", elapsed_ms: 5000 } }`；强制转 Idle；session 可立即接新 user_message；卡死的 goroutine 不阻塞会话使用

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

