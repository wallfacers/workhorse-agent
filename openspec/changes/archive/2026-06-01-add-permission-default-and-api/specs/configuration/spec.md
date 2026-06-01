## MODIFIED Requirements

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
