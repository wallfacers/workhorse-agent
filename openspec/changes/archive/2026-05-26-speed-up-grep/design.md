## Context

`Grep` 工具是内置 5 工具之一（`tool-system` capability），spec 定义见
`openspec/specs/tool-system/spec.md` 的"内置 5 工具"requirement。
当前实现位于 `internal/tools/builtin/grep.go`（154 行），是单线程 `WalkDir`
+ `bufio.Scanner` + `regexp.Compile` 的朴素组合。

在 `accelerate-grep-with-ignore-and-parallel` 这次性能优化的范围内，要回答
五个技术问题：用什么库解析 gitignore、栈如何继承、worker pool 怎么编排、
binary 怎么嗅探、输出怎么保证可复现。下面逐条决策。

## Goals / Non-Goals

**Goals:**

- 在不引入 CGO、不 shellout、不破坏 single-binary 的前提下，把典型大仓
  Grep 时间砍到 1/10 数量级
- 保持 `Grep` 的 `IsReadOnly=true / CanRunInParallel=true` 不变
- 所有文件 I/O 仍走 `internal/tools/pathguard` 单点
- 行为可通过 input 字段（`ignore_vcs`、`hidden`）和 config（`tools.grep.*`）
  双向覆盖

**Non-Goals:**

- 不在本次实现 PCRE2 / lookbehind / backref
- 不切换底层 walker 到 `fastwalk`、不替换 `bufio.Scanner`（Phase 2）
- 不实现上下文行、多模式、文件类型别名（独立 propose）
- 不增加对 `.workhorseignore`、`~/.gitignore_global`、`core.excludesFile` 的支持
  （MVP 范围外；如未来需要再扩）

## Decisions

### D1 · gitignore 解析:vendor `go-git/v5` 的 `pattern.go` + `matcher.go`

候选库实测（数据采集于 2026-05-26，逐项核验 GitHub 仓库状态与 open issues）：

| 库 | 最后代码 commit | conformance 测试 | 已知未修语义 bug |
|---|---|---|---|
| `denormal/go-gitignore` | 2018-09-30 (已 8 年无 commit) | 无 | — |
| `sabhiram/go-gitignore` | 2021-09-23 (5 年无实质 commit) | ~9 KB pattern_test | #21 `*` 跨 `/` 错配 / #20 `?` 不工作 |
| `go-git/v5/plumbing/format/gitignore` | 整库 2026-05 仍活跃 | 35 KB `conformance_test.go` | 无可见 |

`denormal/go-gitignore` 实质死亡；`sabhiram/go-gitignore` 在 gitignore 两个核心
wildcards (`*` 跨 `/`、`?`) 上有 open 多年未修的正确性 bug，用在 grep 工具里
会让 LLM 拿到静默错的搜索结果。

`go-git/v5` 的 `plumbing/format/gitignore` 子包是合规、正确、维护好的选项：

- `pattern.go` (13 KB) + `matcher.go` (0.9 KB) **仅 import `strings`**，
  无 go-git 内部依赖、无 go-billy 文件系统抽象
- API 干净：
  ```go
  gitignore.ParsePattern(line string, domain []string) Pattern
  gitignore.NewMatcher(patterns []Pattern) Matcher
  matcher.Match(path []string, isDir bool) bool  // true = 应排除
  ```
- 35 KB conformance test 已覆盖 `?` 通配、negation、dir-only `foo/`、
  `**`、leading `/`、FNM_PATHNAME（`*` 不跨 `/`）等关键语义
- Apache-2.0，允许重分发，保留 LICENSE + NOTICE 即可

**决策：vendor 进 `internal/tools/builtin/gitignore/`，不通过 go.mod import**。

vendor 而非直接 import 的理由：

1. **`go.sum` 干净**：直接 `import "github.com/go-git/go-git/v5/plumbing/format/gitignore"`
   会把 go-git 整库的 ~20-30 个间接依赖塞进 `go.sum`。dead-code elimination
   保证最终 binary 无差异，但 go.sum 视觉膨胀与本项目"无外部依赖"气质冲突
2. **包稳定**：gitignore 协议本身不动，该子包近 5 年改动稀疏，vendor 后维护
   成本接近 0
3. **气质一致**：本项目已手写 anthropic / openai HTTP 客户端来回避 vendor SDK；
   vendor 一个解析库 fits the same ethos
4. **可定制**：将来如需扩（例如加 `.workhorseignore` 语义），直接改 vendor
   下源，不必 fork upstream

vendor 内容布局（采集自 `v5.19.1`，commit `3c3be601aa6c`，日期 2026-05-26）：

```
internal/tools/builtin/gitignore/
├── pattern.go             ← v5.19.1 plumbing/format/gitignore/pattern.go 原样 (3.2 KB)
├── matcher.go             ← 同上 matcher.go (934 B)
├── doc.go                 ← 同上 doc.go (含 gitignore(5) 全文 3.5 KB)
├── LICENSE                ← Apache-2.0 全文 (从 go-git v5.19.1 仓库根)
└── NOTICE                 ← 上游 repo URL + tag + commit SHA + 日期 +
                              文件清单 + "no modifications" 声明
```

**不 vendor 的文件 + 原因**：

- `dir.go` / `dir_test.go` —— 依赖 `go-billy` 文件系统抽象与多个 go-git 内部包，
  与本项目的 walker 模型不兼容；walker 我们自己写（见 D2-D4）
- `pattern_test.go` / `matcher_test.go` —— upstream 测试用 `gopkg.in/check.v1`
  (gocheck)，vendor 整套会反向把 gocheck 拖进 go.mod，**正好违背 vendor 初衷**。
  实际覆盖由本变更在 `internal/tools/builtin/gitignore_walker_test.go` 用
  stdlib `testing` 重写 sabhiram bug 触发场景 + go-git doc 中列出的 gitignore
  语义条款

> 注：v6（开发中）的 `gitignore` 子包含 35 KB 的 `conformance_test.go`，但 v6
> 仍在 alpha；v5.19.1 是当前唯一稳定 tag，不带该文件。我们用自写 stdlib 测试
> 覆盖 sabhiram 已知失误的语义 (`*` 跨 `/`、`?` 通配)，加上 dir-only 和
> negation，作为 vendor 后的正确性基线。

**合规说明**：Apache-2.0 是兼容 license，vendor + NOTICE 是其明确允许的使用
方式。该 vendor **不**违反 CLAUDE.md 的"no verbatim copies, no transliteration"
红线 —— 那条红线针对的是从其它 agent runtime（特指 Claude Code）复制；
go-git 是 git 协议的纯 Go 实现，与 agent runtime 无关，其实现本身也直接源自
git 公开文档（`gitignore(5)`）。

### D2 · 仓库根定位与 gitignore 栈

```
workdir = /home/x/project/sub/dir
                       ↑
              寻找 .git/ 时从这往上找
                       ↓
找到 /home/x/project/.git/ → 仓库根 = /home/x/project
找不到（workdir 不在 git 仓库内） → 跳过 .gitignore，只用 default_excludes
```

walker 在下钻每一层目录时维护一个 `[]*gitignore.GitIgnore` 栈：

```
仓库根         .gitignore  push 栈
仓库根/sub     .gitignore  push 栈
仓库根/sub/dir .gitignore  push 栈
...
退栈时机：filepath.WalkDir 的 fs.DirEntry 回到上一层（用进出 directory 标记）
```

匹配算法：候选路径 `p` 命中的判定 = 从栈底（仓库根）到栈顶（最近父目录）依次
匹配；后写的规则覆盖先写的（gitignore 标准语义，negation `!pattern` 受此规则
约束）。匹配命中且最终为 "ignore"，则该文件/目录不进 worker 队列。

**目录命中 ignore → `filepath.SkipDir`**，避免再下钻一层才发现子文件都被排除。

### D3 · 硬性默认排除清单

不依赖 `.gitignore`，无条件 `SkipDir`：

```
.git/  .hg/  .svn/                       # VCS 元数据（Q3 决策）
node_modules/  vendor/  __pycache__/     # 常见包目录
dist/  build/  target/  out/             # 构建产物
.next/  .nuxt/  .turbo/  .cache/  .venv/ # 框架/工具缓存
.gradle/                                 # Gradle 缓存（体量大）
.mypy_cache/  .pytest_cache/  .ruff_cache/  .parcel-cache/  .tox/  # Python/JS 工具缓存
coverage/  htmlcov/                      # 覆盖率报告 HTML
__snapshots__/                           # Jest 快照（自动生成）
```

文件类硬排除（按 basename glob）：

```
*.lock                                   # Gemfile.lock / Cargo.lock / poetry.lock / yarn.lock / composer.lock
package-lock.json  pnpm-lock.yaml        # 不被 *.lock 覆盖（扩展名是 .json / .yaml）
*.min.js  *.min.css                      # minified artifacts，命中即灾难性长行
.DS_Store
```

注意：

- 二进制（`.pyc`/`.so`/`.pdf`/`.png` 等）**不**靠扩展名排除，靠 D5 的 NUL 嗅探
- 该清单**重新拟定**，不照抄 ripgrep 内置 `--type` 表，且通过 `default_excludes`
  config 可整体替换或扩展

`input.ignore_vcs=false` **仍不**取消 `.git/`、`.hg/`、`.svn/` 三个硬排除
（Q3 决策；理由：这三个目录在 agent 上下文几乎从无搜索意义，且 `.git/objects`
体量极大、命中是噪音）。

### D4 · Worker pool 拓扑

```
                   ┌───────────────────────┐
                   │  walker goroutine     │
                   │  - WalkDir            │
                   │  - gitignore 栈匹配   │
                   │  - default_excludes   │
                   │  - pathguard 一次性 resolve
                   └────────────┬──────────┘
                                │  abs path
                                ▼  (buffered chan, cap=256)
   ┌─────┐  ┌─────┐  ┌─────┐  ┌─────┐
   │ W₁  │  │ W₂  │  │ W₃  │  │ W_N │   N = config tools.grep.workers
   └──┬──┘  └──┬──┘  └──┬──┘  └──┬──┘   每个 worker:
      │       │       │        │           - open
      │       │       │        │           - binary sniff (D5)
      │       │       │        │           - scan + re.Match
      └───────┴───┬───┴────────┘           - 本地 []hit 缓冲
                  ▼
            results chan (worker 退出前一次性 send 自己的 []hit)
                  ▼
            aggregator goroutine:
            - 收集所有 hits
            - atomic.Int64 全局计数；达 max_hits 时 cancel 派生 ctx
            - 最终 sort by (path, line)
            - 序列化为 path:line:content 字符串
```

**cancel 流**：

- session ctx 由 orchestrator 传入 `Run`
- 内部 `ctx, cancel := context.WithCancel(parentCtx)` 派生
- max_hits 达到 → aggregator 调 `cancel()` → walker 与所有 worker 在
  下一次 channel 操作前的 `ctx.Done()` 检查处退出
- 设计上不要求"恰好 max_hits 命中后立即停"——可能略超过，最终切片裁剪到
  max_hits

**`workers=1` 退化**：完整保留串行 codepath（不通过 channel）。理由：
race detector 跑测试时单 worker 更干净；线上偶发 race 调查方便回滚。

### D5 · Binary 嗅探

```go
const sniffSize = 8 << 10  // 8 KiB

func sniffBinary(f *os.File) (firstChunk []byte, isBinary bool, err error) {
    buf := make([]byte, sniffSize)
    n, err := io.ReadFull(f, buf)
    if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
        return nil, false, err
    }
    chunk := buf[:n]
    return chunk, bytes.IndexByte(chunk, 0) >= 0, nil
}
```

scanner 初始化时把已读的 `firstChunk` 通过 `io.MultiReader(bytes.NewReader(chunk), f)`
拼接回去，避免重读。

跳过的二进制文件**不**计入"access error skipped"计数（与权限错误等区分）。

### D6 · 输出顺序

并行执行下 worker 完成顺序非确定。最终结果按
`sort.SliceStable(hits, func(i,j int) bool { ... })` 排序：

1. 主键：`rel path` 按 byte-wise 字典序
2. 次键：`lineNo` 升序

单文件内的 `lineNo` 已经升序（scanner 顺序），跨文件靠 sort 收尾。
开销 O(n log n)，n ≤ max_hits = 500，可忽略。

输出形态不变：仍是每行 `path:line:content`，结尾换行。

**测试侧影响**：`internal/tools/builtin/builtin_test.go` 中依赖隐式 walk 顺序
的断言需改为对排序后的输出做断言。属内部测试调整，不构造迁移层。

### D7 · 配置 schema 扩展

`internal/config/config.go` 的 `ToolsGrep` 结构新增：

```go
type ToolsGrep struct {
    TimeoutSeconds   int       `yaml:"timeout_seconds"`       // 已有
    Workers          int       `yaml:"workers"`               // 新；0=GOMAXPROCS
    RespectGitignore bool      `yaml:"respect_gitignore"`     // 新；默认 true
    DefaultExcludes  []string  `yaml:"default_excludes"`      // 新；nil=用内置
}
```

校验（`internal/config/validate.go`）：

- `Workers >= 0`，0 或缺省时 runtime 取 `runtime.NumCPU()`
- `Workers <= 256`（防御上限；超出报错启动失败）
- `DefaultExcludes` 不校验内容（让 user 自由），但对每条用 `path.Match`
  做一次 dry-run 解析，非法 pattern 启动失败

`config.yaml` 默认模板增加：

```yaml
tools:
  grep:
    timeout_seconds: 60
    workers: 0                  # 0 = runtime.NumCPU()
    respect_gitignore: true
    default_excludes: []        # 空 = 用内置硬编码清单；非空 = 完整替换
```

**input.ignore_vcs vs config.respect_gitignore 的优先级**：

- input 显式 false → 跳过 gitignore（最强）
- input 缺省 + config.respect_gitignore=false → 跳过
- 否则 → 应用 gitignore

input 字段优先于 config，符合"一次性请求覆盖全局"的直觉。

## Risks

- **R1 / 高 / gitignore 库 corner case** —— `denormal/go-gitignore` 不维护或
  发现 `**` / dir-only / negation 处理 bug。
  *缓解*：在 task 1.1 评估时构造 20 个 gitignore 边界用例的测试套，库不过关
  立即换 `sabhiram/go-gitignore` + 自补 dir-only 逻辑。

- **R2 / 中 / 并行实现引入 race** —— 共享 atomic 计数、ctx cancel、results
  channel 关闭顺序。
  *缓解*：所有相关测试 `go test -race`；workers=1 退化路径作为安全网。

- **R3 / 中 / 行为不兼容** —— 用户脚本中依赖"能搜到 node_modules 内容"。
  *缓解*：`input.ignore_vcs=false` + `tools.grep.respect_gitignore=false`
  + `tools.grep.default_excludes: []` 三层都能退化；CHANGELOG 显著注明。

- **R4 / 低 / 内存放大** —— N=GOMAXPROCS 时每 worker 1MB scanner 缓冲 +
  本地 hits 切片。在 16-core 机器上 ~16MB，可接受。
  *缓解*：监控；如有问题可改 sync.Pool 复用 scanner 缓冲。

- **R5 / 低 / 默认排除清单争议** —— 拟定的清单可能某些项目误伤
  （e.g. 有些 monorepo 真的需要搜 `dist/` 里的 generated code）。
  *缓解*：`default_excludes: ["..."]` 可整体替换；文档给出示例。

## Open Questions

无。Q1-Q5 + ②(binary skip) 在 explore 阶段全部锁定，决策记录于本文 D1-D7。

## Compliance

- 不复制 ripgrep 源码、`ignore` crate、`grep-matcher` crate 的任何代码、
  常量、测试用例
- 默认排除清单根据 LLM agent 场景重新拟定，对照 ripgrep 默认 `--type` 表
  确保**不**字面雷同
- `.gitignore` 语法实现引用 `man gitignore`（gitignore(5)）公开规范，
  等同 MCP/SSE 公开协议档
- 无 CGO 引入；无 vendor SDK；持久化不动
- pathguard 仍是文件 I/O 的单一入口，并行实现的 walker 侧 resolve 后传
  绝对路径给 worker，不在 worker 侧再次 resolve
