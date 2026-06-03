## MODIFIED Requirements

### Requirement: 取消收尾超时

`Cancelled → Idle` 的收尾流程 SHALL 在 `agent.cancel_drain_timeout_seconds`（默认 5，可配）内完成，依次执行以下 checklist：

1. ctx 取消已触发（同步）
2. 等待 provider HTTP 请求中止（HTTP client 内部 ctx 传播；通常 < 50ms）
3. 等待所有正在跑的工具 Run 返回（含 Bash 进程组 SIGTERM → SIGKILL 兜底；含 MCP `notifications/cancelled` 转发；含子 session 级联取消）
4. 合成 cancelled tool_result 并追加 history（同步）
5. 持久化最终 history（如非 ephemeral）
6. **持久化 interrupted 标志**：通过 `Session.LastAssistantMessageID()` 获取最后一条 `role='assistant'` 消息的 ULID，调用 `store.MarkMessageInterrupted(ctx, messageID)` 将 `interrupted` 列设为 `1`；非 ephemeral 且 store 非 nil 且 messageID 非空时执行
7. emit `interrupted` 事件，payload SHALL 包含 `message_id` 字段（值为步骤 6 使用的 messageID）
8. 清空 outbox 中属于被中断那一轮的事件（见 api-protocol "中断到达时清空 SSE 积压"）

若整个 checklist 在超时内完成 SHALL 正常转入 `Idle`。

若超时仍未完成 SHALL：

- emit 一条 `error { code: "cancel_timeout", details: { phase: "<当前 step>", elapsed_ms: <N> } }` 事件
- 对未返回的工具/子 session 启动**强制丢弃**：把它们的 ctx 标 done 后不再等待，goroutine 自然在下次 select ctx.Done() 时退出（可能短暂残留）
- 强制转入 `Idle`，会话可继续接受新 user_message
- 日志 `warn` 级别记录 phase + 元数据供排查

#### Scenario: 正常收尾在超时内完成

- **WHEN** 正常取消（tool 在超时内返回、cancelled tool_result 合成完毕）
- **THEN** 服务 SHALL 在 checklist 完成后转入 `Idle`；工具执行进程已退出；history
  与 events 表均已持久化；最后一条 assistant 消息的 `interrupted` 列 SHALL 为 `1`

#### Scenario: 中断消息持久化后 rehydration 保留标志

- **WHEN** 某会话在中断后从 history 端点重建（如项目切换后重新打开）
- **THEN** `GET /v1/sessions/{id}/history` 返回的最后一条 assistant 消息 SHALL 包含 `"interrupted": true`
- **AND** 客户端 UI SHALL 显示中断标记（如"（已中断）"）

#### Scenario: 超时强制转入 Idle

- **WHEN** tool 执行在 5s 超时后仍未返回
- **THEN** 服务 SHALL emit `cancel_timeout` 错误；跳过未完成的 tool；强制转入
  `Idle`；会话仍可接受新 user_message
