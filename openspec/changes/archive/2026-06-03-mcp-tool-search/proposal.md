## Why

每个 MCP server 可能暴露几十个工具，每个工具的 `name + description + inputSchema` 都要塞进发给模型的 tool 列表里。连上数个 server 后，光工具定义就能吃掉数万 token，挤占对话上下文，并搅乱 prompt cache 的稳定前缀。

Claude Code 用 **Tool Search** 解决：初始只把工具**名字**暴露给模型，完整 schema 延迟加载；模型需要时调用一个 `ToolSearch` 工具按关键词搜出来。我们要把这个机制借鉴进 workhorse-agent。

两个现实约束决定了实现形态：

1. **provider 无关**。workhorse-agent 同时支持 Anthropic 与 OpenAI。Claude Code 的原生实现依赖 Anthropic 的 `defer_loading` / `tool_reference` beta API（服务端展开 schema），OpenAI 路径无法用。因此本 change 采用**客户端模拟（方案 B）**：`ToolSearch` 是一个普通工具，命中后把目标工具的完整 JSON schema 以 `<functions>` 文本块返回，并把命中工具标记为"已发现"；下一轮组装 tool 列表时，把已发现工具的真实 schema 注入 `tools` 数组使其可调用。该形态与本仓库 harness 自身使用的 ToolSearch 一致，已被验证可行。

2. **MCP host 当前未接线**。`internal/mcp`（Host + Adapter）已存在但无任何地方调用 `mcp.NewHost`，MCP 工具尚未进入 registry。因此本 change 构建的是**前瞻性基础设施**：机制做成通用的——任意实现 `Deferrable` 接口的工具都可延迟；MCP `Adapter` 预埋 opt-in。待未来独立 change 把 MCP host 接进 registry，tool search 即自动对其生效，无需改动本机制。

## What Changes

- **新增 `Deferrable` 可选接口（`internal/tools`）**：工具实现 `ShouldDefer() bool` 即可参与延迟；未实现者**永不延迟**（始终全量加载）。MCP `Adapter` 实现该接口（默认延迟，受 per-server `always_load` 反选）。本 change **不延迟** `Dispatch`、`ExternalAgent` 等单个高频重型工具——延迟单个工具几乎不省 context，反而多一次往返。

- **新增 `ToolSearch` 内建工具（`internal/tools/toolsearch`）**：借鉴 Claude Code `ToolSearchTool` 的关键词打分搜索。支持三种 query：`select:A,B,C`（精确多选）、`+slack send`（`+` 前缀为必选项）、`notebook jupyter`（纯关键词加权打分）。命中后返回 `<functions>` 块（每个工具一行 `{"name","description","parameters"}`），并通过 `Result.Modifier` 把命中工具名标记为已发现。其 `Description()` 为英文。

- **新增"已发现工具"会话状态**：`Session` 持有 `discovered` 集合 + `DiscoveredTools()` 访问器；`ModifierTarget` 扩展 `MarkToolsDiscovered(names []string)`。集合存于内存、跨 compaction 存活；会话 rehydration 时通过扫描历史中已持久化的 `ToolSearch` 结果重建（镜像 Claude Code 的 `extractDiscoveredToolNames`），避免恢复后对已发现工具的调用落空。

- **改造 tool 列表组装（`internal/agent/loop.go buildToolSchemas`）**：当延迟生效时，跳过"可延迟且未发现且非 ToolSearch"的工具 schema，仅收集其名字；始终包含 `ToolSearch`；把"可延迟但尚未发现"的工具名以 `<available-deferred-tools>` 合成消息注入请求**尾部**（不污染 cache 前缀）。`Env` 新增 `ToolCatalog` 访问器，向 `ToolSearch` 暴露可延迟工具的 `name/description/inputSchema` 供打分与渲染。

- **新增三档模式 + Claude Code 阈值**：配置 `tools.tool_search` 取值 `tst`（默认，永远延迟可延迟工具）/ `auto`（可延迟工具体量 ≥ context 10% 才延迟）/ `auto:N`（自定百分比 0-100）/ `standard`（从不延迟）。阈值语义照搬 Claude Code（默认 10%），token 体量用现有 chars/4 启发式估算，阈值基准取 `agent.max_history_tokens` 的对应百分比。

- **新增本地工具描述 ASCII 约束**：新增测试断言所有本地（非 MCP）工具的 `Description()` 为纯 ASCII（英文），防止后续有人写入非英文描述污染始终加载的 tool 列表。

- **配置项**：`internal/config` 的 `ToolsConfig` 新增 `tool_search` 字段（默认 `tst`，校验枚举）；`internal/mcp` 的 `ServerConfig` 新增 `always_load`（MCP 接线后 per-server 反选延迟）。

## Capabilities

### New Capabilities
- `tool-search`: 可延迟工具的 schema 延迟加载与按需发现——`Deferrable` 接口、`ToolSearch` 工具、已发现集合、三档模式与阈值、`<available-deferred-tools>` 公告。

### Modified Capabilities
- `tool-system`: 新增要求——tool 列表组装在延迟生效时对可延迟且未发现的工具仅暴露名字；本地工具 `Description()` 必须为英文（ASCII）。
- `configuration`: 新增 `tools.tool_search` 字段及其枚举校验。
- `mcp-integration`: 新增要求——MCP `Adapter` 实现 `Deferrable`（默认延迟）；`ServerConfig` 支持 `always_load` 反选。

## Impact

- **代码**：`internal/tools/tool.go`（`Deferrable` 接口、`Env.ToolCatalog`、`ModifierTarget.MarkToolsDiscovered`）、新增 `internal/tools/toolsearch/`、`internal/agent/loop.go`（`buildToolSchemas` 延迟过滤 + 公告注入 + 模式/阈值判定）、`internal/session/session.go`（discovered 集合 + 重建）、`internal/mcp/adapter.go`（实现 `Deferrable`）、`internal/config/{config,validate}.go`（`tool_search` 字段）、`cmd/workhorse-agent/cmd_serve.go`（注册 `ToolSearch`、接线 catalog）。
- **依赖**：无新增第三方依赖。
- **行为**：延迟模式下初始 tool 列表变小、首次使用某延迟工具会多一次 `ToolSearch` 往返。`standard` 模式行为与现状完全一致。
- **范围外（明确不做）**：不接线 MCP host（独立 change）；不实现 Anthropic `tool_reference` 原生快路径；不延迟 `Dispatch`/`ExternalAgent`；不做 GrowthBook 式远程模型能力开关。
- **前瞻**：MCP host 接线后，其工具因 `Adapter` 已实现 `Deferrable` 自动纳入延迟，无需改本机制。
