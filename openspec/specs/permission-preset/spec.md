# permission-preset Specification

## Purpose
TBD - created by archiving change add-permission-default-and-api. Update Purpose after archive.
## Requirements

### Requirement: 启动时注入预设权限规则

服务启动时 SHALL 从 `tools.preset_rules` 配置读取规则列表，将每条规则写入 SQLite `permissions` 表。

每条预设规则的 ID SHALL 由 `preset-` + hex(sha256(tool + "\x00" + pattern))[:16] 确定性生成（**仅** tool 与 pattern 参与 hash，decision 不参与）。`session_id` 固定为 `""`，`scope` 固定为 `permanent`。`preset-` 前缀使预设规则自标识，便于 API 标注 `source` 与启动对账。

写入操作 SHALL 使用 `INSERT OR REPLACE`。由于 ID 不含 decision，同一 (tool, pattern) 的规则在 decision 变化时 SHALL 原地覆盖，使收紧（如 allow→deny）立即生效，而非遗留旧授权行。

启动时 SHALL 执行对账：枚举所有以 `preset-` 为前缀的永久规则，删除当前配置中已不存在的条目，使从配置移除的预设不再以陈旧授权形式残留。手动创建的规则（`perm-` 前缀）不受对账影响。

服务 SHALL 在有规则注入或有陈旧规则被删除时输出日志：`"applied N preset permission rules"`（附 `removed_stale` 计数）。

#### Scenario: 首次启动创建预设规则

- **WHEN** `config.yaml` 含 2 条 `preset_rules`，数据库为空，服务启动
- **THEN** 数据库中新增 2 条永久规则，日志输出 `"applied 2 preset permission rules"`

#### Scenario: 重复启动幂等

- **WHEN** `config.yaml` 含相同的 2 条 `preset_rules`，服务重启
- **THEN** 数据库中仍为 2 条规则（INSERT OR REPLACE 覆盖），不产生重复或错误

#### Scenario: 修改预设规则后重启（收紧立即生效）

- **WHEN** `preset_rules` 中一条规则的 decision 从 `allow_permanent` 改为 `deny_permanent`，服务重启
- **THEN** ID 不变（decision 不参与 hash），该行被原地覆盖为 `deny_permanent`，DB 中仅保留 1 条该 (tool, pattern) 规则，新决定立即生效

#### Scenario: 从配置移除预设规则后重启

- **WHEN** 某条预设规则从 `tools.preset_rules` 删除，服务重启
- **THEN** 对账删除其对应的 `preset-` 规则行；手动创建的 `perm-` 规则不受影响

#### Scenario: 空预设规则列表

- **WHEN** `tools.preset_rules` 为 `[]` 或未配置
- **THEN** 无规则注入；若存在历史 `preset-` 规则则对账将其删除，否则正常启动且不输出 preset 相关日志

---

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
