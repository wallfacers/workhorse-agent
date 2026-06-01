# permission-api Specification

## Purpose
TBD - created by archiving change add-permission-default-and-api. Update Purpose after archive.
## ADDED Requirements

### Requirement: 启动时注入预设权限规则

服务启动时 SHALL 从 `tools.preset_rules` 配置读取规则列表，将每条规则写入 SQLite `permissions` 表。

每条预设规则的 ID SHALL 由 `perm-` + hex(md5(tool + "\x00" + pattern + "\x00" + decision))[:16] 确定性生成。`session_id` 固定为 `""`，`scope` 固定为 `permanent`。

写入操作 SHALL 使用 `INSERT OR REPLACE`，保证同一条配置每次重启生成相同 ID 并覆盖旧行，实现幂等。

服务 SHALL 在规则注入完成后输出日志：`"applied N preset permission rules"`，其中 N 为配置中的规则数量。

#### Scenario: 首次启动创建预设规则

- **WHEN** `config.yaml` 含 2 条 `preset_rules`，数据库为空，服务启动
- **THEN** 数据库中新增 2 条永久规则，日志输出 `"applied 2 preset permission rules"`

#### Scenario: 重复启动幂等

- **WHEN** `config.yaml` 含相同的 2 条 `preset_rules`，服务重启
- **THEN** 数据库中仍为 2 条规则（INSERT OR REPLACE 覆盖），不产生重复或错误

#### Scenario: 修改预设规则后重启

- **WHEN** `preset_rules` 中一条规则的 decision 从 `allow_permanent` 改为 `deny_permanent`，服务重启
- **THEN** 该规则的 ID 随之变化（decision 参与 hash），旧 ID 的规则保留在 DB 中，新 ID 的规则被创建

#### Scenario: 空预设规则列表

- **WHEN** `tools.preset_rules` 为 `[]` 或未配置
- **THEN** 无规则注入，正常启动，不输出 preset 相关日志

---

### Requirement: 预设规则配置格式

`tools.preset_rules` SHALL 是一个列表，每条规则含以下字段：

```yaml
tools:
  preset_rules:
    - tool: "Bash"                   # 工具名；空或 "*" 表示所有工具
      pattern: "git *"               # glob 模式；空表示所有 resource
      decision: allow_permanent      # allow_permanent | deny_permanent
```

- `tool`：可选。空字符串或 `"*"` 表示匹配所有工具
- `pattern`：可选。空字符串表示匹配所有 resource（等价 `"**"`）
- `decision`：必选。合法值为 `allow_permanent` 或 `deny_permanent`

`decision` 字段非法时 SHALL 拒绝启动并输出明确错误信息。

#### Scenario: 非法 decision 拒绝启动

- **WHEN** `preset_rules[0].decision: allow_once`
- **THEN** 启动失败，stderr 输出 `"invalid config: tools.preset_rules[0].decision must be allow_permanent or deny_permanent, got allow_once"`
