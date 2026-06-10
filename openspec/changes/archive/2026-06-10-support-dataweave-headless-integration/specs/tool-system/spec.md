## MODIFIED Requirements

### Requirement: 工具注册表与动态发现

服务 SHALL 维护全局 ToolRegistry，启动时注册 5 个内置工具；MCP 工具与 Skills 触发的 LoadSkill 注入工具 SHALL 通过同一接口注册。

ToolRegistry 的注册、查询、删除操作 SHALL 用 `sync.RWMutex` 保护，确保运行时多 goroutine（MCP server 启动/重启、LoadSkill 动态注入、session lookup）并发访问安全。Lookup 是高频读操作 SHALL 用 RLock。

会话级 SHALL 支持工具子集：通过 `AllowedTools` 配置可限制本会话可调工具范围。`AllowedTools` 条目 SHALL 支持 **glob 模式**（含 `*` `?` `[` 元字符的条目按 glob 匹配工具名；不含元字符的条目与精确匹配等价，既有配置语义不变）。工具名不含 `/`，glob 语义与权限规则 tool 字段一致——`dataweave__*` 一行即可放行某 MCP server 的全部工具，server 后续新增工具无需改白名单。nil 或空列表仍表示"不过滤"。

会话创建时，被白名单过滤掉的已注册工具 SHALL 在日志中留下一条可观察记录（至少含被过滤工具名列表或数量），避免工具"静默消失"难以排查。

#### Scenario: 子集限制

- **WHEN** 会话配置 `AllowedTools: ["Read", "Grep"]`，LLM 请求调用 `Bash`
- **THEN** Agent 不向 LLM 暴露 Bash schema；若 LLM 仍尝试调用，emit `error { code: "tool_not_allowed" }`

#### Scenario: glob 条目放行整个 MCP server

- **WHEN** 会话配置 `AllowedTools: ["Read", "Grep", "ToolSearch", "dataweave__*"]`，registry 中注册有 `dataweave__query_tasks`、`dataweave__node_exec` 及内置 `Bash`
- **THEN** 工具面包含两个 `dataweave__` 工具与 Read/Grep/ToolSearch，不含 `Bash`；该 server 后续新注册的 `dataweave__query_lineage` 在新会话中自动在列

#### Scenario: 过滤可观察

- **WHEN** 白名单过滤使 `Bash`/`Write`/`Edit` 从某会话工具面移除
- **THEN** 服务日志出现一条记录指明该会话被过滤掉的工具（名列表或数量），级别不低于 DEBUG
