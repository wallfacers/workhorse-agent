## ADDED Requirements

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
