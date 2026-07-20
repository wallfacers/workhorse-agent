# Implementation Plan: Agent 编排能力升级（后台委派、溢出自愈、定时调度、活动上报）

**Branch**: `001-agent-orchestration` | **Date**: 2026-07-20 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/001-agent-orchestration/spec.md`

## Summary

为 workhorse-agent 增加四项编排能力：(1) 后台只读委派子代理——新增 `delegate`/`delegation_read`/`delegation_list` 工具，复用 `session.Manager` 以进程内 goroutine 运行只读工具面的临时子会话，委派记录与结果持久化到 SQLite（迁移 v9），完成通知在下一轮用户消息前经 agent loop 的新注入点写入会话历史（恰好一次）；(2) 上下文溢出自愈——在 `runTurnLoop` 拦截 `CodeContextLengthExceeded`，以放宽的 `RecentKeep`（减半、下限 2）强制执行既有 `Compactor` 压缩后整轮重试一次；(3) 进程内定时调度——仿照 `curation.Worker` 的新 `internal/schedule.Worker` 分钟级 tick，自研五字段 cron 匹配器，`schedule_*` 四个工具管理计划，触发时经 `session.Manager.CreateSession` 无人值守运行并记录运行日志；(4) 子代理活动上报——在 `dispatch/pump.go` 转发泵中把子会话的 `tool_call_start` 翻译为单行活动描述，以新 SSE 事件 `subagent_status` 尽力而为地发布。

## Technical Context

**Language/Version**: Go 1.22+（单二进制，无 CGO）

**Primary Dependencies**: 标准库为主；`modernc.org/sqlite`（既有）；不新增第三方依赖（cron 匹配器自研五字段实现）

**Storage**: SQLite（既有 store，迁移 v8 → v9 新增 `delegations`、`schedules`、`schedule_runs` 三表；`Store` 接口扩展对应方法）

**Testing**: `go test ./...`（既有模式：临时目录 SQLite、fake provider 驱动 loop 测试）；`golangci-lint run` 必须保持干净

**Target Platform**: Linux / macOS 本地常驻 serve 进程（127.0.0.1 绑定）

**Project Type**: 单二进制本地单用户多会话 AI agent 服务器（既有工程，增量特性）

**Performance Goals**: 委派发起 ≤2s 返回；调度触发误差 ≤1 个检查周期（1 分钟）；活动上报不阻塞子代理执行（非阻塞 EmitNow）

**Constraints**: 权限系统对子代理与无人值守运行同样生效（超时即拒绝语义复用）；SSE 协议向后兼容（新增事件类型，旧客户端忽略）；本地内置工具 `Description()` 必须为英文（`TestLocalToolDescriptionsAreEnglish` 门禁）；`panic` 不得出现在 main/init 之外

**Scale/Scope**: 单用户本地服务；委派并发上限固定 4；调度计划量级为个位数到几十条；新增代码集中在 4 个包 + loop/serve 接线

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

`.specify/memory/constitution.md` 为未批准的空模板（无项目 constitution）。以仓库 `CLAUDE.md` 的既有工程约束作为事实 gate 评估：

| Gate（来自 CLAUDE.md） | 评估 | 说明 |
|---|---|---|
| 仅 `modernc.org/sqlite`、无 CGO | PASS | 新表走既有迁移机制 v9，不引入新存储 |
| `panic` 不出 main/init；loop 有顶层 recover | PASS | 委派子会话复用既有 loop（自带 recover）；scheduler worker 内部 recover + WARN |
| 网络姿态（127.0.0.1、token 不入日志） | PASS | 不新增网络面；无新端点 |
| 权限：所有工具走 `Manager.Check`，无人值守不绕过 | PASS | FR-018/FR-023；调度运行沿用超时拒绝语义 |
| 工具经 Registry 注册、受 `allowed_tools`/`Filtered` 过滤 | PASS | 7 个新工具全部走 `registerBuiltinTools` |
| 本地工具 `Description()` 英文 | PASS | 新工具描述以英文撰写（有测试门禁） |
| 事件表 append-only、`idx` 即 SSE `id:` | PASS | 新事件走既有 `Session.Emit`/`EmitNow` 路径 |
| 默认无注释、gofumpt、golangci-lint 干净 | PASS | 编码规范约束，实现阶段遵守 |

**Post-design re-check (Phase 1 完成后)**: 设计未引入违反项；无需 Complexity Tracking 条目。

## Project Structure

### Documentation (this feature)

```text
specs/001-agent-orchestration/
├── plan.md              # 本文件
├── research.md          # Phase 0 输出
├── data-model.md        # Phase 1 输出
├── quickstart.md        # Phase 1 输出
├── contracts/           # Phase 1 输出
│   ├── tools.md         # 7 个新工具的输入/输出契约
│   └── events.md        # 新 SSE 事件契约
└── tasks.md             # Phase 2 输出（/speckit-tasks 生成，非本命令产物）
```

### Source Code (repository root)

```text
internal/
├── agent/
│   ├── loop.go                  # [改] 溢出自愈重试；轮前通知注入点（NotificationSource）
│   ├── compaction.go            # [改] Compactor 支持一次性放宽 RecentKeep 的强制压缩入口
│   └── retry.go                 # [读] providerErrorCodeFor 分类复用，不改语义
├── delegation/                  # [新] 委派管理器：并发上限、生命周期、通知生产
│   ├── manager.go               # Start/Read/List/ConsumeNotifications；goroutine 驱动子会话
│   ├── id.go                    # 人类可读 ID（adjective-color-animal + 唯一性）
│   └── manager_test.go
├── schedule/                    # [新] 调度器：分钟 tick worker + cron 匹配
│   ├── worker.go                # 仿 curation.Worker；ctx 取消停止；同分钟去重
│   ├── cron.go                  # 自研五字段 cron 匹配器（* , - / 数字）
│   ├── runner.go                # 无人值守运行：CreateSession + Inbox/Outbox 驱动 + 日志落库
│   └── *_test.go
├── tools/
│   ├── delegation/              # [新] delegate / delegation_read / delegation_list 工具
│   ├── scheduletool/            # [新] schedule_create / schedule_list / schedule_remove / schedule_read_log
│   └── dispatch/
│       ├── pump.go              # [改] 转发 tool_call_start 时发布 subagent_status
│       └── activity.go          # [新] 工具调用 → 单行人类可读活动描述
├── store/
│   ├── store.go                 # [改] Store 接口新增 delegation/schedule 方法
│   ├── types.go                 # [改] Delegation / Schedule / ScheduleRun 类型
│   └── sqlite/
│       ├── migrations.go        # [改] v9：delegations、schedules、schedule_runs
│       └── crud.go              # [改] 对应 CRUD
├── api/protocol/protocol.go     # [改] EventSubagentStatus 常量 + AllServerEventTypes
├── config/config.go             # [不改] v1 不新增配置键（上限/开关取固定默认）
cmd/workhorse-agent/
└── cmd_serve.go                 # [改] 接线：delegation.Manager、schedule.Worker 启停、注册 7 个新工具

openspec/  # 不涉及；本特性走 speckit 流程，产物在 specs/
```

**Structure Decision**: 沿用仓库既有布局——能力主体各占一个 `internal/` 包（`delegation`、`schedule`），工具薄壳放 `internal/tools/<name>`（与 `sessionsearch`、`dispatch` 同构），持久化集中在 `store`/`store/sqlite`，进程接线只发生在 `cmd_serve.go`。loop 的两处改动（自愈、注入点）以小接口（`NotificationSource`）解耦，避免 agent 包反向依赖 delegation 包。

## Complexity Tracking

> 无 Constitution 违反项，本节为空。
