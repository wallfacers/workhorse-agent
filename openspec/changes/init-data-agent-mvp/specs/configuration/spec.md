## ADDED Requirements

### Requirement: 配置文件位置与加载顺序

服务 SHALL 按以下优先级合并配置（高优先级覆盖低）：

1. 命令行参数（最高，如 `--port 8000`）
2. 环境变量（前缀 `DATAAGENT_`，如 `DATAAGENT_PORT=8000`）
3. 用户配置文件 `~/.dataagent/config.yaml`
4. 内置默认值（最低）

`dataagent init` 命令 SHALL 在 `~/.dataagent/config.yaml` 不存在时交互式生成该文件。

#### Scenario: 环境变量覆盖配置文件

- **WHEN** `config.yaml` 中 `port: 7821`，启动时设 `DATAAGENT_PORT=9000`
- **THEN** 服务绑定 `127.0.0.1:9000`

#### Scenario: 命令行覆盖环境变量

- **WHEN** 同时设 `DATAAGENT_PORT=9000` 和命令行 `--port 8000`
- **THEN** 服务绑定 `127.0.0.1:8000`

### Requirement: 完整 Config Schema

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
  provider_retry_attempts: 3
  provider_retry_backoff_ms: [500, 2000, 8000]

# === 工具 ===
tools:
  bash:
    timeout_seconds: 120           # 单次 Bash 命令默认超时
  default_allowed_tools: []        # 全局工具白名单；空表示全部启用

# === 持久化 ===
store:
  path: ~/.dataagent/state.db
  busy_timeout_ms: 5000

# === 会话上限 ===
sessions:
  max_concurrent: 50               # 同时活跃 session 数；超过 POST /v1/sessions 返回 429

# === MCP ===
mcp:
  config_path: ~/.dataagent/mcp.json

# === Skills / Agent Roles ===
skills:
  dir: ~/.dataagent/skills
agents:
  dir: ~/.dataagent/agents

# === 日志 ===
logging:
  level: info                      # debug | info | warn | error
  format: json                     # json | text
  log_llm_payload: false           # debug 模式才打 LLM full request/response

# === 心跳/调试 ===
debug:
  enabled: false                   # true 时启用 /debug/* 端点
```

所有数值字段 SHALL 在加载时校验：负数、零、超出合理范围（如 port>65535）SHALL 拒绝启动并打印明确错误。

#### Scenario: 非法端口拒绝启动

- **WHEN** `server.port: 70000`
- **THEN** 启动失败，stderr 输出 `invalid config: server.port must be 1-65535, got 70000`

#### Scenario: enabled=true 但 token 为空

- **WHEN** `auth.enabled: true` 但 `auth.bearer_token: ""`
- **THEN** 启动失败，stderr 输出 `invalid config: auth.bearer_token must be set when auth.enabled is true`

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

- `~/.dataagent/agents/*.yaml` —— 由 multi-agent capability 在 Dispatch 时扫描
- `~/.dataagent/skills/*/skill.yaml` —— 由 skills-loader 在会话创建时扫描

`mcp.json` 修改 SHALL 需要重启 MCP host（V2 加 SIGHUP 热重载）。

#### Scenario: config.yaml 修改后未生效

- **WHEN** 服务运行期间用户编辑 `config.yaml` 改 `server.port`
- **THEN** 服务仍用启动时端口；新端口需重启才生效
