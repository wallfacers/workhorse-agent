# Data Model: Agent 编排能力升级

**Feature**: 001-agent-orchestration | **Date**: 2026-07-20

存储层：SQLite 迁移 **v9**（追加到 `internal/store/sqlite/migrations.go` 的 `migrationsByVersion`，模式仿 v7/v8：`CREATE TABLE IF NOT EXISTS`，独立事务）。所有时间戳沿用仓库惯例：INTEGER unix 微秒（`toMicros`/`fromMicros`）。Go 类型加在 `internal/store/types.go`，接口方法加在 `internal/store/store.go` 并于 `sqlite/crud.go` 实现。

## 1. delegations（委派记录）

| 列 | 类型 | 约束 | 说明 |
|---|---|---|---|
| id | TEXT | PRIMARY KEY | 人类可读短 ID：`形容词-颜色-动物`（如 `brisk-amber-fox`），冲突时重试生成，20 次后报错 |
| session_id | TEXT | NOT NULL | 发起委派的父会话（通知投递目标） |
| description | TEXT | NOT NULL | 任务一句话描述（列表展示用） |
| prompt | TEXT | NOT NULL | 交给子代理的完整指令 |
| workdir | TEXT | NOT NULL | 子会话工作目录（继承父会话） |
| status | TEXT | NOT NULL | `running` \| `complete` \| `error`；重启时残留的 `running` 一律判定为 `error`（"server restarted"） |
| title | TEXT | | 完成时从结果首行提取（≤48 码点） |
| summary | TEXT | | 完成时从结果压缩提取（≤180 码点，空白折叠） |
| result | TEXT | | 结果全文（成功为最终 assistant 文本；失败为错误详情） |
| error | TEXT | | 失败原因（status=error 时非空） |
| started_at | INTEGER | NOT NULL | 微秒 |
| completed_at | INTEGER | | 微秒；running 时为 NULL |
| notified_at | INTEGER | | 微秒；NULL = 待通知。置位即视为已通知（exactly-once 由"查询 NULL → 立即置位 → 注入"的顺序保证，先置位后注入：宁可丢一次通知也不重复） |

索引：`idx_delegations_session ON delegations(session_id, started_at DESC)`；`idx_delegations_pending ON delegations(session_id) WHERE status != 'running' AND notified_at IS NULL`。

**状态机**: `running → complete`（子会话 end_turn）/ `running → error`（provider 错误、ctx 取消、进程重启回收）。无其他迁移；terminal 状态不可变。

**校验规则**: `description` 非空且 ≤200 码点；`prompt` 非空且 ≤32 KiB；发起方会话不得带 `delegation_child` metadata 标记（嵌套禁止的第二道防线）；活跃（running）委派数按全库计数 ≥4 时拒绝创建。

**Store 接口新增**: `CreateDelegation`、`GetDelegation(id)`、`ListDelegations(sessionID)`、`CompleteDelegation(id, title, summary, result)`、`FailDelegation(id, errMsg, result)`、`ClaimPendingNotifications(sessionID) []*Delegation`（查 NULL + 置位 notified_at，单事务）、`ReapRunningDelegations()`（启动时回收）。

## 2. schedules（定时计划）

| 列 | 类型 | 约束 | 说明 |
|---|---|---|---|
| id | TEXT | PRIMARY KEY | 由 name slug 化生成（小写、连字符），冲突加 `-2` 后缀 |
| name | TEXT | NOT NULL | 人类可读名称 |
| instruction | TEXT | NOT NULL | 触发时交给新会话的完整指令（≤32 KiB） |
| cron | TEXT | | 五字段 cron 表达式（`分 时 日 月 周`）；与 run_at 二选一 |
| run_at | INTEGER | | 一次性触发时刻（微秒）；与 cron 二选一 |
| workdir | TEXT | NOT NULL | 无人值守会话的工作目录（创建时校验存在） |
| enabled | INTEGER | NOT NULL DEFAULT 1 | 一次性计划触发后置 0（FR-019） |
| created_at | INTEGER | NOT NULL | 微秒 |
| last_run_at | INTEGER | | 最近触发时刻（微秒）；同分钟去重依据 |

**校验规则**: `cron XOR run_at`（恰好其一）；cron 五字段各自语法校验（`*`、数字、`,`、`-`、`*/n`，字段值域 分0-59/时0-23/日1-31/月1-12/周0-6）；非法即拒绝创建（FR-013）。

**触发判定**（worker 每分钟 tick）: `enabled=1` 且（cron 匹配当前分钟 或 `run_at` 所在分钟 ≤ 当前分钟且从未运行）且 `last_run_at` 不在当前分钟内（FR-015 同分钟去重）。错过不补跑：cron 只匹配"当前"分钟；一次性计划例外——`run_at` 已过且 `last_run_at IS NULL` 时仍触发一次（避免创建后 1 分钟内的边界丢失），随后 `enabled=0`。

**Store 接口新增**: `CreateSchedule`、`GetSchedule(id)`、`ListSchedules()`、`DeleteSchedule(id)`、`TouchScheduleRun(id, at)`（更新 last_run_at，一次性计划同时置 enabled=0）。

## 3. schedule_runs（计划运行日志）

| 列 | 类型 | 约束 | 说明 |
|---|---|---|---|
| id | INTEGER | PRIMARY KEY AUTOINCREMENT | |
| schedule_id | TEXT | NOT NULL | 所属计划（计划删除时级联删除运行记录） |
| session_id | TEXT | | 本次运行创建的会话 ID（可用 /v1/sessions/{id}/history 回放全程） |
| started_at | INTEGER | NOT NULL | 微秒 |
| completed_at | INTEGER | | 微秒 |
| status | TEXT | NOT NULL | `running` \| `complete` \| `error` |
| output_tail | TEXT | | 最终 assistant 文本（尾部截断 ≤64 KiB） |
| error | TEXT | | 失败原因 |

索引：`idx_schedule_runs_schedule ON schedule_runs(schedule_id, started_at DESC)`。写入后按计划保留最近 20 条，超出删除最旧（与写入同事务）。

**Store 接口新增**: `CreateScheduleRun`、`FinishScheduleRun(id, status, outputTail, errMsg)`、`ListScheduleRuns(scheduleID, limit)`、`PruneScheduleRuns(scheduleID, keep)`。

## 4. SubagentStatus（瞬时状态，不持久化）

无表。生命周期即 dispatch 泵的生命周期，形态见 [contracts/events.md](./contracts/events.md) 的 `subagent_status` 事件 payload。清空语义：`activity: ""`。

## 5. 关系与一致性

- `delegations.session_id → sessions.id`：弱引用（父会话删除不级联删委派记录；通知在会话已删时静默丢弃）。
- `schedule_runs.schedule_id → schedules.id`：强关系，`DeleteSchedule` 同事务删除 runs。
- 委派子会话为 `Ephemeral`，不出现在 sessions 表——`delegations` 是其唯一持久痕迹。
- 调度运行会话为持久会话，metadata 携带 `{"scheduled": "<schedule_id>"}`，与 `schedule_runs.session_id` 双向可查。
- 事件表不新增行类型约束：`subagent_status` 经 `EmitNow` 走既有 `events` 追加路径（持久会话）或内存 idx（ephemeral），与既有 22 种事件同构。
