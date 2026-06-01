# prompt-module Specification

## Purpose

集中管理所有 LLM 面向的提示词，提供统一的模板类型、编译、渲染和安全约束机制。
本包承载 system prompt、压缩摘要器提示词、技能清单等所有发送给 LLM 的文本资源。

## Requirements

### Requirement: Template 类型和生命周期

`internal/prompt` 包 SHALL 提供 `Template` 类型，生命周期为：

1. **编译时**：`MustParse(name, body string) *Template` 解析 `text/template` 语法；
   解析失败直接 panic（开发期错误，不应进入编译产物）
2. **运行时**：`Execute(data map[string]any) (string, error)` 渲染模板；
   data 为 `nil` 表示无占位符的纯文本模板

使用 `text/template`（不使用 `html/template`，因为输出不是 HTML）。

#### Scenario: 合法模板编译和执行

- **WHEN** `MustParse("test", "Hello {{.Name}}")` 被调用
- **THEN** 返回 `*Template`，无 panic

- **WHEN** 该模板 `Execute(map[string]any{"Name": "world"})` 被调用
- **THEN** 返回 `"Hello world", nil`

#### Scenario: 非法模板编译 panic

- **WHEN** `MustParse("bad", "{{.Unclosed")` 被调用
- **THEN** panic

### Requirement: 安全约束

`Execute` 的 `data` 参数 SHALL 只包含以下类型：

- 基本类型：`string`、`int`、`bool`
- 切片：`[]map[string]string`
- `nil`（无占位符模板）

SHALL NOT 包含：

- 任何结构体（防止模板调用导出方法泄露数据）
- `func`、`chan`、`unsafe.Pointer`
- `map` 中嵌套 `map`（仅允许一层 `map[string]any`，值为上述允许类型）

`text/template` 对 data 值不做二次解析（原生免疫 SSTI / Server-Side Template Injection）。

#### Scenario: SSTI 免疫

- **WHEN** `Execute(map[string]any{"Evil": "{{printf \"%s\" .Secret}}"})` 被调用
- **THEN** 输出包含字面文本 `{{printf "%s" .Secret}}`，不被模板引擎解析

### Requirement: 并发安全

`Template.Execute` SHALL 可被多个 goroutine 并发调用。
`text/template.Template.Execute` 本身是并发安全的（stdlib doc 明确）；
`prompt.Template.Execute` 内部使用 `bytes.Buffer` 局部变量，无共享可变状态。

#### Scenario: 并发渲染

- **WHEN** 同一个 `Template` 实例被 100 个 goroutine 并发 `Execute`
- **THEN** 每次返回正确结果，无 data race

### Requirement: 内置模板

包 SHALL 导出以下预编译模板和常量：

| 名称 | 类型 | 占位符 | 用途 |
|------|------|--------|------|
| `SystemPrompt` | `*Template` | `{{.BasePrompt}}`, `{{.Instructions}}`, `{{.Environment}}`, `{{.Memory}}` | agent 系统提示词 |
| `Compaction` | `*Template` | 无（预留） | 上下文压缩摘要器提示词 |
| `SkillManifest` | `*Template` | `{{range .Skills}}` | 技能清单注入 |
| `CancelledToolOutput` | `string` | — | 取消时 tool_result 输出 |
| `CancelledNote` | `string` | — | cancelled 标记语义说明（不含前导换行） |
| `CompactionFallback` | `string` | — | 摘要失败兜底文本 |

`SystemPrompt` 模板的组装顺序 SHALL 为：`BasePrompt → CancelledNote → Environment → Instructions → Memory`，非空段之间用 `"\n\n"` 连接。`Instructions` 段位于 `Environment` 之后、`Memory` 之前。

`SystemPromptInput` struct SHALL 包含四个字段：`Base string`、`Environment string`、`Instructions string`、`Memory string`。`BuildSystemPrompt` SHALL 将所有四个字段传入模板渲染。

#### Scenario: BuildSystemPrompt 空输入

- **WHEN** `BuildSystemPrompt(SystemPromptInput{})` 被调用
- **THEN** 返回纯 `CancelledNote`，无前导空行

#### Scenario: BuildSystemPrompt 有输入

- **WHEN** `BuildSystemPrompt(SystemPromptInput{Base: "You are a helper."})` 被调用
- **THEN** 返回 `"You are a helper.\n\n" + CancelledNote`

#### Scenario: BuildSystemPrompt with instructions only

- **WHEN** `BuildSystemPrompt(SystemPromptInput{Instructions: "<instructions>...</instructions>"})` 被调用
- **THEN** 返回 `CancelledNote + "\n\n" + "<instructions>...</instructions>"`

#### Scenario: BuildSystemPrompt full assembly order

- **WHEN** `BuildSystemPrompt(SystemPromptInput{Base: "B", Environment: "E", Instructions: "I", Memory: "M"})` 被调用
- **THEN** 返回 `"B\n\n" + CancelledNote + "\n\nE\n\nI\n\nM"`

### Requirement: 提示词语言标准

`prompt` 包内所有提示词 SHALL 使用英文。理由：
- LLM 对英文指令的遵循精度高于中文
- 项目代码库和注释以英文为主
- 统一语言避免混合风格的维护负担

#### Scenario: 内置模板使用英文

- **WHEN** 检查 `SystemPrompt`、`SkillManifest`、`Compaction` 模板和 `CancelledNote`、`CompactionFallback` 常量
- **THEN** 所有面向 LLM 的文本均为英文，不含中文字符

### Requirement: 包依赖约束

`internal/prompt` 包 SHALL NOT import 以下 internal 子包：
- `internal/agent`
- `internal/skills`
- `internal/tools`
- `internal/config`
- `internal/session`
- `internal/coord`
- `internal/provider`
- `internal/api`
- `internal/store`

此约束防止循环依赖（`agent` → `prompt`，`skills` → `prompt` 已存在，
反向引入会形成环）。

#### Scenario: 边界测试守护

- **WHEN** `internal/prompt/boundary_test.go` 扫描包内 `*.go` 文件的 import 列表
- **THEN** 不出现上述任何 internal 子包；若新增引入，测试 fail

### Requirement: 占位符命名风格

模板占位符 SHALL 使用 PascalCase：`{{.BasePrompt}}`、`{{.Name}}`、`{{.Trigger}}`。
与 Go 导出字段命名一致。

#### Scenario: 内置模板使用 PascalCase 占位符

- **WHEN** 检查 `SystemPrompt`、`SkillManifest` 等模板源码
- **THEN** 所有占位符首字母大写（`.BasePrompt`、`.Skills`、`.Name`、`.Trigger`），无 camelCase 或 snake_case
