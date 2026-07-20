# Implementation Report: Agent 编排能力升级

**Branch**: `001-agent-orchestration` | **Date**: 2026-07-21 | **Status**: 全部 27 任务完成，三绿通过

四项能力（后台只读委派、上下文溢出自愈、进程内 cron 调度、子代理活动上报）均已实现并由自动化测试覆盖。真实 serve + 真实 provider 的端到端手工走查留终审。

## 各 User Story 实现位置

### US1 后台只读委派

| 层 | 位置 | 内容 |
|---|---|---|
| 存储 | `internal/store/sqlite/migrations.go` (v9) + `crud.go` + `types.go` | `delegations` 表、Delegation 类型、CRUD；`ClaimPendingNotifications` 单事务 exactly-once（先置 `notified_at` 再返回）；`ReapRunningDelegations` |
| ID | `internal/delegation/id.go` | 形容词-颜色-动物三段式（crypto/rand，冲突重试 20 次） |
| 管理 | `internal/delegation/manager.go` + `run.go` | `Manager.Start`（校验/并发上限/嵌套拒绝）→ 后台 goroutine 驱动 Ephemeral 只读子会话 → `CompleteDelegation`/`FailDelegation`；`ConsumePending` 实现 `agent.NotificationSource` |
| 注入 | `internal/agent/loop.go` | `NotificationSource` 接口 + `injectNotifications`（在追加用户消息前注入 system 消息） |
| 工具 | `internal/tools/delegation/tools.go` | `delegate`/`delegation_read`/`delegation_list` |
| 接线 | `cmd/workhorse-agent/cmd_serve.go` | 两段式构造（sessMgr 回填）、启动 `ReapRunning`、`LoopConfig.Notifications`（top-level）、`ToolEnv.Delegations` |

### US2 上下文溢出自愈

| 层 | 位置 | 内容 |
|---|---|---|
| 压缩 | `internal/agent/compaction.go` | `CompactWithKeep(ctx, history, keep)`（`Compact` 委托） |
| 自愈 | `internal/agent/loop.go` | `consumeResult`（携带 `fatalErr`/`produced`）、`maybeOverflowRecover`（`keep=max(2,RecentKeep/2)`，单次/未输出条件）、`compactAndSwap`（共享 Thinking→Compacting→Thinking + compaction 事件） |

### US3 进程内定时调度

| 层 | 位置 | 内容 |
|---|---|---|
| 存储 | `migrations.go` (v9) + `crud.go` + `types.go` | `schedules`/`schedule_runs` 表、类型、CRUD；`DeleteSchedule` 同事务级联删 runs；`TouchScheduleRun` 一次性 `enabled=0`；`CreateScheduleRun` 同事务 prune 至 20 |
| cron | `internal/schedule/cron.go` | 自研五字段匹配器（`*`/数字/`,`/`-`/`*/n`）+ `NextMatch`（7 天扫描） |
| 运行 | `internal/schedule/runner.go` + `run.go` | `Runner.RunOnce`：workdir 校验 → 持久会话（`{scheduled: <id>}`）→ `schedule_runs` 记录 → 驱动一回合 → `FinishScheduleRun`（output 尾部 ≤64 KiB） |
| 调度 | `internal/schedule/worker.go` | 分钟 tick（serve ctx 可取消）、`shouldFire`（enabled/cron 匹配/一次性 run_at/同分钟去重/不补跑）、`Touch` 先于 fire、tick recover |
| 工具 | `internal/tools/scheduletool/tools.go` | `schedule_create`/`list`/`remove`/`read_log` |
| 接线 | `cmd_serve.go` | `Runner` + `Worker`（`go Start(ctx)`，shutdown 干净退出）、注册四工具、`ToolEnv.Schedules` |

### US4 子代理活动上报

| 层 | 位置 | 内容 |
|---|---|---|
| 协议 | `internal/api/protocol/protocol.go` | `EventSubagentStatus` + `AllServerEventTypes`（23） |
| 翻译 | `internal/tools/dispatch/activity.go` | `FormatActivity`（≤80 码点单行，多行折叠，CJK 按码点截断） |
| 发布 | `internal/tools/dispatch/pump.go` | `forward` 在 `tool_call_start` 发 `subagent_status`；`emitStatusClear`（streaming 模式退出时 `activity:""`）；`EmitNow` 静默丢弃（FR-022） |

## 偏离设计文档之处及理由

1. **新增 `CountRunningDelegations`**（data-model §1 Store 接口列表遗漏）：固定并发上限（4）需要全库 running 计数，data-model 未列。在 `store.Store` 加了该方法（T004，commit 注明）。
2. **`delegation.Manager.ConsumeNotifications` → `ConsumePending`**：tasks.md T006 写 `ConsumeNotifications`、T007/research.md R3 写 `ConsumePending`。以不得推翻的 R3 为准，方法对齐为 `ConsumePending`，使其直接实现 `agent.NotificationSource`（T007，commit 注明）。
3. **`tools/delegation` 包名 `delegationtool`**：目录按 T008 要求为 `internal/tools/delegation`，但包名 `delegation` 会与 import 的 `internal/delegation` 冲突，故包名用 `delegationtool`（T008，commit 注明）。
4. **cron day-of-month/day-of-week 用 AND**（非 vixie-cron OR）：常见用例（至多一个 restrict，如 `0 9 * * 1-5`）下等价；同时 restrict 时与 vixie 不同。简化、可预测、可测（T014，commit 注明）。
5. **`ChildMetaKey` 导出**：原 `childMetaKey` 私有，但它是嵌套禁止第二道防线的契约性标记，且测试 harness 需据此识别 child 会话，故导出为 `ChildMetaKey`。
6. **持久调度会话不删**（spec R7）：为可回放保留，但带来累积副作用（见风险 1）。
7. **`schedule_create` 的 "Next run"**：用 `NextMatch` 7 天扫描计算；极端 cron（如仅 2 月 29 日）7 天内无匹配时不显示 Next run（不报错）。

## 已知风险与未尽事项

1. **持久调度会话累积**：每次 schedule 触发创建持久会话，运行结束后 loop 退出但会话留在 `manager.sessions` + `sessions` 表，长期累积占内存与 `max_concurrent` 配额。仓库无"移除 live 但保留 store"的 forget 机制（`DeleteSession` 会 purge 导致不可回放）。单用户本地 + schedule 量级小（个位数到几十）可控；**未来需补 forget 机制**。
2. **真实 serve 手工走查未执行**：US2 真实 provider 溢出、US3 cron 跨分钟触发/进程重启恢复、客户端订阅 SSE 等需终审在真实环境验证；本实现用 `mockprovider` 覆盖全部逻辑路径。
3. **权限超时测试用阻塞 ctx 模拟**：`TestUS3_UnattendedPermissionTimesOutDoesNotHang` 的 prompt callback 阻塞到 per-request ctx 超时（模拟无人应答）→ `Deny/SourceTimeout`。真实 `permission_request` SSE → 客户端超时路径未端到端测（依赖 SSE 客户端）。
4. **固定超时**：委派子会话 `childTimeout=10min`、调度运行 `runTimeout=10min` 均为常量（R9 不加配置项）。
5. **错过触发不补跑**：serve 停机跨过触发时间点不补跑（spec Assumptions）；启动时仅 `ReapRunningDelegations` 回收委派，schedule 无"错过补偿"。

## 三绿命令最终输出摘要

```
$ go build ./...
（无输出，exit 0）

$ go test ./...
所有包 ok，0 个 FAIL（核心包另以 -race 通过：agent/delegation/schedule/dispatch/store/sqlite）

$ golangci-lint run --new-from-rev=master ./...   # CI 锁定 v1.62.0
（无输出，exit 0 — 零新增；基线 master 的 10 项既有债不计）
```

Lint 门禁说明：CI 锁定 `golangci-lint v1.62.0`（`.github/workflows/ci.yml`），命令为
`golangci-lint run --new-from-rev=master ./...`。本功能触碰/新建的每个 Go 文件自身零 lint
问题。基线 master 的 10 项既有问题集中在 `internal/extagent/`、
`internal/tools/{sessionsearch,agentsetup}`、`test/e2e`、`test/real_e2e`，与本功能文件
无交集，属已知技术债。
