# Tasks: Agent 编排能力升级（后台委派、溢出自愈、定时调度、活动上报）

**Input**: Design documents from `/specs/001-agent-orchestration/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/tools.md, contracts/events.md, quickstart.md

**Tests**: 包含测试任务（仓库工程规范要求：`go test ./...` 与 `golangci-lint run` 必须保持通过；spec 的验收场景均可测试化）。

**Organization**: 按 user story 分组，每个 story 可独立实现、独立测试、独立评审。

## Format: `[ID] [P?] [Story] Description`

- **[P]**: 可并行（不同文件、无未完成依赖）
- **[Story]**: 所属 user story（US1-US4）
- 每个任务自包含：明确文件路径与验收点

## 执行约定（对所有任务生效）

- 工作分支：`001-agent-orchestration`（已存在，勿新建）。
- 每完成一个任务：立即把本文件中对应复选框改为 `[x]`，并做一次独立 commit（`feat(...)`/`test(...)` 风格，消息说清做了什么）。
- 仓库硬约束（违反即返工）：默认不写代码注释（仅在"为什么"会让未来读者意外时写）；`gofumpt` 格式；`panic` 不出 main/init；本地工具 `Description()` 必须全英文；SQLite 仅 `modernc.org/sqlite`。
- 时间戳一律 unix 微秒（`toMicros`/`fromMicros`）；ID 用 `idgen.NewULID()`（除非任务另有说明）。
- 契约以 `contracts/` 与 `data-model.md` 为准；与本文件冲突时以契约文档为准并在 commit message 里注明。
- **Lint 门禁（重要）**：必须用 CI 锁定的版本 **golangci-lint v1.62.0**（`.github/workflows/ci.yml` 所固定；勿用系统里的其他版本，v1.64.x 会因默认规则更严而产生额外噪音）。基线 master 有 **10 项既有 lint 问题**（集中在 `internal/extagent/`、`internal/tools/{sessionsearch,agentsetup}`、`test/e2e`、`test/real_e2e` —— 均与本功能文件无交集，属已知技术债，**不归本功能修**）。因此本功能的 lint 门禁是"**不新增**"而非"全仓零问题"：判定命令为 `golangci-lint run --new-from-rev=master ./...`（基线上为 exit 0）。此外，本功能触碰/新建的每个 Go 文件必须自身零 lint 问题。安装 CI 版本：`go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.62.0`（装到 `$(go env GOPATH)/bin`，确保在 PATH）。

---

## Phase 1: Setup

**Purpose**: 基线确认

- [x] T001 在分支 `001-agent-orchestration` 上确认基线可用：`go build ./...` 与 `go test ./...` 通过；lint 用 CI 锁定的 v1.62.0 跑 `golangci-lint run --new-from-rev=master ./...`，结果必须为 **exit 0（零新增）**——注意全仓 `golangci-lint run ./...` 会报 10 项既有债（见执行约定，非本功能引入，可忽略）。三者达标即勾选并继续；若 `go build`/`go test` 失败或增量 lint 非零，才停止上报

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: 存储 schema 与共享类型——US1 与 US3 的共同前置

- [x] T002 在 `internal/store/sqlite/migrations.go` 追加迁移 v9：按 `data-model.md` 建 `delegations`、`schedules`、`schedule_runs` 三表及全部索引（含 `idx_delegations_pending` 部分索引），提供 Down 语句；仿 v7/v8 的 `CREATE TABLE IF NOT EXISTS` + 单事务模式。验收：新库自动迁到 v9；既有迁移测试模式下 v9 up/down 均通过
- [x] T003 在 `internal/store/types.go` 新增 `Delegation`、`Schedule`、`ScheduleRun` 结构体（字段与 `data-model.md` 表列一一对应，含 status 常量：`DelegationRunning/Complete/Error`、`ScheduleRunRunning/Complete/Error`）。验收：`go build ./...` 通过

**Checkpoint**: 迁移与类型就绪，US1-US4 可并行开工（US2/US4 不依赖本阶段，也可先行）

---

## Phase 3: User Story 1 - 后台只读委派子代理 (Priority: P1) 🎯 MVP

**Goal**: 主 agent 经 `delegate` 发起后台只读调研，不阻塞当前轮；完成通知在下一轮用户消息前恰好注入一次；`delegation_read`/`delegation_list` 可检索。

**Independent Test**: quickstart.md「US1 后台委派」全部 5 个观察点。

- [x] T004 [US1] 在 `internal/store/store.go` 扩展 Store 接口并在 `internal/store/sqlite/crud.go` 实现：`CreateDelegation`、`GetDelegation`、`ListDelegations(sessionID)`、`CompleteDelegation`、`FailDelegation`、`ClaimPendingNotifications(sessionID)`（单事务：SELECT `status!='running' AND notified_at IS NULL` → 先 UPDATE notified_at 再返回，宁丢勿重）、`ReapRunningDelegations`（把所有 running 置为 error="server restarted"）；同步修复实现该接口的全部测试桩。验收：crud 层测试覆盖 claim 的 exactly-once（两次调用第二次返回空）与 reap
- [x] T005 [P] [US1] 新建 `internal/delegation/id.go`：`形容词-颜色-动物` 三段式 ID 生成器（各 10 词表，crypto/rand 选词），`GenerateUnique(exists func(string) bool)` 冲突重试 20 次后返回错误；`internal/delegation/id_test.go` 表驱动测试。验收：格式、唯一性重试、耗尽报错
- [x] T006 [US1] 新建 `internal/delegation/manager.go`：`Manager{Store, SessMgr, Log}`。`Start(ctx, parentSessionID, workdir, description, prompt)`：校验（description ≤200 码点、prompt ≤32 KiB、父会话 metadata 无 `delegation_child` 标记、全库 running 计数 <4），写 `delegations` 行，spawn goroutine——用 `session.Manager.CreateSession` 建 `Ephemeral` 子会话（`AllowedTools: [Read, Grep, session_search, LoadMemory, memory_read, MemorySearch, ToolSearch]`，metadata `delegation_child: <id>`，workdir 继承父会话），仿 `internal/tools/dispatch/dispatch.go:167-217` + `pump.go` 的模式发 prompt 并收集最终 assistant 文本至 turn end，成功调 `CompleteDelegation`（title=结果首行 ≤48 码点，summary=空白折叠 ≤180 码点），失败/取消调 `FailDelegation`，defer 删除子会话；goroutine 顶层 recover → FailDelegation + WARN。另有 `Read(id)`、`List(sessionID)`、`ConsumeNotifications(ctx, sessionID) []string`（调 Claim 并渲染通知文本：ID、标题、摘要、`delegation_read` 提示）。验收：`internal/delegation/manager_test.go` 用 fake RunnerFactory 覆盖成功/失败/并发上限/嵌套拒绝
- [x] T007 [US1] 在 `internal/agent/loop.go`：`LoopConfig` 新增 `Notifications NotificationSource` 接口字段（`ConsumePending(ctx, sessionID) []string`，接口定义放 agent 包避免反向依赖）；`runTurnSafe` 在追加用户消息（现 loop.go:253 附近的 `AppendMessage`）**之前**逐条以 `RoleSystem` 消息 AppendMessage 注入；nil 安全、错误仅 WARN 不阻塞本轮。验收：`internal/agent/` 下新增 loop 级测试——完成的委派在下一轮注入恰好一次、再下一轮不重复（对应 spec US1 场景 2）
- [x] T008 [P] [US1] 新建 `internal/tools/delegation/`：三个工具 `delegate`（IsReadOnly=false、CanRunInParallel=true）、`delegation_read`、`delegation_list`（均 IsReadOnly=true、CanRunInParallel=true），输入 schema、输出文案、错误语义严格按 `contracts/tools.md`；Description 全英文；经 `Env` 类型断言获取 Manager（仿 `Env.TaskList` 的 any 挂载模式，新增 `Env.Delegations any`）。验收：工具单测覆盖三工具的成功与错误输出格式
- [x] T009 [US1] 在 `cmd/workhorse-agent/cmd_serve.go` 接线：构造 `delegation.Manager`（在 `sessMgr` 创建后回填，仿 `dispatchHost.Manager` 两段式），启动时调 `ReapRunningDelegations`，`registerBuiltinTools` 注册三工具，`newRunnerFactory` 里把 Manager 挂到 `LoopConfig.Notifications` 与 `ToolEnv.Delegations`。验收：`go build ./...`；启动冒烟——serve 起动后三工具出现在注册表，`TestLocalToolDescriptionsAreEnglish` 通过
- [x] T010 [US1] US1 集成测试：在 `internal/delegation/` 或 `internal/agent/` 下，以真实 sqlite（临时目录）+ fake provider 走通「delegate → 后台完成 → 下轮通知 → delegation_read 取全文」全链路；断言子会话工具面不含 Write/Edit/Bash/Dispatch/delegate（spec US1 场景 5/6）。验收：`go test ./...` 全绿

**Checkpoint**: US1 可独立交付（MVP）——quickstart「US1」手工路径可全部走通

---

## Phase 4: User Story 2 - 上下文溢出自愈 (Priority: P2)

**Goal**: `context_length_exceeded` 不再直接报错：放宽压缩 + 整轮重试一次；已有输出或再次失败则如实报错。

**Independent Test**: quickstart.md「US2 溢出自愈」；自动化为主。

- [x] T011 [US2] 在 `internal/agent/compaction.go` 为 `Compactor` 增加带自定义 keep 的强制压缩入口（如 `CompactWithKeep(ctx, history, keep)`，现有 `Compact` 委托之）；无可压缩空间（`len(history) <= keep+1`）时返回明确的哨兵值/无变化标记。验收：compaction 单测覆盖 keep 覆写与无空间路径
- [x] T012 [US2] 在 `internal/agent/loop.go` 的 `runTurnLoop`：`streamWithRetry` 返回错误处（现 loop.go:505 附近）以 `provider.AsProviderError` 判定 `CodeContextLengthExceeded`；当「本迭代未向客户端发出任何 assistant/tool 输出」且「本轮未自愈过」时，置 `overflowRecovered` 标记，调 `runCompaction` 变体以 `keep = max(2, CompactRecentKeep/2)` 强制压缩（复用既有 Thinking→Compacting→Thinking 状态机与 `compaction` 事件发射），成功后 `continue` 重试本轮；压缩无空间或再次溢出 → 走既有 `emitProviderError`。中途流事件里的同类错误（loop.go:606 路径）同规则处理。验收：不改 `providerErrorCodeFor` 语义
- [x] T013 [US2] `internal/agent/loop_test.go` 新增 fake provider 场景：①首次溢出→压缩事件→重试成功，客户端无 error 事件（spec US2 场景 1/4）；②压缩后再溢出→恰好一次压缩尝试+既有 error 事件（场景 2）；③已发出部分 text delta 后溢出→不重试直接 error（场景 3）；④历史过短无可压缩→不重试直接 error（edge case）。验收：四场景全绿

**Checkpoint**: US2 独立交付——长会话撞墙自动恢复

---

## Phase 5: User Story 3 - 进程内定时调度 (Priority: P3)

**Goal**: LLM 经 `schedule_*` 工具自助排班；serve 进程分钟级 tick 触发无人值守持久会话；同分钟去重；运行日志可查。

**Independent Test**: quickstart.md「US3 定时调度」全部 6 个观察点。

- [x] T014 [P] [US3] 新建 `internal/schedule/cron.go`：五字段 cron 解析与匹配（`*`、数字、`,`、`-`、`*/n`；字段值域 分0-59/时0-23/日1-31/月1-12/周0-6；本地时区）；`Validate(expr) error` 与 `Matches(expr, t time.Time) bool`；`internal/schedule/cron_test.go` 表驱动（合法/非法表达式、边界匹配、`*/n` 步进、周日 0）。验收：测试全绿，无第三方依赖
- [x] T015 [US3] 在 `internal/store/store.go` + `internal/store/sqlite/crud.go` 实现 schedule 方法：`CreateSchedule`、`GetSchedule`、`ListSchedules`、`DeleteSchedule`（同事务级联删 schedule_runs）、`TouchScheduleRun`（更新 last_run_at；一次性计划同时 enabled=0）、`CreateScheduleRun`、`FinishScheduleRun`、`ListScheduleRuns(id, limit)`、`PruneScheduleRuns(id, keep=20)`；同步修复接口测试桩。验收：crud 测试覆盖级联删除与 prune
- [x] T016 [US3] 新建 `internal/schedule/runner.go`：`RunOnce(ctx, sched)` —— `session.Manager.CreateSession` 建**持久**会话（metadata `{"scheduled": "<id>"}`，workdir 校验存在，不存在则直接 FinishScheduleRun(error) 且计划保留），写 `schedule_runs(running)`，Inbox 发 instruction、泵 Outbox 收集最终文本至 turn end（仿 dispatch/pump 的 isTurnEnd 判定），FinishScheduleRun（output_tail 取尾部 ≤64 KiB）+ Prune。验收：runner 测试用 fake RunnerFactory 覆盖成功/失败/workdir 缺失
- [x] T017 [US3] 新建 `internal/schedule/worker.go`：仿 `internal/memory/curation/worker.go` 的 `Start(ctx)` + `for/select` 模式，分钟对齐 `time.Ticker`；每 tick 扫 `ListSchedules`，触发判定按 `data-model.md`（enabled、cron 匹配当前分钟或一次性 `run_at` 已到且从未运行、`last_run_at` 不在当前分钟），命中先 `TouchScheduleRun` 再 goroutine 跑 `RunOnce`；tick 内 recover + WARN；ctx 取消即停。验收：`worker_test.go` 用可注入时钟覆盖同分钟去重（spec SC-004）、一次性失效（FR-019）、错过不补跑、删除后不触发
- [x] T018 [P] [US3] 新建 `internal/tools/scheduletool/`：四工具 `schedule_create`/`schedule_list`/`schedule_remove`/`schedule_read_log`，schema 与输出严格按 `contracts/tools.md`（含 cron XOR run_at 校验、run_at 用 RFC 3339 解析、workdir 默认取 `Env.Workdir`）；Description 全英文；经 `Env.Schedules any` 断言取依赖。验收：工具单测覆盖校验错误文案
- [x] T019 [US3] 在 `cmd/workhorse-agent/cmd_serve.go` 接线：构造 store 支撑的 schedule 组件，`schedule.Worker` 用**可取消 ctx** 启动并在 shutdown 路径显式停止（注意：不要复制 curation 的 background-ctx 松散接线），注册四工具，`ToolEnv.Schedules` 挂载。验收：serve 启停无 goroutine 泄漏（测试用 `Start`+cancel 断言退出）
- [ ] T020 [US3] US3 集成测试：真实 sqlite + fake provider + 注入时钟，走通「schedule_create（一次性）→ tick 触发 → 持久会话运行 → schedule_runs 落库 → schedule_read_log 可读 → enabled=0」（spec US3 场景 1-4）；权限场景断言无人值守下权限提示走超时拒绝且运行不挂起（场景 7，可用极短 timeout 配置）。验收：`go test ./...` 全绿

**Checkpoint**: US3 独立交付——quickstart「US3」手工路径可走通（含重启持久化）

---

## Phase 6: User Story 4 - 子代理活动实时上报 (Priority: P4)

**Goal**: 子代理每次工具调用产生一条人类可读 `subagent_status` SSE 事件；结束时清空；尽力而为。

**Independent Test**: quickstart.md「US4 活动上报」。

- [ ] T021 [P] [US4] 在 `internal/api/protocol/protocol.go` 新增 `EventSubagentStatus ServerEventType = "subagent_status"` 并加入 `AllServerEventTypes`。验收：protocol 既有测试通过
- [ ] T022 [P] [US4] 新建 `internal/tools/dispatch/activity.go`：`FormatActivity(toolName string, input json.RawMessage) string` —— 按 `contracts/events.md` 的翻译表（Read/Grep/Bash/session_search/MemorySearch/其他），输出单行、≤80 码点、超长 `…` 截断、多行折叠；`activity_test.go` 表驱动（含 CJK 截断按码点不按字节）。验收：测试全绿
- [ ] T023 [US4] 在 `internal/tools/dispatch/pump.go` 的 forward 路径：子事件为 `tool_call_start` 时解析 tool/input，`parent.EmitNow("subagent_status", payload{agent_id, agent_type, description, activity})`；泵结束（isTurnEnd/ctx 取消/错误）时 EmitNow 一条 `activity: ""` 清空事件；EmitNow 返回 false 时静默丢弃（FR-022）。`agent_type`/`description` 从 Dispatch 调用参数传入 pump（扩展现有 pump 参数）。验收：dispatch 既有测试不回归
- [ ] T024 [US4] dispatch 测试新增：子代理跑一次含 2+ 工具调用的任务，断言父会话 SSE 序列——每个 `tool_call_start` 对应一条 `subagent_status`（activity 非空单行）、结束恰好一条清空事件、既有 `subagent_event` 全量转发不受影响（spec US4 场景 1-3）。验收：`go test ./internal/tools/dispatch/...` 全绿

**Checkpoint**: US4 独立交付——全部四个 story 完成

---

## Phase 7: Polish & Cross-Cutting

- [ ] T025 全量回归：`go build ./...` 与 `go test ./...` 全绿；lint 用 v1.62.0 跑 `golangci-lint run --new-from-rev=master ./...` 为 exit 0（本功能零新增，基线 10 项既有债不计）；确认 `TestLocalToolDescriptionsAreEnglish` 覆盖全部 7 个新工具；确认旧客户端兼容（不认识 `subagent_status` 的既有 UI/测试不回归，SC-006）
- [ ] T026 [P] 按仓库惯例更新 `CLAUDE.md`：新增「Delegation & Scheduling」小节，简述后台委派（只读面、通知注入、上限 4）、溢出自愈（减半有下限、单次）、调度器（分钟 tick、同分钟去重、不补跑、持久会话）、`subagent_status` 事件；风格对齐既有小节
- [ ] T027 按 `specs/001-agent-orchestration/quickstart.md` 手工走查四个 user story 的验收观察点，记录结果到本文件末尾「Manual Verification Log」小节（新建）；发现偏差先修复再收口

---

## Dependencies & Execution Order

```text
T001 (Setup)
  └─→ T002 → T003 (Foundational)
        ├─→ US1: T004 → T006 → T007 → T009 → T010；T005、T008 可与 T004 并行
        ├─→ US3: T015 → T016 → T017 → T019 → T020；T014、T018 可与 T015 并行
        ├─→ US2: T011 → T012 → T013（仅依赖 T001，可与 Foundational 并行开工）
        └─→ US4: T021、T022 并行 → T023 → T024（仅依赖 T001）
                    全部 → T025 → T026/T027 (Polish)
```

- **US2 与 US4 不依赖 v9 迁移**，可在 Foundational 期间并行。
- US1 与 US3 都改 `store.go`/`crud.go`/`cmd_serve.go`：若并行执行需注意合并冲突，建议串行（US1 → US3）或由同一执行者处理这三个文件。
- 同一文件的任务（如 T004/T015 之于 crud.go）不得并行。

## Implementation Strategy

- **MVP = Phase 1 + 2 + US1**（T001-T010）：交付后即可独立评审与演示。
- 建议交付节奏：US1 → US2 → US3 → US4 → Polish，每个 checkpoint 提交一次可评审的增量。
- 每个 story 完成时其对应 quickstart 小节必须可走通，不得跨 story 留尾巴。

## Manual Verification Log

（T027 执行时填写）
