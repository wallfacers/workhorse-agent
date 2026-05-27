## 0. 迁移前 baseline 采集（必须在 task 2.1 之前完成）

- [x] 0.1 在当前 master 代码上，临时新建 `cmd/_baseline/main.go`（前缀 `_`
      避开默认 build），写入以下 snippet，跑 `go run ./cmd/_baseline`，
      记录两行 `%q` 输出后**删除整个目录**：
      ```go
      package main

      import (
          "fmt"

          "github.com/wallfacers/workhorse-agent/internal/agent"
      )

      func main() {
          fmt.Printf("%q\n", agent.BuildSystemPrompt(""))
          fmt.Printf("%q\n", agent.BuildSystemPrompt("You are a helper."))
      }
      ```
      把两个 `%q` 字面值复制到 task 5.1 创建的 `template_test.go` 的
      `baselineEmptyBytes` / `baselineWithBaseBytes` 常量中。

      **不采集 `skills.FormatManifest` 的 baseline**：本变更主动改了 footer
      语言（中文 → 英文，见 design D8 / proposal R4），迁移后输出必然与
      master 不等价；manifest 验证改为结构性断言（见 5.1
      `TestFormatManifest_EnglishFooter`）。

## 1. 新建 `internal/prompt` 包

- [x] 1.1 创建 `internal/prompt/doc.go`：包文档，内容包含：
      - 用途：集中管理所有 LLM 面向的提示词
      - 什么算"LLM 面向提示词"（发给 LLM 的 system prompt / 工具注入文本），
        什么不算（工具 Description/Schema、输出标记、CLI 提示）
      - 提示词语言标准：全部使用英文
      - 占位符命名风格：PascalCase（`{{.BasePrompt}}`、`{{.Name}}`）
      - 模板引擎：`text/template`（不使用 `html/template`，输出不是 HTML）
      - 安全约束：Execute 只接受基本类型 + `[]map[string]string`，不传结构体
      - 禁止 import 列表：`internal/agent`、`internal/skills`、`internal/tools`、
        `internal/config`、`internal/session`、`internal/coord`、`internal/provider`、
        `internal/api`、`internal/store`（防循环依赖）
- [x] 1.2 创建 `internal/prompt/template.go`：`Template` struct（私有 `name` + `tmpl`）、
      `MustParse(name, body string) *Template`（编译时 panic on error）、
      `Execute(data map[string]any) (string, error)`；import 仅 `bytes` + `text/template`
- [x] 1.3 创建 `internal/prompt/builtins.go`：
      - 常量 `CancelledToolOutput = "[CANCELLED] Tool execution was interrupted by user"`
      - 常量 `CompactionFallback = "(compaction summary unavailable)"`
      - 常量 `CancelledNote = "Note: if a tool_result begins with ..."` **不含前导 `\n\n`**
        （原 `agent/system_prompt.go:14` 的 `cancelledNote` 带前导 `"\n\n"`，
        在 prompt 包中去掉，由模板 `{{if .BasePrompt}}\n\n{{end}}` 控制）
      - 模板 `SystemPrompt`：`"{{.BasePrompt}}{{if .BasePrompt}}\n\n{{end}}" + CancelledNote`
      - 模板 `Compaction`：摘要器提示词，无占位符
      - 模板 `SkillManifest`：`<available_skills>` 包裹 + `{{range $s := .Skills}}` +
        footer 写死 "You can use the LoadSkill tool to load full instructions.\n"
      - 便捷函数 `BuildSystemPrompt(base string) string`：TrimRight + Execute + error fallback
- [x] 1.4 `go build ./internal/prompt/` 通过

## 2. 迁移 `agent` 包引用

- [x] 2.1 删除 `internal/agent/system_prompt.go`（前置：task 0 已完成 baseline 采集）
- [x] 2.2 修改 `internal/agent/loop.go`：
      - import 添加 `"github.com/wallfacers/workhorse-agent/internal/prompt"`
      - line 262/293/734：`CancelledToolOutput` → `prompt.CancelledToolOutput`
      - line 370：`BuildSystemPrompt(...)` → `prompt.BuildSystemPrompt(...)`
- [x] 2.3 修改 `internal/agent/compaction.go`：
      - import 添加 `"github.com/wallfacers/workhorse-agent/internal/prompt"`
      - 删除 `Compactor` struct 的 `SystemPrompt string` 字段（死字段，从未被读取）
      - line 105-108：内联字符串 → 如下（error 丢弃因编译时模板 + 无占位符不可能失败）：
        ```go
        // Compaction 模板编译时校验 + 无占位符，Execute 不可能失败；
        // error 路径仍兜底为空字符串，由 provider 处理空 System。
        sys, _ := prompt.Compaction.Execute(nil)
        ```
      - line 129：`"(compaction summary unavailable)"` → `prompt.CompactionFallback`
- [x] 2.4 `go build ./internal/agent/...` 通过

## 3. 迁移 `skills` 包引用

- [x] 3.1 修改 `internal/skills/injector.go`：
      - import 添加 `"github.com/wallfacers/workhorse-agent/internal/prompt"`
      - `FormatManifest` 内部改为：
        - 构造 `[]map[string]string`，**对每个 trigger 做
          `strings.Join(strings.Fields(s.Trigger), " ")` 折叠空白**
        - 调用 `prompt.SkillManifest.Execute(map[string]any{"Skills": items})`
      - 签名不变，error fallback 返回 `""`
- [x] 3.2 `go build ./internal/skills/...` 通过

## 4. 接线 FormatManifest 到 system prompt

- [x] 4.1 修改 `cmd/workhorse-agent/cmd_serve.go` 的 `newRunnerFactory`：
      - 将 `skillCatalog` 传入闭包（已在 `cmd_serve.go:81` 创建）
      - 在 `loop.SystemPromptBase = sess.SystemPromptBase` 之后追加：
        ```go
        manifest := skills.FormatManifest(skillCatalog)
        if manifest != "" {
            if loop.SystemPromptBase != "" {
                loop.SystemPromptBase += "\n\n"
            }
            loop.SystemPromptBase += manifest
        }
        ```
      - 注意：不能写 `+= "\n\n" + manifest`，否则空 base 时会产生前导 `\n\n`
- [x] 4.2 `go build ./cmd/...` 通过

## 5. 测试

- [x] 5.1 创建 `internal/prompt/template_test.go`，使用 task 0 采集的两组 baseline 常量：
      - `baselineEmptyBytes` — `BuildSystemPrompt("")` 的原始输出
      - `baselineWithBaseBytes` — `BuildSystemPrompt("You are a helper.")` 的原始输出

      SkillManifest 测试用 `cat`：
      ```go
      cat := &skills.Catalog{Skills: []skills.Skill{
          {Name: "alpha", Trigger: "run alpha"},
          {Name: "beta",  Trigger: "run beta"},
      }}
      ```

      测试用例：
      - `TestSystemPrompt_EmptyBase`：空 base → 无前导换行的 CancelledNote
      - `TestSystemPrompt_WithBase`：有 base → `base + "\n\n" + CancelledNote`
      - `TestSystemPrompt_TrailingWhitespace`：trailing 空白被 TrimRight
      - `TestCompaction_Render`：无占位符直接输出，内容完整
      - `TestSkillManifest_Multiple`：多条技能列表，含 name + trigger
      - `TestSkillManifest_Single`：单条技能
      - `TestSkillManifest_EmptyData`：空 Skills 列表 → 只有标签壳 + footer
      - `TestFormatManifest_EnglishFooter`：断言输出含
        `"You can use the LoadSkill tool to load full instructions."`，
        且**不含**中文字符串 `"可以调用"`（防止 D8 的语言迁移退化）
      - `TestSSTIImmunity`：data 中含 `{{evil}}` 不被模板解析
      - `TestMustParse_InvalidPanic`：非法模板 body → panic
      - `TestBuildSystemPrompt_Equivalence`：用 `baselineEmptyBytes` 和
        `baselineWithBaseBytes` 逐字节断言
      - `TestCancelledMarkerConsistency`：验证 `CancelledToolOutput` 以
        `"[CANCELLED]"` 开头，且 `CancelledNote` 包含 `` `[CANCELLED]` ``
        （两个常量共享同一 marker token 的 invariant）
- [x] 5.2 `go test ./internal/prompt/...` 全绿
- [x] 5.3 `go test ./internal/agent/...` 全绿（现有测试不受影响）
- [x] 5.4 `go test ./internal/skills/...` 全绿
      - `TestFormatManifest_TriggerWithNewlines` 验证 trigger 空白折叠生效
      - `TestFormatManifest_TwoSkills` 的 `LoadSkill` 断言对英文 footer 仍然通过
- [x] 5.5 `go test ./cmd/workhorse-agent/...` 全绿（`main_test.go` 含 6 个测试）

## 6. spec 更新

- [x] 6.1 `specs/agent-loop/spec.md` 补充 `## ADDED Requirements`：
      系统提示词由 `internal/prompt` 包管理，agent-loop 通过 `prompt.BuildSystemPrompt` 获取
- [x] 6.2 `specs/skills-loader/spec.md` 补充 `## ADDED Requirements`（技能清单模板由 prompt 包管理）
      + `## MODIFIED Requirements`（"System prompt 中暴露 skill 清单" 标题逐字匹配，
      body 内说明接线点和空 base 处理）
- [x] 6.3 新建 `specs/prompt-module/spec.md`：`## ADDED Requirements` 描述
      Template 类型 + MustParse/Execute 的 API 契约、安全约束、内置三个模板、
      提示词语言标准（英文）、禁止 import 列表

## 7. 验证

- [x] 7.1 `go build ./...` 通过
- [x] 7.2 `go test -race ./...` 全绿
- [x] 7.3 `golangci-lint run` 通过（无新 warning）
- [x] 7.4 手动验证：启动服务，创建带 skill 的会话，确认 system prompt 中
      出现 `<available_skills>` 块，且无多余前导空行
