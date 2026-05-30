## Why

当前 agent loop 组装 system prompt 的顺序是 `environment → memory → base`
（`internal/agent/loop.go:391-402`，通过字符串 prepend 实现）。随着
`add-orchestrator-system-prompt` 引入一段较长的**静态** `DefaultBasePrompt`，
这段最稳定的内容被排在了**动态**的 `<environment>`（含 cwd、随启动变化的 cli_tools /
dispatch_agents 列表）和 memory 块之后。Anthropic prompt cache 按**前缀**命中：
动态内容在前会让静态 base 落在缓存边界之后，跨会话、跨 turn 的缓存复用率下降，
徒增 token 成本与首 token 延迟。

## What Changes

- 将 system prompt 的组装顺序由 `environment → memory → base` 调整为
  **`base → environment → memory`**，使最稳定的静态内容成为缓存前缀。
- 厘清并修复 spec 与实现的既有偏离：`prompt-memory/spec.md` 描述 memory 经
  `{{.Memory}}` 模板变量注入，而实现实际是 `base = block + "\n\n" + base` 的字符串
  prepend。本变更统一为单一、明确的组装路径，并由 byte-stable 测试锁定输出。
- 为组装顺序补一条显式 requirement + scenario，使「静态在前」成为受测试保护的契约，
  防止未来重构再次把动态内容前置。
- **非破坏性**：最终 system prompt 的**内容集合**不变，仅**顺序**变化；对模型语义无影响，
  仅影响缓存命中与成本。

## Capabilities

### New Capabilities
<!-- 无新增能力 -->

### Modified Capabilities

- `agent-loop`：新增/修订「system prompt 组装顺序」requirement —— 静态 `base` 段
  SHALL 作为前缀，`<environment>` 与 memory 块 SHALL 追加其后，且组装路径唯一、
  输出 byte-stable。
- `prompt-memory`：「System prompt injection」requirement 中 memory 块的相对位置由
  「最外层 prepend」改为「base 之后」；同会话内 byte-stable delimiter 的保证不变。

## Impact

- **代码**：`internal/agent/loop.go:391-402`（组装顺序）；可能涉及
  `internal/prompt`（若改为模板变量统一注入则触及 `SystemPrompt` 模板与
  `BuildSystemPrompt` 签名）。
- **测试**：`internal/prompt` 的 byte-stable 断言需更新期望值；`boundary_test`
  约束 `prompt` 包保持 IO-free（若新增模板变量不可引入新 import）；agent loop 层
  若有 system prompt 快照测试需同步。
- **行为**：system prompt 内容集合不变，顺序变化；对 provider 请求体的 system 字段
  逐字节不同（顺序），故所有硬编码 system 期望值的测试需重新采集 baseline。
- **缓存**：预期提升跨会话/跨 turn 的 prompt-cache 前缀命中率，降低 token 成本与延迟。
  需在 design 中明确如何验证（如本地对比缓存命中指标或人工核对前缀稳定性）。
- **依赖**：无新增 go.mod 依赖。
- **风险**：低-中。主要是回归测试期望值更新，以及确认 memory 的同会话 byte-stable
  保证在新位置下依然成立。
