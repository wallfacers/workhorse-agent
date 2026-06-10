## ADDED Requirements

### Requirement: tools.default_allowed_tools 服务器级默认工具白名单

`tools.default_allowed_tools`（string 列表）SHALL 作为会话创建时 `allowed_tools` 缺省值生效：`POST /v1/sessions` 未携带（或携带空）`allowed_tools` 时，会话工具面 SHALL 按该配置过滤（语义与请求级 `allowed_tools` 相同，经 `Registry.Filtered` 从发往 LLM 的 schema 中移除未列出的工具）。

- 列表条目 SHALL 支持 glob 模式（与会话级 `AllowedTools` 同一套语义，见 tool-system capability）：`dataweave__*` 一行放行整个 MCP server，server 新增工具无需改配置。
- 配置为空列表或未配置时 SHALL 保持现状（全部已注册工具可用）。
- 请求显式携带非空 `allowed_tools` 时 SHALL 覆盖该默认值。
- 该字段不属于热加载子集，变更需重启生效。
- Dispatch 子代理的工具面继续由 agent 定义决定，不受此默认值影响。

#### Scenario: headless 画像关闭内置副作用工具

- **WHEN** config 配置 `tools.default_allowed_tools: [Read, Grep, ToolSearch, "dataweave__*"]`，客户端 POST `/v1/sessions` 不带 `allowed_tools`
- **THEN** 该会话发往 LLM 的工具 schema 不含 `Bash`/`Write`/`Edit`，但含 `dataweave` server 的全部工具；LLM 无法发起被移除的内置工具调用

#### Scenario: 请求级显式列表覆盖默认

- **WHEN** 同上配置，但请求携带 `allowed_tools: [Read, Grep, Bash]`
- **THEN** 该会话工具面含 `Bash`（请求级覆盖默认画像）

#### Scenario: 未配置时行为不变

- **WHEN** `tools.default_allowed_tools` 未配置，请求不带 `allowed_tools`
- **THEN** 会话工具面包含全部已注册工具（与本变更前一致）
