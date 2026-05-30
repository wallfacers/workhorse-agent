## Context

`internal/agent/loop.go:391-402` 在每个 turn 重新拼接 system prompt：

```go
base := l.SystemPromptBase
if block := memory.Block(l.Session.MemorySnapshot); block != "" {
    base = block + "\n\n" + base          // memory prepend 到 base 前
}
if envBlock := l.Session.EnvSnapshot; envBlock != "" {
    base = envBlock + "\n\n" + base        // environment prepend 到最外层
}
req.System = prompt.BuildSystemPrompt(base)
```

最终顺序是 `environment → memory → base`。`EnvSnapshot` 含 cwd、cli_tools、
dispatch_agents 等随环境/启动变化的内容；`base`（`DefaultBasePrompt` 引入后）是
一大段稳定文本。当前实现把动态内容放在缓存前缀位置，静态内容反而靠后。

同时存在 spec 偏离：`prompt-memory/spec.md` 写 memory 经 `{{.Memory}}` 模板变量注入，
实现却是字符串 prepend。本变更顺带收敛为单一组装路径。

约束：
- `internal/prompt` 包必须保持 IO-free（`boundary_test` 限定可 import 的标准库集合）。
- memory 的同会话 byte-stable 保证（prompt cache 命中前提）不能被破坏。
- `BuildSystemPrompt` 历史上有「输出逐字节等价」的回归测试基线。

## Goals / Non-Goals

**Goals:**
- 将组装顺序改为 `base → environment → memory`，让静态 base 成为缓存前缀。
- 收敛为唯一、byte-stable 的组装路径，消除 spec 与实现的偏离。
- 用测试锁定「静态在前」契约，防止回归。

**Non-Goals:**
- 不改变 system prompt 的**内容**（仅顺序）。
- 不改 memory 的字符上限、文件位置、快照不可变等既有行为。
- 不引入新的缓存控制机制（如显式 cache_control 断点）——本变更只调顺序；
  显式断点可作后续 change。

## Decisions

### D1：顺序 `base → environment → memory`，而非其他排列

base 最稳定（跨会话相同）、environment 次之（同机器跨会话大体稳定、cwd 可能变）、
memory 最易变（用户随时编辑）。按「稳定→易变」排布最大化最长公共前缀。
**备选**：`base → memory → environment` 被否——environment 比 memory 更稳定，应更靠前。

### D2：组装方式 —— 两种候选，design 阶段二选一

- **D2a（小改）**：保留 `loop.go` 内拼接，仅反转顺序为 append（base 在前，
  依次 append environment、memory）。改动最小，但组装逻辑仍在 agent 包、
  与 spec 的「`{{.Memory}}` 模板变量」描述不一致。
- **D2b（收敛到 prompt 包）**：给 `prompt.SystemPrompt` 模板增加 `{{.Environment}}`
  与 `{{.Memory}}` 占位符，`BuildSystemPrompt` 改为接收结构化输入并按固定顺序渲染，
  agent 包只传三段原文。更贴合 spec、组装路径唯一，但触及 `BuildSystemPrompt`
  签名与 `boundary_test`（不可引入新 import；纯字符串拼接无碍）。

倾向 **D2b**，因为它同时消除 spec 偏离并把顺序固化在受测的 prompt 包内；
但需在实现前确认 `BuildSystemPrompt` 的所有调用方与回归基线影响面。最终取舍留待
apply 前评估调用方数量。

### D3：缓存收益的验证方式

无法在单测里直接断言「缓存命中率提升」。改为断言**前缀稳定性**：构造两个仅 memory
不同、base+environment 相同的输入，断言渲染结果的最长公共前缀覆盖整个 base+environment
段。再辅以人工/集成层对 provider 请求体 system 字段的核对。

## Risks / Trade-offs

- **[回归基线失效] → 缓解**：所有硬编码 system prompt 期望值的测试（`prompt` 包
  byte-stable、agent loop 快照）需重采 baseline。先跑现有测试采集旧值，改完用新顺序
  重新生成期望常量，确保是「顺序变化」而非「内容意外丢失」。
- **[D2b 改签名牵连调用方] → 缓解**：先 grep `BuildSystemPrompt` 全部调用点，
  评估改造面；若调用方过多则回退 D2a。
- **[memory byte-stable 保证被破坏] → 缓解**：保留 memory 块的稳定分隔符，仅改其
  在整体中的位置；新增 scenario 锁定「同会话 memory 块逐字节稳定」。
- **[boundary_test 失败] → 缓解**：组装仅用 `strings`/`text/template`，不新增 import。

## Migration Plan

1. grep `BuildSystemPrompt` 调用点，决定 D2a / D2b。
2. 采集旧 system prompt 的 baseline 字节串（空/有 memory/有 environment 各组合）。
3. 实施顺序调整，更新 `loop.go:391-402`（D2a）或 prompt 模板+签名（D2b）。
4. 更新 byte-stable 测试期望值；新增「静态前缀」「顺序不改内容集合」scenario 测试。
5. 跑 `go test ./...`、确认 `boundary_test` 通过、`gofmt`/`golangci-lint` 干净。

回滚：单 commit 还原顺序即可，无数据/持久化层影响。

## Open Questions

- 选 D2a 还是 D2b？取决于 `BuildSystemPrompt` 调用面（apply 前 grep 确认）。
- 是否顺带引入显式 `cache_control` 断点（在 base 段尾），还是仅靠隐式前缀缓存？
  倾向本变更不做，留独立 change。
