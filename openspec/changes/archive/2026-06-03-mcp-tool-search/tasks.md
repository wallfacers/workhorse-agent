## 1. 接口与脚手架

- [x] 1.1 在 `internal/tools/tool.go` 新增 `Deferrable` 接口（`ShouldDefer() bool`）与文档注释
- [x] 1.2 在 `internal/tools/tool.go` 新增 `Env.ToolCatalog any` 字段；定义 `ToolCatalog` 接口与 `ToolInfo{Name, Description string; InputSchema json.RawMessage}`
- [x] 1.3 扩展 `ModifierTarget` 接口新增 `MarkToolsDiscovered(names []string)`
- [x] 1.4 新增包骨架 `internal/tools/toolsearch/`（工具名常量、Description 文案占位）

## 2. 已发现集合（TDD）

- [x] 2.1 写测试：`Session.MarkToolsDiscovered` 后 `DiscoveredTools()` 反映新值；并发读写无 data race（`-race`）
- [x] 2.2 在 `internal/session/session.go` 实现 `discovered` 集合、`DiscoveredTools()`、`MarkToolsDiscovered()`（`mu` 保护），并让 `Session` 满足扩展后的 `ModifierTarget`
- [x] 2.3 写测试：会话 rehydration 时从历史中的 `ToolSearch` 结果重建已发现集
- [x] 2.4 实现 rehydration 重建：扫描历史 `ToolSearch` 结果的机器可读 `matches` 字段填充 `discovered`

## 3. ToolSearch 工具（TDD）

- [x] 3.1 写测试：`select:A,B,C` 精确多选返回对应 schema；部分缺失只返回命中者
- [x] 3.2 写测试：纯关键词按相关性打分排序、受 `max_results` 截断；`+term` 必选项过滤；无命中返回提示且不标记发现
- [x] 3.3 写测试：命中结果含 `<functions>` 块（含每个工具的 `parameters`）+ 机器可读 `matches`；`Result.Modifier` 调用 `MarkToolsDiscovered`
- [x] 3.4 实现 `parseToolName`（按 `__`/`_` 拆分，无 `mcp__` 特判）与关键词打分（移植 Claude Code `ToolSearchTool.ts` 逻辑）
- [x] 3.5 实现 `ToolSearch.Run`：从 `Env.ToolCatalog` 取可延迟工具，处理三种 query，渲染 `<functions>`，挂 `Result.Modifier`；英文 `Description()`
- [x] 3.6 实现 `IsReadOnly()=true`、`CanRunInParallel()=true`、合理 `DefaultTimeout`

## 4. 模式与阈值（TDD）

- [x] 4.1 写测试：`tools.tool_search` 解析——空/`tst`→tst，`auto`/`auto:0`/`auto:100`/`auto:50`/`standard` 各自映射，`auto:abc`/`auto:200`/`foo` 被校验拒绝
- [x] 4.2 在 `internal/config/{config,validate}.go` 新增 `ToolsConfig.ToolSearch`（默认 `tst`）与枚举校验
- [x] 4.3 写测试：`auto` 阈值——可延迟工具体量跨越 `max_history_tokens*pct%` 时延迟开/关切换
- [x] 4.4 实现模式判定与 chars/4 体量估算 + 阈值比较（mode helper，可置于 `internal/agent` 或新 helper）

## 5. 组装集成（TDD）

- [x] 5.1 写测试（用 fake `Deferrable` 工具）：`tst` 模式下可延迟未发现工具被排除出 schema、仅进公告；`ToolSearch` 始终在列；非可延迟工具照常
- [x] 5.2 写测试：发现某工具后，后续轮其完整 schema 持续出现；compaction 后仍在
- [x] 5.3 写测试：`standard` 模式与现状逐项等价（无公告、无 ToolSearch 强制、全量 schema）
- [x] 5.4 写测试：AllowedTools 排除的可延迟工具既不在列表也不在公告
- [x] 5.5 改造 `buildToolSchemas`：延迟过滤、始终含 `ToolSearch`、构造并填充 `Env.ToolCatalog`
- [x] 5.6 实现 `<available-deferred-tools>` 公告注入请求消息尾部（不入 cache 前缀），仅列未发现的可延迟工具

## 6. MCP Adapter opt-in（TDD）

- [x] 6.1 写测试：`Adapter.ShouldDefer()` 默认 true；server `always_load:true` 时 false
- [x] 6.2 在 `internal/mcp/adapter.go` 实现 `Deferrable`；`internal/mcp/host.go` 的 `ServerConfig` 新增 `always_load` 字段并透传给 `NewAdapter`

## 7. 本地描述 ASCII 守门（TDD）

- [x] 7.1 写测试：遍历全部本地（非 MCP）工具，断言 `Description()` 为纯 ASCII
- [x] 7.2 确认现有本地工具全部通过（调研已确认为英文；如有遗漏则修正）

## 8. 接线与端到端

- [x] 8.1 在 `cmd/workhorse-agent/cmd_serve.go` 注册 `ToolSearch` 内建工具
- [x] 8.2 端到端测试（fake `Deferrable` 工具注册进 registry）：模型首轮看不到该工具 schema → 调 `ToolSearch` 命中 → 次轮 schema 出现并可调用 → compaction 后仍可调用
- [x] 8.3 更新 `CLAUDE.md`：新增 "Tool search" 小节（机制、`tools.tool_search` 三档、`Deferrable`、本地描述英文约束、MCP `always_load`）
- [x] 8.4 `golangci-lint run` + `go test ./... -race` 全绿
