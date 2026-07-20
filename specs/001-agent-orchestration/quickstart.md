# Quickstart: Agent 编排能力升级

**Feature**: 001-agent-orchestration

四项能力落地后的手工验证路径（自动化测试见 research.md R10）。

## 前置

```bash
go build ./... && go test ./... && golangci-lint run
workhorse-agent serve   # 127.0.0.1，默认端口
```

创建会话并订阅事件流（两个终端）：

```bash
# 终端 A：订阅 SSE
SID=$(curl -s -XPOST localhost:8787/v1/sessions -d '{"workdir":"'$PWD'"}' | jq -r .id)
curl -N localhost:8787/v1/sessions/$SID/stream

# 终端 B：发消息
say() { curl -s -XPOST localhost:8787/v1/sessions/$SID/stream \
  -d '{"type":"user_message","payload":{"content":"'"$1"'"}}'; }
```

## US1 后台委派

```bash
say "Delegate a background research task: summarize how permission checks work in this repo. Then immediately tell me a joke."
```

验收观察：

1. 笑话在委派返回后立刻到来（不等调研完成，SC-001）。
2. SSE 上出现 `tool_call_done`，Output 含 `Delegation started: <adj-color-animal>`。
3. 等待后台完成后：`say "hello"` → 本轮回复前，历史中出现恰好一条委派完成 system 通知（再 `say "hi"` 验证不重复，SC-002）。
4. `say "Read the delegation result"` → agent 调 `delegation_read` 返回全文。
5. 反向验证：委派 prompt 里让子代理写文件 → 子代理无 Write/Bash 工具可用（工具面只读）。

## US2 溢出自愈

自动化测试为主（fake provider 首次返回 `context_length_exceeded`）。手工近似验证：

1. 用小上下文模型配置制造长会话直至溢出。
2. 观察 SSE：出现 `compaction` 事件后本轮正常完成，**没有** `error{code:provider_context_length_exceeded}`。
3. 极端场景（压缩后仍溢出）：本轮以既有 error 事件结束，且只发生一次压缩尝试。

## US3 定时调度

```bash
say "Create a schedule named smoke that runs the instruction 'append current time to /tmp/wh-sched.log using Bash' one minute from now"
# agent 应调 schedule_create（run_at 一次性）
sleep 90
say "Read the run log of schedule smoke"
```

验收观察：

1. `schedule_read_log` 返回一次 complete 运行，含 session_id。
2. `curl localhost:8787/v1/sessions/<session_id>/history` 可回放无人值守运行全程。
3. 一次性计划触发后 `schedule_list` 显示 disabled（FR-019）。
4. cron 场景：`say "... cron '*/1 * * * *' ..."` 观察每分钟恰好一次（SC-004）；`schedule_remove` 后不再触发。
5. 权限场景：instruction 含需审批的命令 → 运行日志显示该调用被超时拒绝，运行未挂起（FR-018）。
6. 重启 serve → `schedule_list` 计划仍在（FR-014）。

## US4 活动上报

```bash
say "Use the Dispatch tool with the explore agent to find where SSE events are written"
```

验收观察：终端 A 的 SSE 流中，子代理每次工具调用对应一条 `subagent_status`（`activity` 为单行可读文本），结束时一条 `activity:""` 清空事件；全程 `subagent_event` 照旧出现（互不影响）。

## 回归

```bash
go test ./... && golangci-lint run
```

旧客户端兼容（SC-006）：用不认识 `subagent_status` 的既有客户端连接，所有既有功能不受影响。
