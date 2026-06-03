## ADDED Requirements

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
