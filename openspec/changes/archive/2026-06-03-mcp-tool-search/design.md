## Context

发给模型的 tool 列表由 `internal/agent/loop.go` 的 `buildToolSchemas()` 每轮重建：取 `l.Session.AllowedTools()`，经 `l.Registry.Filtered(allowed)` 得到 `[]tools.Tool`，再映射成 `[]provider.ToolSchema{Name, Description, InputSchema}` 交给 provider。这是延迟过滤的天然插入点。

关键既有事实（决定方案可行）：

- `tools.Tool` 接口仅有 `Name/Description/InputSchema/IsReadOnly/CanRunInParallel/DefaultTimeout/Run`，**无任何 "isMcp" 或来源元数据**。MCP `Adapter`（`internal/mcp/adapter.go`）只是一个普通 `tools.Tool`，命名 `server__tool`（单下划线前缀，**非** Claude Code 的 `mcp__server__tool`）。
- 工具改会话状态已有机制：`Result.Modifier`（`ContextModifier`）在工具批次结算后由 session 应用；`ModifierTarget` 当前只暴露 `SetAllowedTools`。`Session` 已实现该接口（`internal/session/session.go`）。
- `internal/mcp` 的 `Host`/`Adapter` 存在但**全仓库无 `mcp.NewHost` 调用**——MCP 尚未接线。
- token 估算有现成的 `agent.EstimateTokens`（chars/4 启发式，`internal/agent/compaction.go`）；上下文规模代理值为 `agent.max_history_tokens`（默认 200_000）。
- 本仓库 harness 自身使用的 ToolSearch 即方案 B 形态：返回 `<functions>` 文本块、随后工具变为可调用——已验证可行。

## Goals / Non-Goals

**Goals:**
- provider 无关地实现 schema 延迟加载 + 按需发现，不依赖任何 beta API。
- 机制通用：任意实现 `Deferrable` 的工具可延迟；MCP `Adapter` 预埋 opt-in。
- 复用现有 `ContextModifier`/`ModifierTarget` 与 `buildToolSchemas`，最小化新概念。
- 阈值语义与 Claude Code 对齐（`tst`/`auto`(10%)/`auto:N`/`standard`）。
- `standard` 模式与现状逐字节等价（零回归）。
- 本地工具描述强制英文（ASCII），由测试守门。

**Non-Goals:**
- 不接线 MCP host（`mcp.NewHost` 接入 registry 属独立 change）。
- 不做 Anthropic `tool_reference` 原生快路径。
- 不延迟 `Dispatch`/`ExternalAgent` 等单个高频重型工具。
- 不实现 GrowthBook 式远程"不支持 tool_reference 的模型清单"——方案 B 不依赖模型能力。
- 不做 `searchHint`（Claude Code A/B 证明无收益）。

## Decisions

### D1：延迟判定 — 可选 `Deferrable` 接口，靠元数据而非名字前缀

新增可选接口：

```go
// Deferrable is an optional interface a Tool may implement to participate in
// tool search. A tool whose ShouldDefer() returns true is "deferred": its full
// schema is withheld from the model's tool list until ToolSearch surfaces it.
// Tools that do not implement Deferrable are never deferred.
type Deferrable interface {
    ShouldDefer() bool
}
```

`isDeferred(t)` = `t` 实现 `Deferrable` 且 `ShouldDefer()` 为 true，且 `t.Name() != ToolSearchName`。MCP `Adapter` 实现 `ShouldDefer() bool { return !a.alwaysLoad }`。

**不照搬** Claude Code 靠 `mcp__` 名字前缀判定 isMcp 的做法——本仓库 MCP 命名是 `server__tool`，且靠字符串前缀猜来源很脆。用接口元数据更干净，也让未来任何工具（不限 MCP）都能 opt-in。

- 备选：在 `Tool` 接口加 `ShouldDefer()` 必选方法 —— 需改所有现有工具，侵入大，弃用。
- 备选：registry 记录"哪些名字来自 MCP host" —— 把延迟资格耦合到注册来源，不如让工具自述。

### D2：`ToolSearch` 工具 — 客户端关键词搜索 + `<functions>` 返回

`internal/tools/toolsearch`，名字常量 `ToolSearch`，`Description()` 英文（移植 Claude Code `prompt.ts` 文案，去掉 `tool_reference` API 措辞，保留 query 形式说明）。

- 输入：`{"query": string, "max_results": int (default 5)}`。
- 数据源：从 `Env.ToolCatalog` 读取**当前会话可延迟工具**的 `{name, description, inputSchema}`（见 D4）。
- query 解析（移植 `ToolSearchTool.ts:186` 打分逻辑）：
  - `select:A,B,C` → 精确多选；命中即返回（名字不在延迟集但在全集也接受，作无害 no-op）。
  - 纯关键词 → 对每个候选工具按"name 分段精确命中 / 子串命中 / description 词边界命中"加权打分，取 `max_results` 高分。
  - `+term` → 该 term 为必选，先过滤出 name/description 全含所有必选项的候选再打分。
  - 名字分段：`parseToolName` 对 `server__tool` 按 `__`/`_` 拆分小写（移植并去掉 `mcp__` 特判）。
- 输出：`Result.Output` 为 `<functions>` 块，每个命中工具渲染一行 `{"description":..., "name":..., "parameters": <inputSchema>}`；无命中时返回提示文本。同时 `Result.Modifier` 携带命中工具名 → `MarkToolsDiscovered`。
- `IsReadOnly() = true`，`CanRunInParallel() = true`。

渲染完整 schema 进结果文本是为了让模型即时看到参数；真正使其"可调用"的是 D3 下一轮把真实 schema 注入 `tools` 数组。两者一致即可（同源于 catalog）。

### D3：已发现集合 — 会话状态 + Modifier 写入 + rehydration 重建

- `Session` 新增 `discovered map[string]struct{}`（`mu` 保护）、`DiscoveredTools() []string`、`MarkToolsDiscovered(names []string)`。
- `ModifierTarget` 接口扩展 `MarkToolsDiscovered(names []string)`；`Session` 实现之。`ToolSearch` 的 `Result.Modifier.Apply(target)` 调用它。
- **生命周期**：集合存于内存，**跨 compaction 存活**（挂在 `Session` 而非 history，比 Claude Code 扫历史更简单）。
- **rehydration 重建**：会话从持久化恢复时，`discovered` 内存态丢失。若不重建，历史里"模型已调用某延迟工具"而该工具未进 `tools` 数组 → 调用落空。故恢复路径 SHALL 扫描历史中已持久化的 `ToolSearch` 工具结果，解析出曾命中的工具名重建 `discovered`（镜像 `extractDiscoveredToolNames`）。为使扫描可靠，`ToolSearch` 结果除 `<functions>` 文本外，附带机器可读的命中名字段（如结果 JSON 的 `matches`）。

### D4：组装集成 — `buildToolSchemas` 延迟过滤 + 公告注入 + catalog 暴露

`buildToolSchemas()` 改造（伪码）：

```
allowed   := session.AllowedTools()
tools     := registry.Filtered(allowed)
mode      := toolSearchMode(config)             // tst | auto | auto:N | standard
discovered:= session.DiscoveredTools() (set)

deferActive := mode != standard &&
               (mode == tst || deferredTokenBytes(tools) >= threshold(config))   // D5

deferredNames := []string{}
for t := range tools {
    if deferActive && isDeferred(t) && !discovered.has(t.Name()) {
        deferredNames = append(deferredNames, t.Name())   // 只收名字，不发 schema
        continue
    }
    out = append(out, ToolSchema{t.Name(), t.Description(), t.InputSchema()})
}
if deferActive {
    ensure ToolSearch in out                                // 始终可用
    // 公告：把 deferredNames 以合成消息注入请求尾部（非 cache 前缀）
}
```

- **公告位置**：`<available-deferred-tools>\n<names>\n</available-deferred-tools>` 作为一条合成消息追加到请求**消息尾部**，不并入 base-first 的 system prompt 缓存前缀（避免池变动 busting cache，与 `prompt-cache` 能力的稳定前缀约定一致）。
- **catalog 暴露**：`Env` 新增 `ToolCatalog any`（沿用本仓库"`any` + 类型断言避免 import cycle"惯例，如 `ExtAgentRegistry`/`TaskList`）。`ToolSearch` 断言为：

```go
type ToolCatalog interface { DeferredTools() []ToolInfo }
type ToolInfo struct { Name, Description string; InputSchema json.RawMessage }
```

loop 每轮用同一份 `tools` + `discovered` 构造 catalog 填入 `Env`，保证 `ToolSearch` 看到的可延迟集与组装侧一致。

### D5：模式与阈值 — 照搬 Claude Code 语义

`tools.tool_search` 配置值 → 模式：

| 配置值 | 模式 | 行为 |
| --- | --- | --- |
| 空 / `tst` | tst | 永远延迟可延迟工具（**默认**，对齐 CC unset 默认） |
| `auto` | auto | 可延迟工具体量 ≥ context 10% 才延迟 |
| `auto:N` | auto | 阈值百分比改为 N（0-100 钳制） |
| `auto:0` | tst | 等价永远延迟 |
| `auto:100` | standard | 等价从不延迟 |
| `standard` | standard | 从不延迟，行为同现状 |

阈值计算：`threshold = floor(max_history_tokens * pct/100)`；`deferredTokens ≈ EstimateTokens 风格的 chars/4`，对每个可延迟工具的 `name+description+inputSchema` 求和。`max_history_tokens` 作为 context 规模的代理值（本仓库无 per-model context window 表，不为此引入）。

### D6：本地工具描述 ASCII 约束

新增测试遍历 registry 中**非 MCP**（不实现 `Deferrable` 或显式标记为本地）的工具，断言 `Description()` 为纯 ASCII。理由：本地描述进入"始终加载"的 tool 列表，必须英文；MCP 描述来自 server 不受此约束。当前所有本地工具描述已是英文（本 change 调研确认），该测试是回归守门而非修复。

## Risks / Trade-offs

- **首次往返成本**：延迟模式下，模型首次用某延迟工具需先调 `ToolSearch`，多一轮。可接受——这正是用 context 换往返的取舍；`tst` 仅对 MCP 这类大体量工具生效（本地工具不延迟）。
- **MCP 未接线 → 当下无真实受益**：本 change 是前瞻基础设施。端到端验证用 fake `Deferrable` 工具覆盖（test-only），确保 MCP 接线后零改动生效。
- **rehydration 重建依赖结果可解析**：若历史里 `ToolSearch` 结果格式变更，重建会失效 → 退化为模型重新搜索（自愈，非致命）。
- **关键词搜索精度**：纯启发式打分，可能漏召。缓解：模型可用 `select:` 直选、可加 `max_results`、可多次搜索。与 Claude Code 现状一致。

## Migration

无数据迁移。默认 `tst` 模式仅在存在可延迟工具时改变行为；当前无 MCP 工具接线，故默认对现网行为**无可见影响**，直到 MCP host 接线或有工具实现 `Deferrable`。需要旧行为者显式设 `tools.tool_search: standard`。
