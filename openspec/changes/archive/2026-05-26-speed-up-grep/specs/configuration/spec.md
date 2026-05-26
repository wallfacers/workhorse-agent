## ADDED Requirements

### Requirement: Grep 工具配置项

`config.yaml` 的 `tools.grep` 配置块 SHALL 支持以下键：

| 键 | 类型 | 默认 | 含义 |
|---|---|---|---|
| `timeout_seconds` | int | 60 | （已有）`Grep` 工具执行超时 |
| `workers` | int | 0 | 并行扫描 goroutine 数；`0` 表示 `runtime.NumCPU()`；`1` 走串行 codepath |
| `respect_gitignore` | bool | true | 是否应用 `.gitignore` 栈过滤；input `ignore_vcs` 字段优先级更高 |
| `default_excludes` | []string | `null` | basename glob 模式数组；`null` 或空时使用内置硬编码清单；非空时**完整替换**内置清单（不是追加） |

校验 SHALL 在启动时进行：

- `workers ∈ [0, 256]`，越界 SHALL 启动失败并给出明确错误
- `default_excludes` 每条 SHALL 通过 `path.Match` 的 dry-run 解析；非法 pattern SHALL 启动失败

#### Scenario: workers 配置生效

- **WHEN** `config.yaml` 中 `tools.grep.workers: 4`
- **THEN** Grep 用 4 个 worker goroutine 执行

#### Scenario: workers 越界

- **WHEN** `config.yaml` 中 `tools.grep.workers: 1000`
- **THEN** `workhorse-agent serve` 启动失败，stderr 提示 `workers must be in [0, 256]`

#### Scenario: default_excludes 完整替换

- **WHEN** `config.yaml` 中 `tools.grep.default_excludes: ["only_this"]`，workdir 含 `only_this/`、`node_modules/`、`dist/`
- **THEN** Grep 跳过 `only_this/`，扫描 `node_modules/` 与 `dist/`（因为完整替换而非追加）

#### Scenario: 非法 default_excludes 启动失败

- **WHEN** `config.yaml` 中 `tools.grep.default_excludes: ["[bad"]`（非法 glob）
- **THEN** `workhorse-agent serve` 启动失败，stderr 提示该条 pattern 非法
