## Why

workhorse-agent 定位多任务 / 多 Agent 编排，但当前没有等价于 Claude Code
`TaskCreate` / `TodoWrite` 的内置任务清单工具。模型无法把一个复杂请求显式拆成
可追踪的步骤、无法标记进度，导致：用户看不到「整体进展到第几步」，模型自身也缺少
一个结构化的「待办—进行中—已完成」状态来约束自己不漏步、不在中途丢失上下文。
对一个鼓励分阶段（Research→Synthesis→Implementation→Verification）编排的系统，
这是显著缺口。

## What Changes

- 新增内置 **task-list 工具**（todo 清单），让模型在会话内创建、更新、列出任务，
  任务带 `pending` / `in_progress` / `completed` 三态。
- 工具经**既有 ToolRegistry 注册**，受会话 `AllowedTools` 约束（与 `memory_*`、
  `session_search` 等既有内置工具同一注册/门控路径，不改动 `tool-system` 的
  「核心 5 工具」表 requirement）。
- 任务清单为**会话级**状态；其更新通过 SSE 向前端广播，使用户可见整体进度。
- 在编排者默认提示词（`DefaultBasePrompt`）中追加引导：≥3 步的复杂任务先建任务清单、
  开始某步前置 `in_progress`、完成后及时置 `completed`，避免批量补记。

## Capabilities

### New Capabilities

- `task-list`：会话级任务清单内置工具。覆盖工具形态（单 `TodoWrite` 整表覆盖
  vs. `TaskCreate`/`TaskUpdate`/`TaskList` 多工具，由 design 定夺）、任务字段与
  三态状态机、经既有 registry 注册并受 `AllowedTools` 门控、清单作用域与生命周期、
  状态变更的 SSE 广播、以及编排者提示词中的使用引导。

### Modified Capabilities
<!-- 无：复用 tool-system 既有注册与门控机制，与 memory/session_search 加入时的先例一致，
     不修改 tool-system 的「内置 5 工具」与「工具注册表」requirement。 -->

## Impact

- **新增代码**：`internal/tools/tasklist/`（工具实现 + 状态结构）；在 `cmd_serve.go`
  的 registry 装配处注册；`DefaultBasePrompt`（`internal/prompt/builtins.go`）追加引导段。
- **持久化**：design 决定任务状态是纯内存（随会话生命周期）还是落 events 表
  （append-only，可重放）。涉及 `store` / events idx 时需评估。
- **协议**：可能新增一类 SSE 事件（如 `task_update`）向前端广播任务变更；触及
  `api-protocol` / 前端 capabilities（需在 design 中确认是否纳入本变更）。
- **依赖**：无新增 go.mod 依赖（沿用 modernc.org/sqlite 若需持久化）。
- **测试**：工具行为单测、状态机转换、`AllowedTools` 门控、SSE 广播；新能力 spec 的
  各 scenario 对应测试用例。
- **风险**：低-中。新增工具不影响既有工具；主要复杂度在持久化选型与 SSE 协议是否扩展。
