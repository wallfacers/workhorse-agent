## MODIFIED Requirements

### Requirement: 内置 5 工具

服务 SHALL 内置注册以下 5 个工具：

| 工具 | IsReadOnly | CanRunInParallel | 行为 |
|---|---|---|---|
| `Read` | true | true | 读 `path` 文件内容（支持 offset/limit/pages） |
| `Grep` | true | true | 在 `path` 下搜索 `pattern`（regex），返回匹配行；默认应用 `.gitignore` 与内置默认排除清单；并行 walker pool 执行；自动跳过二进制文件；输出按 (path, line) 字典序排序 |
| `Write` | false | false | 写 `content` 到 `path`（覆盖） |
| `Edit` | false | false | 在 `path` 中把 `old_string` 替换为 `new_string`（exact-match） |
| `Bash` | false（MVP 简化） | false | 在会话 workdir 中执行 `command`，最长 `timeout` 秒，返回 stdout+stderr |

所有工具 SHALL 强制路径在会话 workdir 内（或额外 allowed_paths 内），否则返回 `is_error: true`。**路径校验的具体算法**（filepath.Clean → EvalSymlinks → filepath.Rel → O_NOFOLLOW / Lstat 复检）由 `session-management` capability 的 "路径越界防护算法" requirement 定义；本 capability 的所有工具实现 SHALL 通过统一的 `internal/tools/pathguard` 模块调用该算法，不得自行实现。MCP 工具适配层、Skills 的 LoadSkill 注入工具、未来动态工具同样 SHALL 经过 pathguard。

#### Scenario: Edit 未找到 old_string

- **WHEN** 调用 `Edit { path, old_string: "foo", new_string: "bar" }`，文件中无 "foo"
- **THEN** 返回 `tool_result { is_error: true, output: "old_string not found in file" }`，不修改文件

#### Scenario: Bash 在会话 workdir 中执行

- **WHEN** 会话 workdir 为 `/tmp/x`，Bash 执行 `pwd`
- **THEN** 输出包含 `/tmp/x`

#### Scenario: Grep 默认尊重 .gitignore

- **WHEN** workdir 是 git 仓库，仓库根有 `.gitignore` 排除 `node_modules/`，调用 `Grep { pattern: "TODO" }`
- **THEN** 输出不含任何 `node_modules/` 下的匹配行

## ADDED Requirements

### Requirement: Grep 默认排除语义

`Grep` 工具在文件遍历时 SHALL 按以下优先级判定路径是否进入扫描队列：

1. **硬性排除**（最高优先级，不可被任何开关关闭）：basename 命中 `.git`、`.hg`、`.svn` 的目录 SHALL 跳过（`filepath.SkipDir`）
2. **`hidden=false`（默认）**：basename 以 `.` 开头的目录与文件 SHALL 跳过；`hidden=true` 时除上一条外照常进入
3. **`default_excludes` 硬性清单**：内置默认或 `tools.grep.default_excludes` 配置项指定的 basename 模式命中的目录 SHALL 跳过（即使 `ignore_vcs=false`）
4. **`.gitignore` 栈匹配**：当 `ignore_vcs=true`（默认）且 `tools.grep.respect_gitignore=true`（默认）且 workdir 处于 git 仓库内（向上找到 `.git/` 目录），SHALL 按 git 仓库根至当前目录的 `.gitignore` 栈匹配；命中 ignore 的目录或文件 SHALL 跳过；`input.ignore_vcs` 字段值优先于 config

内置的默认 `default_excludes` 清单 SHALL 包括但不限于：`node_modules`、`vendor`、`__pycache__`、`dist`、`build`、`target`、`out`、`.next`、`.nuxt`、`.turbo`、`.cache`、`.venv`、`*.lock`、`*.min.js`、`*.min.css`、`.DS_Store`。具体清单实现细节随版本演进；`tools.grep.default_excludes: [...]` 非空时 SHALL **完整替换**内置清单（不是追加）。

#### Scenario: ignore_vcs=false 退化 .gitignore

- **WHEN** workdir 在 git 仓库内，`.gitignore` 排除 `dist/`，调用 `Grep { pattern: "x", ignore_vcs: false }`
- **THEN** `dist/` 下的文件被扫描；但 `.git/` 目录仍跳过（硬性排除）

#### Scenario: default_excludes 覆盖

- **WHEN** `tools.grep.default_excludes: ["build"]`，workdir 含 `node_modules/` 与 `build/`
- **THEN** `build/` 跳过，`node_modules/` 进入扫描（因为完整替换而非追加）

#### Scenario: hidden 开关

- **WHEN** workdir 含 `.env` 与 `.config/foo.yaml`，调用 `Grep { pattern: "x", hidden: true }`
- **THEN** `.env`、`.config/foo.yaml` 进入扫描；`.git/` 仍跳过

### Requirement: Grep 二进制文件跳过

`Grep` 工具 SHALL 对每个候选文件读前 8 KiB（不足读完即可），若该段字节中存在 NUL 字节（`0x00`），SHALL 判定为二进制文件并跳过；二进制跳过 SHALL **不**计入"access error skipped" 计数，亦 SHALL **不**出现在输出末尾的 `[N entries skipped]` 提示中。

读已读的 8 KiB 字节 SHALL 复用为后续行扫描的输入（通过 `io.MultiReader` 等机制），SHALL **不**为非二进制文件触发二次读盘。

#### Scenario: 二进制文件不污染输出

- **WHEN** workdir 含 `foo.pdf`（含 NUL 字节），调用 `Grep { pattern: "x" }`
- **THEN** 输出不含来自 `foo.pdf` 的任何字节；输出末尾无 skipped 计数提示

### Requirement: Grep 并行执行与输出顺序

`Grep` 工具 SHALL 以并行 walker pool 模型执行：

1. 一个 walker goroutine 执行 `filepath.WalkDir` 并应用上述排除规则
2. N 个 worker goroutine 并发处理文件（打开 + binary 嗅探 + 行扫描 + regex 匹配）
3. N 取值 SHALL 优先按 `tools.grep.workers` 配置；`workers <= 0` 或缺省时 SHALL 取 `runtime.NumCPU()`；`workers > 256` SHALL 在启动时校验失败
4. **`workers=1` SHALL 走完整的串行 codepath**（不创建 channel / 不创建 goroutine 池），作为退化与调试用

无论并行或串行执行，`Grep` 工具的最终输出 SHALL 按 `(rel path 字典序, lineNo 升序)` 双键排序后序列化。同一文件内的命中 SHALL 连续出现且按 lineNo 升序。

并行执行下 `max_hits` 早停 SHALL 通过 `atomic.Int64` 计数 + 派生 ctx cancel 实现；允许略超 `max_hits`，最终输出 SHALL 裁剪到不超过 `max_hits` 行。

#### Scenario: 并行执行输出可复现

- **WHEN** 对固定 fixture 反复调用 `Grep { pattern: "TODO" }` 100 次
- **THEN** 每次返回的 `output` 字节完全相同

#### Scenario: workers=1 退化

- **WHEN** `tools.grep.workers: 1`，调用 `Grep`
- **THEN** 实现走串行 codepath（无 worker goroutine 启停），行为与并行模式输出完全一致

#### Scenario: max_hits 早停

- **WHEN** workdir 命中数远超 `max_hits=10`，调用 `Grep`
- **THEN** 输出行数 ≤ 10；剩余 worker 与 walker 在 ctx 取消后 100ms 内退出
