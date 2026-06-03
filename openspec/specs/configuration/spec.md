# configuration Specification

## Purpose
TBD - created by archiving change init-workhorse-agent-mvp. Update Purpose after archive.
## Requirements
### Requirement: 配置文件位置与加载顺序

服务 SHALL 按以下优先级合并配置（高优先级覆盖低）：

1. 命令行参数（最高，如 `--port 8000`）
2. 环境变量（前缀 `DATAAGENT_`，如 `DATAAGENT_PORT=8000`）
3. 用户配置文件 `~/.workhorse-agent/config.yaml`
4. 内置默认值（最低）

`workhorse-agent init` 命令 SHALL 在 `~/.workhorse-agent/config.yaml` 不存在时交互式生成该文件。

#### Scenario: 环境变量覆盖配置文件

- **WHEN** `config.yaml` 中 `port: 7821`，启动时设 `DATAAGENT_PORT=9000`
- **THEN** 服务绑定 `127.0.0.1:9000`

#### Scenario: 命令行覆盖环境变量

- **WHEN** 同时设 `DATAAGENT_PORT=9000` 和命令行 `--port 8000`
- **THEN** 服务绑定 `127.0.0.1:8000`

`config.yaml` SHALL 遵循以下 schema：

```yaml
# === 服务端配置 ===
server:
  host: 127.0.0.1                  # 默认 127.0.0.1；非 localhost 时 Origin 缺失校验更严
  port: 7821                       # 1-65535
  read_header_timeout_seconds: 10
  read_timeout_seconds: 60
  idle_timeout_seconds: 120
  max_header_bytes: 1048576        # 1 MiB
  max_request_body_bytes: 1048576  # 1 MiB；POST body 上限，超限返 413
  graceful_shutdown_timeout_seconds: 30
  sse_keepalive_seconds: 25
  allowed_origins:                 # 精确 origin 字符串列表，扩展默认白名单
    - http://localhost:5173
  allow_null_origin: false         # 是否允许 Origin: null（sandboxed iframe）

# === 鉴权 ===
auth:
  enabled: false                   # 默认 false（本地单用户）
  bearer_token: ""                 # enabled=true 时必填；建议 32+ 字节随机；不写日志

# === LLM Provider ===
providers:
  default: anthropic               # "anthropic" | "openai"
  anthropic:
    api_key: ""                    # 必填（若使用）；可通过 env DATAAGENT_PROVIDERS_ANTHROPIC_API_KEY
    base_url: https://api.anthropic.com
    fast_model: claude-haiku-4-5-20251001
  openai:
    api_key: ""
    base_url: https://api.openai.com/v1
    fast_model: gpt-4o-mini

# === 模型策略 ===
models:
  default: anthropic:claude-sonnet-4-6
  fast: anthropic:claude-haiku-4-5-20251001
  by_session_type: {}              # agent_type → "provider:model"

# === Agent 行为 ===
agent:
  max_parallel_tools: 10           # 并发批信号量上限
  max_depth: 5                     # 子 agent 嵌套深度上限
  auto_compact_ratio: 0.85         # token 阈值触发压缩
  compact_recent_keep: 8           # 压缩时保留最近 K 条
  max_history_tokens: 200000       # 单 session history 硬上限；超过拒绝新 user_message
  permission_request_timeout_seconds: 300  # 5 分钟无决策视为 deny
  cancel_drain_timeout_seconds: 5          # 取消后等待工具/子 session 收尾的上限
  provider_retry_attempts: 3
  provider_retry_backoff_ms: [500, 2000, 8000]

# === 工具 ===
tools:
  default_timeout_seconds: 60      # 工具执行全局超时（除 Bash 用自己的）
  tool_result_max_bytes: 1048576   # 单个 tool_result.output 最大字节（1 MiB）；超出自我截断 + 截断标记
  bash:
    timeout_seconds: 120           # 单次 Bash 命令默认超时
  read:
    timeout_seconds: 30            # 可选覆盖；缺省取 DefaultTimeout
  grep:
    timeout_seconds: 60
  default_allowed_tools: []        # 全局工具白名单；空表示全部启用
  default_permission: ""           # 无规则匹配时的默认决策；空=弹窗，allow_permanent | deny_permanent
  preset_rules: []                 # 启动时注入的永久规则列表
    # - tool: "Bash"               # 工具名；空="*"=所有工具
    #   pattern: "git *"           # glob 模式；空=所有 resource
    #   decision: allow_permanent  # allow_permanent | deny_permanent

# === 持久化 ===
store:
  path: ~/.workhorse-agent/state.db
  busy_timeout_ms: 5000

# === 会话上限 ===
sessions:
  max_concurrent: 50               # 同时活跃 session 数；超过 POST /v1/sessions 返回 429

# === MCP ===
mcp:
  config_path: ~/.workhorse-agent/mcp.json

# === Skills / Agent Roles ===
skills:
  dir: ~/.workhorse-agent/skills
agents:
  dir: ~/.workhorse-agent/agents

# === 日志 ===
logging:
  level: info                      # debug | info | warn | error
  format: json                     # json | text
  log_llm_payload: false           # debug 模式才打 LLM full request/response

# === 心跳/调试 ===
debug:
  enabled: false                   # true 时启用 /debug/* 端点
```

所有数值字段 SHALL 在加载时校验：负数、零、超出合理范围 SHALL 拒绝启动并打印明确错误。下表列出关键字段的合法范围：

| 字段 | 合法范围 | 拒绝原因（举例） |
|---|---|---|
| `server.port` | 1-65535 | port>65535 或 <=0 |
| `server.read_header_timeout_seconds` | 1-60 | 0 会立即超时所有请求 |
| `server.read_timeout_seconds` | 5-3600 | 过短拒大 body；过长助长 slowloris |
| `server.idle_timeout_seconds` | 10-3600 | - |
| `server.max_header_bytes` | 4096-16777216 | 4KB-16MB |
| `server.max_request_body_bytes` | 1024-104857600 | 1KB-100MB |
| `server.graceful_shutdown_timeout_seconds` | 1-600 | - |
| `server.sse_keepalive_seconds` | 5-300 | <5 心跳过频浪费带宽；>300 代理可能因无数据断连 |
| `agent.max_parallel_tools` | 1-100 | 0 阻塞所有工具 |
| `agent.max_depth` | 1-20 | 过深递归触发栈/资源问题 |
| `agent.auto_compact_ratio` | 0.5-0.99 | <0.5 过早压缩损失上下文；>=1 永不触发 |
| `agent.compact_recent_keep` | 1-100 | - |
| `agent.max_history_tokens` | 1000-10000000 | - |
| `agent.permission_request_timeout_seconds` | 5-3600 | - |
| `agent.cancel_drain_timeout_seconds` | 1-60 | <1 收尾流程必然超时；>60 用户感知延迟过大 |
| `tools.default_timeout_seconds` | 1-3600 | - |
| `tools.tool_result_max_bytes` | 1024-104857600 | 1KB-100MB |
| `tools.bash.timeout_seconds` | 1-3600 | - |
| `tools.default_permission` | "" / `allow_permanent` / `deny_permanent` | 其他值拒绝 |
| `tools.preset_rules[].decision` | `allow_permanent` / `deny_permanent` | 其他值拒绝 |
| `sessions.max_concurrent` | 1-10000 | - |

#### Scenario: 非法端口拒绝启动

- **WHEN** `server.port: 70000`
- **THEN** 启动失败，stderr 输出 `invalid config: server.port must be 1-65535, got 70000`

#### Scenario: enabled=true 但 token 为空

- **WHEN** `auth.enabled: true` 但 `auth.bearer_token: ""`
- **THEN** 启动失败，stderr 输出 `invalid config: auth.bearer_token must be set when auth.enabled is true`

#### Scenario: sse_keepalive_seconds 超出范围

- **WHEN** `server.sse_keepalive_seconds: 0`
- **THEN** 启动失败，stderr 输出 `invalid config: server.sse_keepalive_seconds must be 5-300, got 0`

#### Scenario: default_permission 非法值拒绝启动

- **WHEN** `tools.default_permission: allow_once`
- **THEN** 启动失败，stderr 输出 `invalid config: tools.default_permission must be empty, allow_permanent, or deny_permanent, got allow_once`

#### Scenario: preset_rules decision 非法值拒绝启动

- **WHEN** `tools.preset_rules[0].decision: allow_session`
- **THEN** 启动失败，stderr 输出 `invalid config: tools.preset_rules[0].decision must be allow_permanent or deny_permanent, got allow_session`

### Requirement: 会话并发上限

服务 SHALL 维护活跃 session 计数；当 POST `/v1/sessions` 时若当前活跃 session 数 ≥ `sessions.max_concurrent` SHALL 返回 `429 Too Many Requests` 含 body `{ "code": "max_sessions_reached", "limit": <N>, "active": <N> }`，不创建新 session。

活跃 session 指未被 DELETE 的 session（含 Idle、Thinking、AwaitPerm、Executing、Compacting、Cancelled）。

#### Scenario: 达到 max_concurrent 上限

- **WHEN** `sessions.max_concurrent: 50` 且当前已有 50 个未销毁 session，客户端 POST `/v1/sessions`
- **THEN** 服务返回 `429 Too Many Requests` 含 `{"code":"max_sessions_reached","limit":50,"active":50}`

### Requirement: history token 硬上限

服务 SHALL 在每次 LLM 调用前检查 session history token 数；若超过 `agent.max_history_tokens` 且压缩已完成仍超限，SHALL：

- 拒绝当前 user_message（如适用），POST 返回 `409 Conflict` `{ "code": "history_token_limit", "limit": <N>, "current": <N> }`
- SSE emit `error { code: "history_token_limit" }`
- 不调用 LLM；状态回 `Idle`

#### Scenario: 压缩后仍超 token 上限

- **WHEN** `agent.max_history_tokens: 200000`，session 触发自动压缩后 history 仍 > 200000（极端情况）
- **THEN** 服务拒绝当前 user_message；emit `error { code: "history_token_limit" }`

### Requirement: 配置热重载范围

服务 SHALL **不**支持 config.yaml 的热重载（修改需重启）。但以下两类文件 SHALL 支持热加载：

- `~/.workhorse-agent/agents/*.yaml` —— 由 multi-agent capability 在 Dispatch 时扫描
- `~/.workhorse-agent/skills/*/skill.yaml` —— 由 skills-loader 在会话创建时扫描

`mcp.json` 修改 SHALL 需要重启 MCP host（V2 加 SIGHUP 热重载）。

#### Scenario: config.yaml 修改后未生效

- **WHEN** 服务运行期间用户编辑 `config.yaml` 改 `server.port`
- **THEN** 服务仍用启动时端口；新端口需重启才生效

### Requirement: Grep 工具配置项

`config.yaml` 的 `tools.grep` 配置块 SHALL 支持以下键：

| 键 | 类型 | 默认 | 含义 |
|---|---|---|---|
| `timeout_seconds` | int | 60 | （已有）`Grep` 工具执行超时 |
| `workers` | int | 0 | 并行扫描 goroutine 数；`0` 表示 `min(runtime.NumCPU(), 8)`（见 docs/bench-grep-scaling.md）；`1` 走串行 codepath |
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

### Requirement: Thinking 配置（会话内不可变）

`AgentConfig` SHALL 支持 thinking 配置项，仅对 Anthropic provider 生效：

```yaml
thinking:
  enabled: false        # 默认关闭
  budget_tokens: 16000  # enabled 时生效；SHALL > 0
```

该配置 SHALL 在会话创建时读取并冻结，在单个会话生命周期内不可变（复用 memory snapshot 的"启动即冻结"模式）。SHALL NOT 提供中途调整 budget 或开关 thinking 的运行时接口——因为 thinking 参数变化会使 Anthropic 消息缓存前缀失效（见 prompt-cache 能力）。

校验：`enabled=true` 时 `budget_tokens` SHALL > 0，否则配置加载 SHALL 报错。`budget_tokens` 还存在模型相关的上限（不同 Anthropic 模型上限不同）；本次至少 SHALL 校验 `budget_tokens` 不超过该会话 `max_tokens`（thinking 预算不能超过总输出预算），模型感知的精确上限表见 design Open Questions。

#### Scenario: thinking 配置冻结于会话生命周期

- **WHEN** 会话以 `thinking.enabled=true, budget_tokens=16000` 创建
- **THEN** 该会话所有请求的顶层 thinking 参数恒为 `{type:"enabled",budget_tokens:16000}`，无运行时接口可改变它

#### Scenario: 非 Anthropic provider 忽略 thinking 配置

- **WHEN** 会话使用 OpenAI provider 且配置了 thinking
- **THEN** 配置加载不报错，但请求中不下发任何 thinking/reasoning 参数（本次范围外）

#### Scenario: 非法 budget 校验

- **WHEN** 配置 `thinking.enabled=true` 但 `budget_tokens=0`
- **THEN** 配置加载报错，提示 budget_tokens 必须 > 0

#### Scenario: budget 超过 max_tokens 校验

- **WHEN** 配置 `thinking.enabled=true`、`budget_tokens` 大于该会话 `max_tokens`
- **THEN** 配置加载报错，提示 thinking 预算不能超过总输出预算

### Requirement: 运行期权限配置热加载

`serve` 运行期间，服务 SHALL 监听 `~/.workhorse-agent/config.yaml` 的变更并在变更后热加载**权限子集**，无需重启进程，且 SHALL NOT 中断正在运行的会话。

监听 SHALL 满足：

- 监听 `config.yaml` 所在**目录**（而非固定 inode），以正确捕获编辑器"写临时文件 + rename"式存盘；
- 对短时间内的连续写入事件 SHALL 做 debounce 合并（如 200ms 窗口），避免半成品文件触发重载；
- SHALL 额外接受 `SIGHUP` 信号作为手动重载触发入口。

热加载触发后，服务 SHALL 以与启动相同的逻辑重新执行 `config.Load()`（含校验）。

可热加载的字段集合 SHALL 限定为：`tools.preset_rules`、`tools.default_permission`、`agent.permission_request_timeout_seconds`。其余字段（包括但不限于 `store.path`、`server.host`、`server.port`、`providers.*`）SHALL NOT 在运行期热换。

#### Scenario: 手改 preset_rules 后热加载生效

- **WHEN** `serve` 运行中，用户编辑 `config.yaml` 在 `tools.preset_rules` 新增一条合法规则并保存
- **THEN** 服务在 debounce 窗口后重载，将该规则对账进 SQLite，且进程不重启、已有会话不中断

#### Scenario: SIGHUP 触发重载

- **WHEN** `serve` 运行中，进程收到 `SIGHUP`
- **THEN** 服务重新执行 `config.Load()` 并应用权限子集变更

#### Scenario: 非权限字段变更被忽略并告警

- **WHEN** 热加载时检测到 `server.port` 或 `store.path` 较当前生效值发生变化
- **THEN** 服务 SHALL NOT 改变监听端口或数据库路径，并记录一条 `WARN` 日志说明该字段需重启方可生效

### Requirement: 热加载校验闸门

热加载重新执行 `config.Load()` 时，若解析失败或校验（validate）未通过，服务 SHALL 保留当前已生效的配置，记录 `WARN` 日志，并 SHALL NOT 应用任何来自该次失败重载的字段。

#### Scenario: 写坏 YAML 不影响当前配置

- **WHEN** `serve` 运行中，用户将 `config.yaml` 改成语法错误或含非法 `decision` 值的内容并保存
- **THEN** 服务记录 `WARN`，继续使用变更前已生效的权限规则，已有会话的 `Check()` 行为不变

#### Scenario: 校验通过后才应用

- **WHEN** 用户先存了一份语法错误的文件（被拒绝），随后修正为合法内容再次保存
- **THEN** 第二次保存通过校验，新的权限子集被应用并对账进 SQLite

### Requirement: tools.tool_search 配置项

配置 `tools.tool_search`（YAML 键 `tool_search`，位于 `tools` 段）SHALL 控制 tool search 的延迟模式。其取值集合 SHALL 限定为：`tst`、`auto`、`auto:N`（N 为 0-100 的整数）、`standard`，以及空值（视同 `tst`）。默认值 SHALL 为 `tst`。

配置校验 SHALL 在启动时拒绝不属于上述集合的值（如 `auto:abc`、`auto:200`、`foo`）。

#### Scenario: 默认值为 tst

- **WHEN** `config.yaml` 未设置 `tools.tool_search`
- **THEN** 加载后的有效模式 SHALL 为 `tst`

#### Scenario: 合法 auto:N 通过校验

- **WHEN** `tools.tool_search` 为 `auto:25`
- **THEN** 配置校验通过，阈值百分比为 25

#### Scenario: 非法值被拒绝

- **WHEN** `tools.tool_search` 为 `auto:abc` 或 `foo`
- **THEN** `config.Load()` SHALL 返回校验错误，进程 SHALL NOT 以该配置启动

