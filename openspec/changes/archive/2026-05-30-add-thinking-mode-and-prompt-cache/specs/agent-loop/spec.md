## MODIFIED Requirements

### Requirement: LLM 推理 → 工具 → 回灌循环

会话进入 `Thinking` 状态后，Agent SHALL 反复执行下述循环直到 LLM 不再返回 tool_use 或被取消：

1. 用当前 `history` + `system prompt` + `tools schema` 调用 provider 的 `Stream`
2. 流式接收 `text_delta` / `reasoning_start` / `reasoning_delta` / `reasoning_end` / `tool_use` / `stop`，按事件类型 emit 给客户端
3. 累积 thinking 块（含 signature）与 text、tool_use，组装进**同一条** assistant 消息（顺序保持产出顺序），追加进 history
4. 若本轮含 `tool_use`：按并发性切批、执行（见 tool-system）、把 `tool_result[]` 追加进 history
5. 若本轮 stop 原因为 `end_turn` 或无 `tool_use`：循环结束，状态回 `Idle`
6. 若本轮被 ctx 取消：转入 `Cancelled` 收尾流程（见取消语义）

`reasoning_delta` 事件 SHALL 实时 emit 为客户端 SSE `reasoning_delta`（见 api-protocol 能力）；thinking 块的 `signature` SHALL NOT 出现在任何 SSE 事件中，仅进入持久化与回传。

#### Scenario: 多轮工具循环

- **WHEN** 用户问 "读取 a.go 和 b.go 并总结"，LLM 返回 `[Read(a), Read(b)]`，回灌后 LLM 返回总结文本
- **THEN** Agent 完成 2 个 Read 工具的并发调用，把 2 个 tool_result 追加进 history，再调一次 LLM 拿到总结，emit `assistant_text_done` 后状态回 `Idle`

#### Scenario: 仅文本响应不触发工具循环

- **WHEN** 用户问 "你好"，LLM 返回纯文本 "你好！"，stop 原因 `end_turn`
- **THEN** Agent 不调用任何工具，emit `assistant_text_delta` / `assistant_text_done` 后状态回 `Idle`

#### Scenario: 含 thinking 的工具循环组装

- **WHEN** LLM 返回 `[thinking(sig=S), tool_use(Read)]`
- **THEN** Agent 实时 emit `reasoning_start`/`reasoning_delta`/`reasoning_end`（不含 signature），把 thinking 块（含 signature=S）与 tool_use 组装进同一条 assistant 消息追加进 history；执行 Read 并回灌后继续循环
