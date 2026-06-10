## MODIFIED Requirements

### Requirement: 预设规则配置格式

`tools.preset_rules` SHALL 是一个列表，每条规则含以下字段：

```yaml
tools:
  preset_rules:
    - tool: "Bash"                   # 工具名 glob；空或 "*" 表示所有工具
      pattern: "git *"               # glob 模式；空表示所有 resource
      decision: allow_permanent      # allow_permanent | deny_permanent
    - tool: "dataweave__query_*"     # tool 字段支持 glob，可按名前缀覆盖 MCP 工具
      decision: allow_permanent
```

- `tool`：可选。glob 模式（与 `pattern` 同一套 `MatchGlob` 语法）；空字符串或 `"*"` 表示匹配所有工具；不含 glob 元字符的值与精确匹配等价
- `pattern`：可选。空字符串表示匹配所有 resource（等价 `"**"`）
- `decision`：必选。合法值为 `allow_permanent` 或 `deny_permanent`

`decision` 字段非法时 SHALL 拒绝启动并输出明确错误信息。

#### Scenario: 非法 decision 拒绝启动

- **WHEN** `preset_rules[0].decision: allow_once`
- **THEN** 启动失败，stderr 输出 `"invalid config: tools.preset_rules[0].decision must be allow_permanent or deny_permanent, got allow_once"`

#### Scenario: tool glob 预设规则注入

- **WHEN** config 配置 `{ tool: "dataweave__query_*", decision: allow_permanent }` 并启动（或热加载）
- **THEN** `permissions` 表出现对应 `preset-` 前缀行，任意会话中 `dataweave__query_tasks` 等调用免询问放行
