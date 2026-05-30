## Context

Claude Code 通过 `TaskCreate` / `TaskUpdate` / `TaskList`（及更简的 `TodoWrite`）
让模型把复杂请求拆成可追踪的步骤，并在 spinner / UI 中向用户展示进度。其工具提示词
强调：≥3 步的复杂任务才用、开始前置 `in_progress`、完成后立即置 `completed`、
不要批量补记、单步任务直接做。

workhorse-agent 已有成熟的内置工具骨架可复用：
- `tools.Tool` 接口（Name / Description / InputSchema / Run / IsReadOnly /
  CanRunInParallel / DefaultTimeout），经全局 `ToolRegistry` 注册。
- 会话级 `AllowedTools` 门控（`tool-system` 既有 requirement）。
- SSE 事件经会话 Outbox 广播；events 表 append-only、ULID、idx 即 SSE `id:`。
- 先例：`memory_*`、`session_search` 都是「在自己的 capability 里定义、走既有
  registry 注册」，未改动 tool-system 的核心工具表。

约束：无 CGO（modernc.org/sqlite）；事件 append-only；子 Agent 会话隔离。

## Goals / Non-Goals

**Goals:**
- 给模型一个结构化、受测的任务清单工具，对齐 Claude Code 的使用纪律。
- 任务变更对用户可见（SSE）。
- 会话级隔离，子 Agent 各自独立。
- 在 `DefaultBasePrompt` 加使用引导。

**Non-Goals:**
- 不做跨会话的全局任务看板 / 持久待办（仅会话生命周期内）。
- 不做任务依赖图、blockedBy 等高级编排（可留后续；MVP 只要三态线性流转）。
- 不改 tool-system 的「内置 5 工具」表。

## Decisions

### D1：工具形态 —— 单 `TodoWrite`（整表覆盖） vs 多工具

- **D1a 单工具 `TodoWrite`**：一次传入完整任务数组，整表覆盖。状态机简单、调用次数少、
  不易产生「忘了更新某条」的偏差；缺点是每次需重发全表。**倾向此项**（MVP 最简，
  且 Claude Code 后期也以 TodoWrite 整表覆盖为主流）。
- **D1b 多工具 `TaskCreate`/`TaskUpdate`/`TaskList`**：粒度细、增量更新；但暴露更多
  工具、提示词负担更大、并发更新需考虑顺序。

最终取舍在实现前确认；spec 已写成与形态无关。

### D2：持久化 —— 纯内存 vs 落 events 表

- **D2a 纯内存**：任务清单挂在 `session.Session` 上，随会话结束消失。最简，足够覆盖
  「会话内追踪进度」目标。SSE 广播即时态，不可重放历史任务。
- **D2b 落 events 表**：每次变更 append 一条 event，可重放、可在会话恢复时还原清单。
  与既有 events/idx 机制一致，但实现更重。

倾向 **D2a（纯内存）** 作为 MVP，因为目标是「当前会话的进度可见」，并非长期待办；
若后续需要会话恢复后还原清单，再迭代到 D2b。design 阶段保留两者，apply 前定。

### D3：SSE 事件形态

新增一类事件（暂名 `task_update`），负载为当前任务清单快照（整表）或单任务增量。
若选 D1a（整表覆盖），事件自然携带整表快照，前端直接替换渲染，最简单。需与
`api-protocol` capability 对齐事件名与 schema —— 确认是否纳入本变更或拆出协议子任务。

### D4：注册与门控

按既有模式在 `cmd_serve.go` 的 registry 装配处注册工具实例；`buildProviderToolSchemas`
自动按 `AllowedTools` 过滤，无需特殊处理。子 Agent 会话因独立 `session.Session` 而
天然拥有独立清单（D2a 下尤其直接）。

## Risks / Trade-offs

- **[整表覆盖丢更新] → 缓解**：D1a 下若模型重发表时漏带某条会「删除」它；提示词明确
  「每次传完整清单」，并在工具 Description 强调。
- **[SSE 协议扩张] → 缓解**：优先复用既有事件管道；若新增事件类型，最小化负载并与
  api-protocol 对齐，必要时拆为独立协议子任务避免本变更膨胀。
- **[与编排引导耦合] → 缓解**：base prompt 引导文字与工具行为解耦；引导仅描述「何时用」，
  工具语义由 spec 锁定。
- **[纯内存不可重放] → 接受**：MVP 明确 Non-Goal；后续可平滑升级 D2b。

## Migration Plan

1. 定 D1（工具形态）、D2（持久化）、D3（SSE 是否扩协议）。
2. 实现 `internal/tools/tasklist/`：状态结构 + 工具 Run + 输入 schema。
3. 在 `cmd_serve.go` 注册；确认 `AllowedTools` 过滤生效。
4. 接 SSE 广播（按 D3）。
5. 在 `DefaultBasePrompt` 追加使用引导段。
6. 写单测（状态机、门控、隔离、广播）+ 各 scenario 测试；`go test ./...` 全绿；
   `gofmt`/`golangci-lint` 干净。

回滚：移除工具注册即从工具面消失；无持久化（D2a）则无数据迁移。

## Open Questions

- D1 单工具还是多工具？D2 内存还是 events？D3 是否新增 SSE 事件类型并改 api-protocol？
- 是否需要 `description`/`activeForm` 之外的字段（如 priority、blockedBy）？MVP 倾向不加。
- 子 Agent 的任务进度是否需要冒泡到父会话展示？MVP 倾向否（各自独立）。
