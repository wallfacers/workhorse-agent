## Context

项目中三条 LLM 面向的提示词分散在三个包中，通过三种不同的渲染方式产出到各自的消费者：

```
提示词           当前位置                    渲染方式
──────────────  ──────────────────────     ──────────────
cancelledNote   agent/system_prompt.go     字符串拼接 (base + const)
compaction      agent/compaction.go:105    内联字符串字面量
skill manifest  skills/injector.go         strings.Builder 手拼
```

另外发现已存在的 spec ↔ 实现脱节：`FormatManifest` 从未在生产代码中被调用，
`skills-loader/spec.md` 要求的 "system prompt SHALL 追加 skill 清单" 从未兑现。

本设计统一为 `text/template` 模板 + `prompt.Template` 类型，同时补上接线。

## Goals / Non-Goals

**Goals:**

- 所有 LLM 提示词集中到 `internal/prompt/`，一处可见
- 统一占位符机制（`text/template`），支持 `{{.Var}}` 和 `{{range}}`
- 编译时模板校验（`MustParse` panic on error）
- 安全约束：模板上下文不传入结构体，只允许基本类型
- 现有行为零差异：`BuildSystemPrompt` 输出逐字节等价
- 统一提示词语言为英文
- 补上 `FormatManifest` 到 system prompt 的接线

**Non-Goals:**

- 不做 overlay 文件系统覆盖（留后续迭代）
- 不提取工具 Description/InputSchema
- 不引入外部模板引擎
- 不支持多语言提示词切换（但统一为英文）

## Decisions

### D1 · Execute 签名：`map[string]any`

```go
func (t *Template) Execute(data map[string]any) (string, error)
```

`SkillManifest` 需要 `[]map[string]string`（技能列表），`map[string]string` 无法表达。
`map[string]any` 更通用，安全约束通过代码规范保证：

- 值只允许 `string`、`int`、`bool`、`[]map[string]string`
- 不允许传入任何结构体（防止模板调用对象方法泄露数据）
- 不允许传入 `func` 或 `chan`

### D2 · SystemPrompt 模板的空 base 处理（含 cancelledNote 重定义）

**关键前提**：原 `cancelledNote` 带前导 `"\n\n"`：

```go
// agent/system_prompt.go 原值
const cancelledNote = "\n\nNote: if a tool_result begins with `[CANCELLED]`, ..."
//                     ↑↑ 两个前导换行
```

原 `BuildSystemPrompt` 的逻辑是：
- 空 base → `strings.TrimLeft(cancelledNote, "\n")` → 去掉前导 `\n\n` → `"Note: ..."`
- 有 base → `base + cancelledNote` → base 末尾 + `"\n\n" + "Note: ..."`

在 `prompt` 包中 **重新定义** `CancelledNote` **不含前导换行**：

```go
const CancelledNote = "Note: if a tool_result begins with `[CANCELLED]`, " +
    "the tool call was interrupted by the user. Do not retry it automatically; " +
    "acknowledge the interruption and ask the user how to proceed."
```

模板等价实现：

```go
var SystemPrompt = MustParse("system_prompt",
    "{{.BasePrompt}}{{if .BasePrompt}}\n\n{{end}}" + CancelledNote)
```

逐字节验证（代入实际值）：

| 输入 | 模板输出 | 原逻辑输出 | 一致？ |
|------|---------|-----------|:------:|
| `""` | `"" + "" + "Note: ..."` = `"Note: ..."` | `TrimLeft("\n\nNote:...")` = `"Note: ..."` | ✓ |
| `"X"` | `"X" + "\n\n" + "Note: ..."` = `"X\n\nNote: ..."` | `"X" + "\n\nNote: ..."` | ✓ |

`CancelledNote` 不能放在反引号 raw string 中（正文含反引号字符），所以用 Go
解析字符串常量 + 拼接构造模板 body。

### D3 · `BuildSystemPrompt` 便捷函数保留在 prompt 包

```go
func BuildSystemPrompt(base string) string {
    base = strings.TrimRight(base, " \t\n")
    out, err := SystemPrompt.Execute(map[string]any{"BasePrompt": base})
    if err != nil {
        return base
    }
    return out
}
```

理由：
- `loop.go` 只需改 import 路径，`prompt.BuildSystemPrompt(...)` 对 `BuildSystemPrompt(...)`
- 封装了 TrimRight 和 error fallback，调用方不暴露模板细节
- error fallback 返回 `base`（不 crash）——模板是编译时硬编码的，运行时失败只可能是
  开发错误，但防御性返回比 panic 更安全

### D4 · SkillManifest 模板设计

```go
var SkillManifest = MustParse("skill_manifest",
    "<available_skills>\n"+
        "{{range $s := .Skills}}"+
        "- name: {{$s.Name}}\n"+
        "  trigger: {{$s.Trigger}}\n"+
        "{{end}}"+
        "</available_skills>\n\n"+
        "You can use the LoadSkill tool to load full instructions.\n")
```

footer 写死在模板内，不作为占位符传入。理由：
- footer 值全局唯一，没有调用方需要自定义的场景
- 避免数据驱动的 footer 被 integration_test 断言脆耦合
- 如未来需可变 footer，再抽取为占位符

data 结构：
```go
map[string]any{
    "Skills": []map[string]string{
        {"Name": "code-review", "Trigger": "review code"},
        {"Name": "git-helper",  "Trigger": "git operations"},
    },
}
```

`skills/injector.go` 的 `FormatManifest` 内部改用此模板，但签名不变——调用方和测试
无需任何改动。**触发器的空白折叠（`strings.Fields`）仍在 `FormatManifest` 中完成**，
不在模板层做——保持模板纯净，逻辑在 Go 侧。

### D5 · 文件布局

```
internal/prompt/
├── doc.go              包文档 + 安全约束 + 提示词标准 + 禁止 import 列表
├── template.go         Template 类型 + MustParse + Execute
├── builtins.go         三个模板 + 常量 + BuildSystemPrompt
└── template_test.go    完整测试
```

不拆成 `system.go` / `compaction.go` / `skills.go`——总量约 100 行代码，
拆文件增加导航成本。

### D6 · 安全约束

| 约束 | 机制 |
|------|------|
| SSTI 免疫 | `text/template` 对 data 值不做二次解析 |
| 无敏感数据泄露 | 模板上下文只含操作级字符串，无 API key / token |
| 无结构体方法调用 | Execute 签名约定 + code review |
| 编译时模板校验 | `MustParse` panic on parse error |
| 数据源可信 | `SystemPromptBase` 不对 API 用户暴露 |
| 并发安全 | `Template.Execute` 内部用 `bytes.Buffer` 局部变量，无共享可变状态；`text/template.Template.Execute` 本身并发安全（stdlib doc 明确） |
| 模板引擎选择 | 使用 `text/template`，不使用 `html/template`（输出不是 HTML） |

### D7 · FormatManifest 接线

`FormatManifest` 当前只有测试调用，从未接入 system prompt 流程。
`skills-loader/spec.md` 明确要求 "system prompt SHALL 追加 skill 清单"。

接线位置：`cmd_serve.go` 的 `newRunnerFactory` 闭包内，在
`loop.SystemPromptBase = sess.SystemPromptBase` 之后追加：

```go
manifest := skills.FormatManifest(skillCatalog)
if manifest != "" {
    if loop.SystemPromptBase != "" {
        loop.SystemPromptBase += "\n\n"
    }
    loop.SystemPromptBase += manifest
}
```

**注意**：不能写 `loop.SystemPromptBase += "\n\n" + manifest`，否则当
`sess.SystemPromptBase == ""` 时会产生前导 `\n\n`，被 `BuildSystemPrompt`
的 `TrimRight` 保留在左侧，导致 system prompt 多出空行。

`skillCatalog` 已在 `cmd_serve.go:81` 创建，只需传入 `newRunnerFactory` 闭包。

**Scope**：本接线只覆盖 `cmd_serve.go` 的 `newRunnerFactory` 创建的顶层会话。
`dispatch.go` 派生的子 agent 不接 skill manifest。理由：子 agent 的
`allowed_tools` 通常由 agent 定义窄化，是否需要 `LoadSkill` 由 agent 定义控制，
而非全局注入。

### D8 · 提示词语言标准

所有 `prompt` 包内的提示词 SHALL 使用英文。理由：
- LLM 对英文指令的遵循精度高于中文
- 项目代码库和注释以英文为主
- 统一语言避免混合风格的维护负担

本次变更落地：
- `CancelledNote` — 已是英文，不变
- `Compaction` — 已是英文，不变
- `SkillManifest` footer — 从 "可以调用 LoadSkill 工具加载完整指令。" 改为
  "You can use the LoadSkill tool to load full instructions."

现有 `injector_test.go` 和 `integration_test.go` 断言 `strings.Contains(got, "LoadSkill")`
不受影响（"LoadSkill" 在英文 footer 中也出现）。

### D9 · Compactor.SystemPrompt 死字段处理

`compaction.go:25` 有 `SystemPrompt string` 字段，但 `summarise()` 从未读取。
本变更删除该字段。理由：
- 字段从未生效，删掉不改变行为
- 保留会产生误导——读者以为可以通过它定制 compaction 提示词
- 如未来需要定制，通过 config 或 `prompt` 包 overlay 机制实现，不需要 struct 字段

### D10 · Execute 错误处理策略

`prompt` 包是内部包，模板在 `init` 时编译，data 形状由调用方保证。
Execute 返回 error 的唯一场景是 data 类型不匹配（如 `range` 收到 `string` 而非 `slice`）。

策略：**调用方处理 error**，不统一 panic。理由：
- `BuildSystemPrompt` 已有 fallback（返回 `base`）
- `FormatManifest` 已有 fallback（返回 `""`）
- 不同调用方对错误的容忍度不同（system prompt fallback 到 base 可以接受；
  compaction fallback 到 nil system 字段也可以接受）

`slog.Error` 记录不做——编译时模板 + Go 静态类型意味着 error 只在开发期出现，
运行时不应该发生。

## Open Questions

无。

## Compliance

- 不复制任何其他 agent runtime 的提示词文本
- `cancelledNote` 和 `Compaction` 提示词为项目原创
- `text/template` 是 Go 标准库，不引入外部依赖
- 不违反 CLAUDE.md 的 "no verbatim copies" 约束
