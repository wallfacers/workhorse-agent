## 1. 决策与设计定稿

- [ ] 1.1 定 D1 工具形态（单 `TodoWrite` 整表覆盖 vs 多工具 `TaskCreate`/`TaskUpdate`/`TaskList`）并记录结论
- [ ] 1.2 定 D2 持久化（纯内存 vs 落 events 表）；若落表，确认遵循 append-only + ULID + idx 约定
- [ ] 1.3 定 D3 SSE 事件方案（复用既有管道 vs 新增 `task_update` 事件类型）；如需扩协议，与 api-protocol 对齐事件名与 schema

## 2. 工具实现

- [ ] 2.1 新建 `internal/tools/tasklist/`：定义任务结构（subject/status/可选 description、activeForm）与三态状态机
- [ ] 2.2 实现 `tools.Tool` 接口（Name/Description/InputSchema/Run/IsReadOnly/CanRunInParallel/DefaultTimeout）
- [ ] 2.3 实现状态流转校验：拒绝枚举外状态值，返回 `is_error: true` 且不改任务
- [ ] 2.4 将任务清单状态挂到会话级作用域，确保子 Agent 会话独立清单

## 3. 接线与门控

- [ ] 3.1 在 `cmd_serve.go` registry 装配处注册工具实例
- [ ] 3.2 验证 `buildProviderToolSchemas` 按 `AllowedTools` 过滤生效（未授权时不暴露 schema）
- [ ] 3.3 按 D3 接入 SSE 广播任务变更

## 4. 提示词引导

- [ ] 4.1 在 `internal/prompt/builtins.go` 的 `DefaultBasePrompt` 追加任务清单使用引导（≥3 步才用、开始前置 in_progress、完成即置 completed、不批量补记、琐碎任务直接做）
- [ ] 4.2 更新 `internal/prompt` 相关 byte-stable / 内容断言测试（base prompt 文本变化）

## 5. 测试与验收

- [ ] 5.1 工具单测：创建、更新、列出、状态机流转、非法状态拒绝
- [ ] 5.2 门控测试：`AllowedTools` 未含工具时不暴露且调用报 `tool_not_allowed`
- [ ] 5.3 会话隔离测试：A/B 两会话清单互不可见；子 Agent 独立清单
- [ ] 5.4 SSE 广播测试：状态变更在 SSE 流中出现携带最新状态的事件
- [ ] 5.5 「默认提示词含引导」测试：渲染 `DefaultBasePrompt` 含任务清单引导文字
- [ ] 5.6 `go build ./...`、`go test ./...` 全绿；`gofmt -l` 无输出；`golangci-lint run` 干净
- [ ] 5.7 更新 task-list spec 并归档 change
