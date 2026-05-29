## 1. 基础设施：RecordingProvider

- [x] 1.1 创建 `test/real_e2e/judge/recorder.go`：`RecordingProvider` 类型，实现 `provider.Provider` 接口，支持 `ModeReplay`/`ModeRecord`/`ModeLive` 三种模式，`Name()` 委托内部 Provider
- [x] 1.2 实现 `Load()` 方法：读取 `test/real_e2e/fixtures/recordings/<testID>.jsonl`，解析 header + turn 行到内存
- [x] 1.3 实现 `streamReplay()`：按 offset 顺序弹出 turn，通过 channel 回放事件；turn 耗尽时返回 `{EventStop, stop_reason: "end_turn"}`
- [x] 1.4 实现 `streamRecord()`：调用内部 Provider 的 `Stream()`，收集事件同时转发，结束时追加到 turns 列表
- [x] 1.5 实现 `Save()`/`Flush()`：序列化 turns 为 JSONL 写入文件（header + turn 行）
- [x] 1.6 实现 `modeFromEnv()`：读取 `WORKHORSE_TEST_MODE` 环境变量
- [x] 1.7 单元测试 `TestRecordingProvider_ReplayMode`：写入测试 JSONL，验证 Load + Stream 回放
- [x] 1.8 单元测试 `TestRecordingProvider_RecordMode`：用 mockprovider 作为内部 Provider，验证 Stream + Save 生成 JSONL

## 2. 基础设施：TraceCollector

- [x] 2.1 创建 `test/real_e2e/judge/trace.go`：定义 `Trace`、`Turn`、`ToolCallRecord`、`ToolResultRecord` 类型
- [x] 2.2 实现 `CollectTrace()`：从 `bufio.Reader` 逐行解析 SSE 帧，按事件类型（`assistant_text_delta`/`assistant_text_done`/`tool_call_start`/`tool_call_done`/`error`）组装 Trace
- [x] 2.3 实现 `stop_reason == "end_turn"` 终止收集逻辑
- [x] 2.4 实现超时机制：`CollectTrace()` 接受 `timeout` 参数，超时返回已收集的 Trace
- [x] 2.5 单元测试 `TestCollectTrace_TextOnly`：模拟纯文本 SSE 流，验证 ModelOutput 拼接
- [x] 2.6 单元测试 `TestCollectTrace_WithToolCall`：模拟含工具调用的 SSE 流，验证 ToolCalls + ToolResults 收集

## 3. 基础设施：Judge 接口与 GLM-5 实现

- [x] 3.1 创建 `test/real_e2e/judge/judge.go`：定义 `Verdict`/`JudgeResult`/`Rubric`/`Criterion` 类型和 `Judge` 接口
- [x] 3.2 实现 `judgeCacheKey()`：`SHA-256(Trace JSON || Rubric JSON)` 前 16 字符
- [x] 3.3 实现 `loadCachedJudge()`/`saveCachedJudge()`：缓存 JSON 文件读写
- [x] 3.4 创建 `test/real_e2e/judge/glm5.go`：`GLM5Judge` 类型，通过 DashScope Anthropic 兼容 API 调用 `glm-5`
- [x] 3.5 实现 `buildPrompt()`：构造 Judge prompt（用户消息 + 交互轨迹 + Rubric 标准 + JSON 输出指令）
- [x] 3.6 实现 `callLLM()`：发送 HTTP 请求，解析响应，从 markdown code block 中提取 JSON
- [x] 3.7 实现 `Evaluate()`：带缓存逻辑（cached 模式读缓存，llm 模式调 API 并写缓存）
- [x] 3.8 单元测试 `TestGLM5Judge_EvaluateWithMock`：用 httptest 模拟 API，验证 Evaluate 返回正确 JudgeResult
- [x] 3.9 单元测试 `TestGLM5Judge_Caching`：验证首次调 API、第二次命中缓存

## 4. 测试框架：Rubric 定义

- [x] 4.1 创建 `test/real_e2e/rubrics.go`（`//go:build real_e2e`）：定义 `fileToolsRubric`、`fileNotFoundRubric`、`memoryRubric`、`sessionSearchRubric`、`extAgentRubric`

## 5. 测试框架：Runner Helpers

- [x] 5.1 创建 `test/real_e2e/helpers.go`（`//go:build real_e2e`）：定义 `realStack` 结构体和 `newRealStack()` 构建完整测试栈（SQLite + RecordingProvider + ToolRegistry + SessionManager + HTTP Server）
- [x] 5.2 实现 `createSession()`：通过 HTTP API 创建会话
- [x] 5.3 实现 `openSSE()`：打开 SSE 连接
- [x] 5.4 实现 `postMessage()`：发送用户消息
- [x] 5.5 定义 `scenarioConfig` 和 `runScenario()`：完整的场景驱动器（newRealStack → createSession → postMessage → CollectTrace → 可选 Judge Evaluate → Save recording）
- [x] 5.6 实现 `assertVerdict()`：Judge 结果断言，PASS 通过否则 FailNow 并输出 Reasoning + Suggestions
- [ ] 5.7 单元测试 `TestNewRealStack_SkipWithoutKey`：无 API Key 时验证 `t.Skip`（见下方说明）

## 6. 场景测试：文件操作工具

- [x] 6.1 创建 `test/real_e2e/file_tools_test.go`（`//go:build real_e2e`）
- [x] 6.2 `TestFileRead_Basic_Smoke`：读取已知文件，验证模型报告内容（Setup: 创建 `hello.txt`）
- [x] 6.3 `TestFileRead_NotFound_Smoke`：读取不存在的文件，验证模型报告错误
- [x] 6.4 `TestFileWrite_Create_Integration`：创建文件，验证写入
- [x] 6.5 `TestFileEdit_Modify_Integration`：编辑已有文件，验证修改（Setup: 创建 `config.yaml`）
- [x] 6.6 `TestBash_ListDir_Smoke`：通过 Bash 执行 `ls`，验证报告文件列表（Setup: 创建 `a.txt` + `b.txt`）
- [x] 6.7 `TestMultiTool_Workflow_Full`：Read → 分析 → Write 多步流程（Setup: 创建 `data.csv`）

## 7. 场景测试：记忆子系统

- [x] 7.1 创建 `test/real_e2e/memory_test.go`（`//go:build real_e2e`）
- [x] 7.2 `TestMemoryWrite_Read_Smoke`：写入记忆后读回验证
- [x] 7.3 `TestMemoryCrossSession_Integration`：会话 A 写入，会话 B 读取验证持久化
- [x] 7.4 `TestSessionSearch_Basic_Smoke`：搜索历史会话消息

## 8. 场景测试：外部代理

- [x] 8.1 创建 `test/real_e2e/extagent_test.go`（`//go:build real_e2e`）
- [x] 8.2 `TestExtAgent_Invoke_Smoke`：调用已知外部代理
- [x] 8.3 `TestExtAgent_Error_Integration`：调用不存在的代理，验证错误处理

## 9. Fixtures 与文档

- [x] 9.1 创建 `test/real_e2e/fixtures/recordings/.gitkeep` 和 `test/real_e2e/fixtures/judge_cache/.gitkeep`
- [x] 9.2 创建 `test/real_e2e/fixtures/README.md`：说明录制文件和缓存的使用/重新生成方式

## 10. 端到端验证

- [x] 10.1 验证完整栈编译：`go build -tags=real_e2e ./test/real_e2e/...` ✓ 编译通过，现有 e2e 测试不受影响
- [ ] 10.2 录制一个 smoke 测试：`WORKHORSE_TEST_MODE=record WORKHORSE_JUDGE_MODE=off go test -tags=real_e2e -run TestFileRead_Basic_Smoke`（需要 DASHSCOPE_API_KEY）
- [ ] 10.3 回放验证：依赖 10.2 的录制文件
- [ ] 10.4 提交初始录制文件到 git（依赖 10.2）
