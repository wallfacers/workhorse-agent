## ADDED Requirements

### Requirement: 会话级 instructions 与 metadata 持久化

会话 SHALL 支持创建时注入的两项定制数据并持久化（sessions 表新增列，sqlite 迁移）：

- `instructions`：会话级附加指令文本。Agent loop 装配 system prompt 时 SHALL 将其拼接进 Instructions 动态段（项目 AGENTS.md 内容之后），SHALL NOT 改变 base-first 缓存前缀顺序。会话生命周期内不可变。
- `metadata`：string→string 映射。服务 SHALL 仅存储与返回，不参与任何推理或工具逻辑。

两者 SHALL 在会话水合（按需重开）后保持生效；ephemeral 会话仅存内存。

#### Scenario: instructions 注入 system prompt 动态段

- **WHEN** 会话创建时携带 `instructions: "引用平台对象时使用其 ID"`，随后发起一轮推理
- **THEN** 发往 provider 的 system prompt 中 Instructions 段含该文本，且静态 base 前缀字节序列与未携带 instructions 的会话完全一致（缓存前缀不受影响）

#### Scenario: 水合后 instructions 仍生效

- **WHEN** 携带 instructions 的会话被驱逐，客户端向其发送新消息触发水合
- **THEN** 该轮推理的 system prompt 仍含原 instructions 文本
