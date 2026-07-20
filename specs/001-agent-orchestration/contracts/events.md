# SSE Event Contracts: Agent 编排能力升级

**Feature**: 001-agent-orchestration

事件走既有 SSE 通道（`GET /v1/sessions/{id}/stream`，`id:` = events.idx，支持 `Last-Event-ID` 重放）。新增事件类型追加到 `internal/api/protocol/protocol.go` 的 `ServerEventType` 常量与 `AllServerEventTypes`。旧客户端忽略未知 `event:` 类型即可，协议向后兼容，`protocol_version` 保持 `"2"` 不变。

## 新增：subagent_status

面向 UI 的子代理活动摘要（与既有全量转发事件 `subagent_event` 互补，后者保持不变）。发布方：`internal/tools/dispatch/pump.go` 的 forward 路径，`EmitNow`（非阻塞、outbox 满则丢弃——尽力而为，FR-022）。

```json
{
  "type": "subagent_status",
  "payload": {
    "agent_id": "01J…（子会话 ULID）",
    "agent_type": "explore",
    "description": "Research auth flow",
    "activity": "Read internal/agent/loop.go"
  }
}
```

字段约束：

- `activity`：单行、≤80 码点、超长以 `…` 截断；多行输入折叠为单行。
- 清空事件：任务结束（turn end / 取消 / 错误）时发布一次 `activity: ""`（其余字段不变），客户端据此移除状态显示。
- 触发时机：子会话每个 `tool_call_start` 事件翻译一条；文本生成阶段不发（避免噪声）。

活动翻译规则（`internal/tools/dispatch/activity.go`，示例）：

| 子会话工具 | activity 模板 |
|---|---|
| Read | `Read <file_path>` |
| Grep | `Grep "<pattern>"` |
| Bash | `<command 截断>` |
| session_search / MemorySearch | `Search "<query>"` |
| 其他/MCP | `<tool_name>` |

## 复用（无契约变更，列出以明确观察面）

- **compaction**：溢出自愈触发的强制压缩复用既有事件与 payload（`before_tokens/after_tokens/before_messages/after_messages`），满足 FR-012 可观察性；自愈重试本身不新增事件类型（重试对客户端透明，失败时走既有 `error` 事件，code 仍为 `provider_context_length_exceeded`）。
- **error / provider_retry / tool_call_* / permission_***：语义不变。
- 委派完成通知**不是** SSE 事件：它是注入会话历史的 system 消息（进 messages/事件流的既有消息表示），客户端经正常历史/流路径可见。

## 事件流兼容性验收

- 不认识 `subagent_status` 的旧客户端：功能不受影响（SC-006）。
- `AllServerEventTypes` 相关测试与 debug snapshot 端点随常量新增自动覆盖。
