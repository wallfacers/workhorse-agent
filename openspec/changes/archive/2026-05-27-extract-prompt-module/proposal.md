## Why

项目中 LLM 面向的提示词散落在 4 个包（`agent`、`skills`、`tools/bash`、`tools/builtin`）中，以硬编码
常量或内联字符串形式存在。这导致三个问题：

- **无法集中审视**：改一条系统提示词要翻 `system_prompt.go` + `compaction.go` + `injector.go`
  三个文件，且没有统一入口知道"项目里到底有几条提示词"
- **没有占位符机制**：所有提示词都是纯字符串拼接（`base + note`），未来扩动态
  上下文变量（workdir、session ID、allowed tools 列表）缺乏统一范式
- **缺乏提示词标准**：新增提示词时没有"往哪里放、怎么写、怎么传参"的规范，导致风格参差
- **语言混用**：`cancelledNote` 和 `Compaction` 是英文，但 skill manifest footer
  "可以调用 LoadSkill 工具加载完整指令。" 是中文，无统一语言策略

本变更提取一个独立的 `internal/prompt` 包，采用分层设计：Go 常量默认值 + `text/template`
模板引擎，为所有 LLM 面向的提示词建立统一管理、渲染和安全约束机制。

## What Changes

在 `internal/prompt/` 下新建提示词模块，包含：

- **模板引擎**：`Template` 类型 + `MustParse`（编译时解析）+ `Execute(data map[string]any)`
- **三个内置模板**：
  - `SystemPrompt` — agent 系统提示词，占位符 `{{.BasePrompt}}`
  - `Compaction` — 上下文压缩摘要提示词（当前无占位符，预留模板结构）
  - `SkillManifest` — 技能清单注入模板，占位符 `{{range .Skills}}`，footer 写死在模板内
- **常量**：`CancelledToolOutput`、`CompactionFallback`、`CancelledNote`（不含前导换行）
- **便捷函数**：`BuildSystemPrompt(base string) string`，封装 TrimRight + 模板渲染，
  与原逻辑输出严格等价

同时修复已存在的 spec ↔ 实现脱节：`FormatManifest` 从未被接线到 system prompt 流程，
本变更补上这条接线。

明确**不做**：

- 不提取工具 Description/InputSchema — 与代码逻辑紧耦合，提取收益低
- 不提取输出标记字符串（`[truncated]` 等）— 是格式常量不是提示词
- 不提取 CLI 提示文本 — 面向人类终端
- 不实现 overlay 文件系统覆盖（`~/.workhorse-agent/prompts/*.tmpl`）— 留后续迭代
- 不引入外部模板引擎 — `text/template` 是标准库，零依赖
- 不引入多语言提示词切换机制 — 但统一所有提示词为英文（见 D8）

## Capabilities

### New Capabilities

- `prompt-module`：集中管理所有 LLM 面向提示词的包；提供 Template 类型、MustParse 编译、
  Execute 渲染、内置默认模板、安全约束（`map[string]any` 只允许基本类型和 `[]map[string]string`，
  不传结构体）；提示词语言统一为英文

### Modified Capabilities

- `agent-loop`：`BuildSystemPrompt`、`CancelledToolOutput`、compaction 摘要提示词从 `agent`
  包迁移到 `prompt` 包；`loop.go` 和 `compaction.go` 改为 import prompt 包；
  `Compactor.SystemPrompt` 死字段删除
- `skills-loader`：`FormatManifest` 内部从 `strings.Builder` 手拼改为调用
  `prompt.SkillManifest.Execute`；签名不变，调用方和测试无需改动；
  **新增接线**：`cmd_serve.go` 的 `newRunnerFactory` 中将 `FormatManifest` 输出
  拼入 `SystemPromptBase`，兑现 `skills-loader/spec.md` "system prompt SHALL 追加 skill 清单" 的要求

## Impact

- **新增代码**：约 200 行 Go（含测试 ~150 行），落在 `internal/prompt/`
- **删除代码**：`internal/agent/system_prompt.go`（27 行）；`Compactor.SystemPrompt` 字段
- **修改代码**：`loop.go`（4 处引用换前缀）、`compaction.go`（2 处改动 + 删死字段）、
  `skills/injector.go`（内部重写，签名不变）、`cmd_serve.go`（补接线）
- **新增 go.mod 依赖**：0（`text/template` 是标准库）
- **行为变更**：
  - `BuildSystemPrompt` 输出与原逻辑逐字节等价
  - `FormatManifest` 输出与原逻辑一致，但 footer 从中文改为英文
  - **新增**：有 skill 时 system prompt 末尾追加 `<available_skills>` 块（此前从未接线）
- **安全**：模板上下文不包含 API key、token 等敏感数据；`text/template` 原生免疫 SSTI；
  `SystemPromptBase` 不对 API 用户暴露
- **工期估算**：1-2 天（单人）

## Risks

- **R1 / 低 / 模板输出微差异**：`text/template` 对空行、尾换行的处理可能与手工拼接
  不完全一致。*缓解*：迁移前在原 `system_prompt.go` 上跑 baseline 采集两组
  `[]byte` 期望值（空 base / 有 base），硬编码为测试常量 `wantBytes`，迁移后断言
  `[]byte(got) == wantBytes`

- **R2 / 低 / 循环依赖**：`skills` 包 import `prompt` 包，如果未来 `prompt` 需要 import
  `skills`，会形成循环。*缓解*：`prompt` 包禁止 import `internal/agent`、
  `internal/skills`、`internal/tools`、`internal/config` 等 internal 子包——
  写入 `doc.go` 并由 code review 强制

- **R3 / 低 / Skill manifest 接线影响 system prompt 长度**：有大量 skill 时
  manifest 块会占掉 system prompt 的 token 预算。*缓解*：manifest 只含 name + trigger
  两行/条，典型 5 条 skill 约 200 字符，可忽略；未来如有问题可加截断逻辑

- **R4 / 低 / FormatManifest footer 语言变更**：footer 从中文改为英文，现有
  `integration_test.go` 断言 `strings.Contains(got, "LoadSkill")` 不受影响
  （"LoadSkill" 在英文 footer 中也出现）。`injector_test.go` 的
  `TestFormatManifest_TwoSkills` 断言 `strings.Contains(got, "LoadSkill")` 同理

- **R5 / 中 / Skill manifest 不响应 hot-reload**：`skillCatalog` 在启动时
  一次性 `skills.Scan` 后被 `newRunnerFactory` 闭包捕获（`cmd_serve.go:81`），
  热加载的 skill 不会出现在新会话的 system prompt 中。这是已存在的 spec 偏离
  （`skills-loader/spec.md` 要求"每次创建会话时扫描"，实际只扫一次），
  本变更接线后该偏离的可见性提高。*缓解*：本变更不修复，留后续 change——
  把 catalog 换成 `func() *Catalog` 工厂或在 factory 内重扫
