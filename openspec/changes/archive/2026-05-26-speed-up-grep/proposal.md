## Why

当前 `internal/tools/builtin/grep.go` 是单线程 `filepath.WalkDir`，无 `.gitignore`
感知、无二进制识别。在生产规模仓库（典型 Node 项目 ~30k 文件，其中 ~25k 位于
`node_modules`/`dist`/`.git`）上的真实表现：

- 95%+ 的 I/O 花在 LLM agent 几乎从不关心的目录上
- 1GB / 30k 文件量级的简单 `pattern` 搜索逼近 60s 超时上限
- 偶尔会把二进制文件的非 UTF-8 字节拖进 `tool_result.output`，污染 LLM 上下文

性能瓶颈不在 RE2 引擎，而在"读了不该读的文件"。本变更针对这一根因，目标是在
单 binary / 无 CGO / 无外部运行时依赖 的现有约束下，把典型大仓搜索从 ~40s
压到 ~3s 量级。

## What Changes

针对 `Grep` 工具的三项增强，均在 `internal/tools/builtin/grep.go` 内完成，
不引入 CGO，不 shellout 到外部 `rg`：

- **gitignore 感知 + 默认排除清单**：从 workdir 向上找最近的 `.git/` 目录定位
  仓库根，按目录栈式继承 `.gitignore`/`.git/info/exclude`；外加硬编码默认排除
  清单 (`node_modules/`、`dist/`、`*.lock`、`.DS_Store` 等)，对 LLM agent 场景下
  几乎永远无意义的路径直接 `SkipDir`。
- **并行 walker pool**：walker goroutine 产出已过滤的文件路径到 channel，
  N 个 worker goroutine 并发执行"打开 + binary 嗅探 + 行扫描 + regex"；
  N 默认 `GOMAXPROCS`，可由 `tools.grep.workers` 覆盖，`workers=1` 完整跑串行
  codepath 作为退化路径。
- **二进制嗅探**：读每个文件前 8KiB，遇 NUL 字节即跳过且不计入访问错误。

输入面新增两个开关：`ignore_vcs` (默认 `true`)、`hidden` (默认 `false`)。

输出格式保持 `path:line:content`，但顺序由"walk 隐式顺序"变为 "(path, line)
字典序"，以保证并行实现下的可复现性。

明确**不做**：

- 不引入 PCRE2 / lookbehind / backref —— 这要么需要 CGO（违反 CLAUDE.md
  no-CGO 红线），要么改用回溯引擎（如 `dlclark/regexp2`），后者对恶意 regex
  缺乏线性时间保证，会被 ReDoS 打爆 60s 超时
- 不引入 SIMD 字面量预过滤 —— Go 缺 Teddy，且标准 `regexp` 已对字面量前缀
  走 Boyer-Moore，差距对 agent 场景非主导
- 不切换到 `charlievieth/fastwalk`、不自写 newline 扫描替代 `bufio.Scanner` ——
  这两项额外提升 ~1.5-2x，但与本变更解耦，留 Phase 2 单独 propose
- 不 shellout 到系统 `rg` —— LLM 已可通过 Bash 工具直接调 `rg`；把 rg 包到
  Grep 内部会引入运行时外部依赖，破坏 README 顶部 "Single binary" 定位
- 不实现 `-A/-B/-C` 上下文行、不支持多模式 `-e` —— 不属于性能议题，本期不进

## Capabilities

### Modified Capabilities

- `tool-system`: `Grep` 工具能力扩展（gitignore 语义、并行执行、binary 嗅探、
  输出排序保证）；输入 schema 新增字段；不影响其他 4 个内置工具
- `configuration`: `tools.grep` 配置块新增 3 个键（`workers`、`respect_gitignore`、
  `default_excludes`）；缺省值保证现有 yaml 仍可正常加载

### New Capabilities

无

## Impact

- **新增代码**：约 400-600 行 Go（含测试），主体在 `internal/tools/builtin/grep.go`
  + 一个 walker 兄弟文件；`internal/config` 扩 ~30 行
- **vendored 代码**：`go-git/v5/plumbing/format/gitignore` 的 `pattern.go` +
  `matcher.go` + `doc.go` + 4 个测试文件，共约 900 行（含测试），落在
  `internal/tools/builtin/gitignore/`，附 LICENSE + NOTICE。理由与 vendor
  路径选择见 `design.md` D1
- **新增 go.mod 依赖**：0（gitignore 解析采用 vendor 方式，不通过 go.mod 引入）
- **工期估算**：1-1.5 周（单人）；任务清单见 `tasks.md`
- **行为变更**：
  - Grep 默认不再搜 `.gitignore` 排除的路径与 `node_modules/` 等 ——
    属于"过去能搜到的现在搜不到"的不向后兼容行为变化，但符合 agent
    场景下绝大多数用户的真实期望；`ignore_vcs=false` 提供退化开关
  - 输出顺序从"walk 顺序"变为 "(path, line) 字典序" —— 仅影响测试断言，
    无外部消费者依赖该顺序
- **性能预期**（1GB / 30k 文件 monorepo 量级）：
  - 单 ① gitignore 生效：~5-10x
  - ① + ③ 并行：~10-20x（典型 60s 超时 → 3-6s）
- **合规**：`.gitignore` 语法是 git 公开文档（`gitignore(5)`），等同 MCP/SSE
  公开协议档；本变更**不**从 ripgrep 的 `ignore` crate 复制任何代码/常量/
  测试用例；默认排除清单根据 LLM agent 场景重新拟定，非照抄 ripgrep 默认
- **风险**：见 `design.md` 的 `Risks` 章节
