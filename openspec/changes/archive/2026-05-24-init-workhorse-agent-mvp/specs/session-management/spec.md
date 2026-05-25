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
6. emit `interrupted` 事件
7. 清空 outbox 中属于被中断那一轮的事件（见 api-protocol "中断到达时清空 SSE 积压"）

若整个 checklist 在超时内完成 SHALL 正常转入 `Idle`。

若超时仍未完成 SHALL：

- emit 一条 `error { code: "cancel_timeout", details: { phase: "<当前 step>", elapsed_ms: <N> } }` 事件
- 对未返回的工具/子 session 启动**强制丢弃**：把它们的 ctx 标 done 后不再等待，goroutine 自然在下次 select ctx.Done() 时退出（可能短暂残留）
- 强制转入 `Idle`，会话可继续接受新 user_message
- 日志 `warn` 级别记录 phase + 元数据供排查

#### Scenario: 正常收尾在超时内完成

- **WHEN** session 处于 Executing，Bash 工具跑 `sleep 60` 时被 cancel
- **THEN** ctx 取消 → SIGTERM Bash 进程组 → 1.5s 后 SIGKILL → 合成 cancelled tool_result → emit interrupted → 状态转 Idle；整个过程 < 5s

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
