# Research: Agent 编排能力升级

**Feature**: 001-agent-orchestration | **Date**: 2026-07-20

Technical Context 无遗留 NEEDS CLARIFICATION；本文件记录设计取向的调研结论。事实依据来自对本仓库的两轮代码调研（agent loop / coord / tools；store / SSE / permission / 后台 worker 模式）以及对借鉴对象 `~/project/grok-cli` 的完整源码分析。

## R1. 后台委派的执行载体：进程内子会话 vs 独立子进程

**Decision**: 进程内 goroutine + `session.Manager.CreateSession`（Ephemeral 子会话），委派记录与结果另存 SQLite。

**Rationale**: grok-cli 用 detached 子进程是因为它是"每次调用一个进程"的 CLI；workhorse-agent 是常驻 serve，`Dispatch` 工具已验证"创建子会话 → Inbox 发 prompt → 泵 Outbox 收结果"的全套机制（`internal/tools/dispatch/dispatch.go:114-217`）。后台化只需把"阻塞等 pumpDone"改为"goroutine 中等待 + 完成后写库"。隔离目标（不与前台写冲突）由只读工具面达成，与进程隔离等效且更轻。

**Alternatives considered**: ① 独立子进程（grok-cli 方案）——需要可重入 CLI headless 入口与进程管理，收益仅是崩溃隔离，而 loop 已有顶层 recover，拒绝；② 复用 Dispatch 工具加 `background` 参数——Dispatch 语义是同步返回 tool_result，后台化会使其返回值语义分裂，单独建 `delegate` 工具更清晰。

## R2. 委派结果与通知的持久化

**Decision**: 新表 `delegations`（迁移 v9），结果全文入库（TEXT），`notified_at` 列保证恰好一次通知；子会话本身 `Ephemeral: true` 不落 sessions 表。

**Rationale**: 仓库规范是 SQLite 单库（CLAUDE.md「Persistence」）；文件落盘（grok-cli 方案）会引入第二套存储位置。结果是最终文本（子代理 end_turn 的 assistant text，`collector.FinalText()` 模式），量级可控；中间过程无需回放，故子会话不必持久化。`notified_at IS NULL` 查询 + 更新在单进程内即可保证 exactly-once。

**Alternatives considered**: 结果写 `~/.workhorse-agent/delegations/*.md` 文件（grok-cli 同款）——违背单库原则、需另做清理与路径安全，拒绝；子会话持久化保留完整 transcript——膨胀 sessions/events 表且无消费方，拒绝。

## R3. 通知注入点

**Decision**: `agent.LoopConfig` 新增小接口 `NotificationSource`（`ConsumePending(ctx, sessionID) []Notification`），loop 在 `runTurnSafe` 追加用户消息前（`loop.go:253` 附近）逐条 `AppendMessage` 注入 system 角色消息并持久化；delegation.Manager 实现该接口，serve 接线。

**Rationale**: 调研确认这是既有代码里唯一"下一轮开始前"的天然接缝；用接口解耦避免 `internal/agent` 依赖 `internal/delegation`（与 `Compactor`、`MemoryIngestor` 在 LoopConfig 上的挂载方式同构）。grok-cli 的 `consumeBackgroundNotifications` 验证了"通知作为 system 消息进历史"对模型是自然可消费的。

**Alternatives considered**: SSE 层注入（只通知客户端不进历史）——模型看不到，无法驱动后续动作，不满足 FR-005；Inbox 伪造用户消息——污染用户消息语义与 title 生成，拒绝。

## R4. 只读工具面与嵌套禁止

**Decision**: 委派子会话 `AllowedTools` 白名单固定为既有只读工具集：`Read, Grep, session_search, LoadMemory, memory_read, MemorySearch, ToolSearch`；`delegate` 等新工具不在名单内即天然禁止嵌套；再加一道防御：delegation.Manager 拒绝来自委派子会话（以 session metadata 标记）的 Start 调用。

**Rationale**: `Registry.Filtered` 白名单机制已存在且经过测试（`registry.go:112`）；白名单（而非黑名单）在未来新增写工具时默认安全。双重防护成本一行判断。

**Alternatives considered**: 仅靠 DenyTools 黑名单——新增工具默认可见，不安全，拒绝；靠 `Depth` 上限禁嵌套——语义间接且影响正常 Dispatch 嵌套，拒绝。

## R5. 溢出自愈的触发与参数

**Decision**: 在 `runTurnLoop` 捕获 `provider.CodeContextLengthExceeded`（经 `AsProviderError` 判定），条件为本迭代尚未向客户端发出任何 assistant/tool 输出且本轮未曾自愈过；随后调用 Compactor 的强制压缩入口（`RecentKeep` 取 `max(2, RecentKeep/2)`），成功后 `continue` 重试本轮；再次失败或无可压缩空间则走既有 `emitProviderError` 路径。

**Rationale**: 调研确认现状是 `context_length_exceeded → recoverable=false → 终止`（`retry.go:111`、`loop.go:505`），自愈是纯增量；既有 `runCompaction` 已处理状态机（Thinking→Compacting→Thinking）与 `compaction` 事件发射，满足 FR-012 的可观察性，复用即可。"减半有下限"直接移植 grok-cli `relaxCompactionSettings` 的验证过的策略，适配为消息数维度（本仓库 Compactor 以 `RecentKeep` 条数为参数）。

**Alternatives considered**: 把 `context_length_exceeded` 改为 retryable 交给 `streamWithRetry`——该层无法执行压缩这种带状态副作用的恢复动作，且会与退避逻辑纠缠，拒绝；预防性精确 token 计数——各 provider 计数不可得，`EstimateTokens` 启发式已存在，自愈兜底比精确预防更稳，拒绝。

## R6. 调度器形态与 cron 解析

**Decision**: 新 `internal/schedule.Worker`，仿 `curation.Worker` 的 `for/select + ticker` 模式，分钟对齐 tick；cron 匹配器自研（五字段：分 时 日 月 周；支持 `*`、数字、`,` 列表、`-` 区间、`*/n` 步进；本地时区）；同分钟去重靠 `last_run_at` 与当前分钟比较；不设 leader lease。

**Rationale**: 仓库已有成熟的进程内后台 worker 先例（`curation/worker.go:117`）；cron 匹配是 ~80 行纯函数（grok-cli 的 `cronMatchesDate` 即此规模），引入 `robfig/cron` 违背仓库"最小依赖"取向且带来 API 面负担。单 serve 进程由端口绑定天然单例，跨进程 lease（curation 需要，因为 CLI 命令也可触发 curation）对调度器是多余的。错过不补跑：与 spec Assumptions 一致，避免重启风暴。

**Alternatives considered**: `robfig/cron/v3` 依赖——功能超需求（秒级、时区表达式、job wrapper），拒绝；复用 curation 的 lease——无多进程场景，拒绝；`time.AfterFunc` 逐计划定时——需维护动态 timer 集合，分钟 tick 扫表更简单且误差满足 SC-004。

## R7. 无人值守运行的会话形态与日志

**Decision**: 触发时经 `session.Manager.CreateSession` 创建**持久**会话（非 ephemeral，`metadata` 标记 `scheduled: <schedule_id>`），按 dispatch/pump 模式驱动 Inbox/Outbox 至 turn end；运行摘要（起止时间、状态、最终文本尾部 ≤64KiB）写入 `schedule_runs` 表；`schedule_read_log` 从该表读最近 N 条。

**Rationale**: 持久会话让完整 transcript 可用既有 `/v1/sessions/{id}/history` 与 `session_search` 回查（免费获得可观察性）；`schedule_runs` 只存索引级摘要避免重复存储。权限语义零改动：无人值守下 `permission_request` 无人应答 → 既有超时拒绝（`promptAndPersist` → `SourceTimeout`），调研已确认该路径不会挂起。

**Alternatives considered**: 每计划一个日志文件（grok-cli 方案）——第二套存储位置 + 轮转清理负担，拒绝；ephemeral 会话 + 全文入库——丢失 transcript 回放能力，拒绝。

## R8. 活动上报的挂载点与事件

**Decision**: 在 `dispatch/pump.go` 的 `forward` 路径上，当子事件为 `tool_call_start` 时，经新 `activity.go` 将 `(tool, input)` 翻译为 ≤80 字符单行描述，以 `parent.EmitNow` 发布新 SSE 事件 `subagent_status`（payload：`agent_id, agent_type, description, activity`）；pump 结束（isTurnEnd/ctx 取消）时发布 `activity: ""` 的清空事件。协议侧在 `protocol.go` 增加 `EventSubagentStatus` 并入 `AllServerEventTypes`。

**Rationale**: 泵已逐事件观察子会话 Outbox（`pump.go:103`），零新增数据通道；`EmitNow` 非阻塞、满则丢弃，天然满足 FR-022 尽力而为。保留既有 `subagent_event`（全量转发）不动，`subagent_status` 是面向 UI 的语义化摘要，二者互补。活动翻译表移植 grok-cli `formatSubagentActivity` 的映射思路，按本仓库工具名（Read/Grep/Bash/…）重写。

**Alternatives considered**: 让客户端自行从 `subagent_event` 里解析 tool_call_start——把翻译逻辑推给每个客户端且暴露原始参数（可能含敏感路径全文），拒绝；在 Orchestrator 层挂 observer——影响所有会话（含顶层），范围过大，拒绝。

## R9. 配置面

**Decision**: v1 不新增任何 config 键。固定默认：委派并发上限 4、委派结果不清理、调度 tick 60s、schedule_runs 每计划保留最近 20 条、活动行上限 80 字符。

**Rationale**: spec Assumptions 已声明上限不做配置；`config` 的每个新键都要进 validate + reload 分类（调研确认新键默认 restart-only 也需显式列入 `nonReloadableFields` 维护面）。先以常量落地，出现真实调参需求再提升为配置。

**Alternatives considered**: 新增 `scheduler.enabled` 开关——无计划时 worker 每分钟一次空表扫描成本可忽略，开关多余，拒绝。

## R10. 测试策略

**Decision**: 沿用既有模式——临时目录 SQLite + fake provider 驱动 loop 级测试（溢出自愈：fake provider 首次返回 `CodeContextLengthExceeded`、压缩后成功）；cron 匹配器纯函数表驱动测试；委派 exactly-once 通知用两轮 turn 断言；`subagent_status` 用既有 dispatch 测试基建断言事件序列；7 个新工具纳入 `TestLocalToolDescriptionsAreEnglish` 覆盖（自动，注册即被扫）。

**Rationale**: 调研确认 `internal/agent/loop_test.go`、`dispatch` 测试与 sqlite 测试基建齐全，无需新测试框架。
