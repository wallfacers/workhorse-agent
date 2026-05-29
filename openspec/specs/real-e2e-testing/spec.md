# real-e2e-testing Specification

## Purpose

为 workhorse-agent 提供真实 LLM 驱动的端到端测试框架，使用 LLM-as-Judge（GLM-5 评审主模型输出）替代传统字符串断言，配合录制/回放机制实现 CI 零成本稳定运行。

## Requirements

### Requirement: RecordingProvider 装饰器

`RecordingProvider` SHALL 实现 `provider.Provider` 接口，包装真实 Provider，支持三种模式：

| 模式 | `Stream()` 行为 |
|---|---|
| `replay` | 从 JSONL 文件读取预录响应，通过 channel 回放 |
| `record` | 调用内部 Provider 的 `Stream()`，收集所有事件，序列化到 JSONL 文件 |
| `live` | 纯透传到内部 Provider，不录制不回放 |

`RecordingProvider` SHALL 通过环境变量 `WORKHORSE_TEST_MODE` 决定模式，默认 `replay`。

`replay` 模式下若 JSONL 文件不存在 SHALL 返回错误（测试调用 `t.Skip`）。

`record` 模式下 SHALL 在 `Save()` 调用时原子写入 JSONL 文件。

#### Scenario: replay 模式回放预录响应

- **WHEN** `WORKHORSE_TEST_MODE=replay`，JSONL 文件含 2 轮录制
- **THEN** `Stream()` 前两次调用返回录制的响应，第三次返回 `{EventStop, stop_reason: "end_turn"}`（兜底）

#### Scenario: record 模式录制真实响应

- **WHEN** `WORKHORSE_TEST_MODE=record`，内部 Provider 返回 3 个事件
- **THEN** `Stream()` 透传 3 个事件到调用方，同时追加 1 行 JSON 到 JSONL 文件

#### Scenario: replay 模式无录制文件

- **WHEN** `WORKHORSE_TEST_MODE=replay`，`Load()` 找不到 JSONL 文件
- **THEN** `Load()` 返回错误，测试框架调用 `t.Skip`

### Requirement: 录制文件格式

录制文件 SHALL 使用 JSONL 格式（每行一个 JSON 对象）：

- **第 1 行**（header）：`{"test": "<TestName>", "model": "<model>", "recorded_at": "<RFC3339>"}`
- **第 2+ 行**（turn）：`{"request": {...}, "events": [...]}`

每个 turn 行的 `request` 字段 SHALL 包含完整的 `provider.Request`（model, system, messages, tools, max_tokens）。

每个 turn 行的 `events` 字段 SHALL 包含完整的 `[]provider.ProviderEvent`，按原始顺序。

#### Scenario: 录制文件结构校验

- **WHEN** 对 `TestFileRead_Basic_Smoke` 执行 record 模式
- **THEN** 生成 `TestFileRead_Basic_Smoke.jsonl`，第 1 行为 header 含 `"test": "TestFileRead_Basic_Smoke"`，第 2 行起每行含 `"request"` 和 `"events"` 字段

### Requirement: SSE TraceCollector

TraceCollector SHALL 从 HTTP SSE 事件流（与真实客户端相同的 GET 端点）收集交互轨迹，组装为 `Trace` 结构：

```
Trace {
  TestName    string
  UserMessage string
  Turns      []Turn
}

Turn {
  ModelOutput string
  ToolCalls   []ToolCallRecord
  ToolResults []ToolResultRecord
  Duration    time.Duration
}
```

收集规则：

- `assistant_text_delta` 事件的 `delta` 字段 SHALL 累加到 `Turn.ModelOutput`
- `tool_call_start` 事件的 `tool_name` SHALL 追加到 `Turn.ToolCalls`
- `tool_call_done` 事件的 `tool_name`、`output`、`is_error` SHALL 追加到 `Turn.ToolResults`
- `assistant_text_done` 事件 SHALL 终止当前 Turn 并开始新 Turn
- `assistant_text_done` 事件 `stop_reason == "end_turn"` SHALL 终止收集
- 收集超时 SHALL 可配置（默认 60s），超时后返回已收集的 Trace

#### Scenario: 纯文本响应收集

- **WHEN** SSE 流依次发送 `assistant_text_delta{delta:"hello "}` → `assistant_text_delta{delta:"world"}` → `assistant_text_done{stop_reason:"end_turn"}`
- **THEN** Trace 含 1 个 Turn，`ModelOutput == "hello world"`，无 ToolCalls

#### Scenario: 工具调用收集

- **WHEN** SSE 流发送 `tool_call_start{tool_name:"Read"}` → `tool_call_done{tool_name:"Read",output:"file content"}` → `assistant_text_delta{delta:"The file..."}` → `assistant_text_done{stop_reason:"end_turn"}`
- **THEN** Trace 含 1 个 Turn，`ToolCalls[0].ToolName == "Read"`，`ToolResults[0].Output == "file content"`，`ModelOutput == "The file..."`

### Requirement: LLM-as-Judge 评估协议

Judge SHALL 实现 `Judge` 接口：

```go
type Judge interface {
    Evaluate(ctx context.Context, trace *Trace, rubric Rubric) (*JudgeResult, error)
}
```

`JudgeResult` SHALL 包含：

| 字段 | 类型 | 语义 |
|---|---|---|
| `Verdict` | `PASS`/`FAIL`/`PARTIAL` | PASS = ≥ MinScore 且无 Required 项失败；FAIL = 低于阈值或 Required 失败；PARTIAL = 接近阈值可重试 |
| `Score` | `float64` (0.0-1.0) | 所有 Criterion 评分的加权和 |
| `Reasoning` | `string` | Judge 的评估推理过程 |
| `Suggestions` | `[]string` | 改进建议 |

Judge SHALL 构造包含以下内容的 prompt 发送给评审模型：

1. 原始用户消息
2. 完整交互轨迹（所有 Turn 的 ModelOutput + ToolCalls + ToolResults）
3. Rubric 的所有 Criterion（name + description + weight + required）
4. MinScore 阈值
5. 要求返回 JSON 格式 `{verdict, score, reasoning, suggestions}` 的指令

Judge SHALL 从评审模型响应中提取 JSON（支持 markdown code block 包裹）。

#### Scenario: Judge 返回 PASS

- **WHEN** Trace 显示模型正确调用了 Read 工具并准确报告文件内容，Rubric MinScore=0.7
- **THEN** Judge 返回 `{verdict: "PASS", score: 0.85, reasoning: "模型正确调用了 Read..."}`

#### Scenario: Judge 返回 FAIL（Required 项失败）

- **WHEN** Trace 显示模型未调用任何工具而是直接幻觉答案，Rubric 中 `tool_call_correct` 为 Required
- **THEN** Judge 返回 `{verdict: "FAIL", score: 0.2, reasoning: "未调用 Read 工具..."}`

### Requirement: GLM-5 Judge 实现

GLM-5 Judge SHALL 通过 DashScope Anthropic 兼容 API（`/apps/anthropic`）调用 `glm-5` 模型。

API 配置 SHALL 从环境变量读取：

| 环境变量 | 用途 | 默认值 |
|---|---|---|
| `DASHSCOPE_API_KEY` | API 密钥 | 无（live/record 模式必须设置） |
| `DASHSCOPE_BASE_URL` | API 地址 | `https://coding.dashscope.aliyuncs.com/apps/anthropic` |

HTTP 请求 SHALL 设置 `Anthropic-Version: 2023-06-01` header。

HTTP 请求 SHALL 设置 `max_tokens: 1024`。

Judge 调用超时 SHALL 为 60 秒。

#### Scenario: GLM-5 Judge 调用成功

- **WHEN** `DASHSCOPE_API_KEY` 已设置，调用 `Evaluate()`
- **THEN** 向 DashScope 发送 Anthropic 格式请求（model="glm-5"），解析响应中的 JSON，返回 `JudgeResult`

### Requirement: Rubric 评分标准

Rubric SHALL 包含：

```go
type Rubric struct {
    Criteria   []Criterion
    MinScore   float64  // 最低通过分数
    MaxRetries int      // VerdictPartial 最大重试次数
}

type Criterion struct {
    Name        string
    Description string
    Weight      float64  // 权重，所有 Criterion 权重之和应为 1.0
    Required    bool     // 任一 Required 项失败则整体 FAIL
}
```

场景 Rubric 定义：

**文件工具**（MinScore: 0.7, MaxRetries: 2）：

| Criterion | Weight | Required |
|---|---|---|
| `tool_call_correct` — 模型是否调用了正确的工具和参数 | 0.3 | 是 |
| `response_accuracy` — 回复是否准确反映工具结果 | 0.35 | 是 |
| `no_hallucination` — 是否避免了幻觉 | 0.2 | 是 |
| `efficiency` — 是否避免了不必要的额外工具调用 | 0.15 | 否 |

**记忆子系统**（MinScore: 0.8, MaxRetries: 1）：

| Criterion | Weight | Required |
|---|---|---|
| `tool_invocation` — 是否正确使用 memory_read/memory_write | 0.3 | 是 |
| `data_integrity` — 读回数据是否与写入一致 | 0.4 | 是 |
| `cross_session` — 新会话是否能看到之前写入的数据 | 0.3 | 是 |

**外部代理**（MinScore: 0.7, MaxRetries: 2）：

| Criterion | Weight | Required |
|---|---|---|
| `correct_invocation` — 是否调用了正确的代理和参数 | 0.4 | 是 |
| `output_handling` — 是否正确处理和传达代理输出 | 0.3 | 是 |
| `error_recovery` — 失败时是否给出有用解释 | 0.3 | 否 |

### Requirement: Judge 缓存

Judge 结果 SHALL 缓存到 `test/real_e2e/fixtures/judge_cache/` 目录。

缓存键 SHALL 为 `SHA-256(Trace JSON || Rubric JSON)` 的前 16 个十六进制字符。

缓存文件格式 SHALL 为 JSON：`{verdict, score, reasoning, suggestions}`。

`WORKHORSE_JUDGE_MODE=cached` 时 SHALL 仅读取缓存，不调用 API；缓存未命中 SHALL 导致 `t.Skip`。

`WORKHORSE_JUDGE_MODE=llm` 时 SHALL 调用真实 API 并更新缓存。

`WORKHORSE_JUDGE_MODE=off` 时 SHALL 跳过 Judge 评估，`assertVerdict` 直接通过。

#### Scenario: 缓存命中

- **WHEN** `WORKHORSE_JUDGE_MODE=cached`，缓存文件存在且 hash 匹配
- **THEN** 不调用 GLM-5 API，直接返回缓存的 `JudgeResult`

#### Scenario: 缓存未命中

- **WHEN** `WORKHORSE_JUDGE_MODE=cached`，缓存文件不存在
- **THEN** 测试 `t.Skip`，提示需用 `WORKHORSE_JUDGE_MODE=llm` 生成缓存

### Requirement: 执行模式控制

三个独立控制轴：

**Provider 模式**（`WORKHORSE_TEST_MODE`）：

| 值 | 行为 | 需要 API Key |
|---|---|---|
| `replay`（默认） | 从 JSONL 回放 | 否 |
| `record` | 调真实 API + 录制 | 是 |
| `live` | 调真实 API，不录制 | 是 |

无 API Key 且模式需要时 SHALL `t.Skip`。

**Judge 模式**（`WORKHORSE_JUDGE_MODE`）：

| 值 | 行为 | 需要 API Key |
|---|---|---|
| `cached`（默认） | 读缓存 | 否 |
| `llm` | 调 GLM-5 API | 是 |
| `off` | 跳过 Judge | 否 |

**测试级别**（`-run` 正则）：

| 级别 | 命名模式 | 典型超时 |
|---|---|---|
| smoke | `Test*_Smoke*` | 60s |
| integration | `Test*_Integration*` | 90s |
| full | `Test*_Full*` | 120s |

#### Scenario: CI 模式（零成本）

- **WHEN** `WORKHORSE_TEST_MODE=replay WORKHORSE_JUDGE_MODE=cached go test -tags=real_e2e ./test/real_e2e/...`
- **THEN** 所有测试从 JSONL 回放 LLM 响应，Judge 从缓存读取，零 API 调用，零 token 消耗

#### Scenario: 发布验证模式

- **WHEN** `WORKHORSE_TEST_MODE=live WORKHORSE_JUDGE_MODE=llm go test -tags=real_e2e ./test/real_e2e/...`
- **THEN** 所有测试调用真实 DashScope API，Judge 调用 GLM-5，完整验证

#### Scenario: 录制模式

- **WHEN** `WORKHORSE_TEST_MODE=record WORKHORSE_JUDGE_MODE=off go test -tags=real_e2e -run TestFileRead_Basic_Smoke ./test/real_e2e/...`
- **THEN** 调用真实 API 并生成 JSONL 录制文件，跳过 Judge 评估

### Requirement: 构建标签隔离

所有 `test/real_e2e/` 下的 `.go` 文件 SHALL 使用 `//go:build real_e2e` 构建标签。

`go test ./...`（不带 `-tags=real_e2e`）SHALL 完全跳过 `test/real_e2e/` 目录。

`go test -tags=real_e2e ./test/real_e2e/...` SHALL 编译并运行真实 E2E 测试。

#### Scenario: 默认测试不触发真实 E2E

- **WHEN** 执行 `go test ./...`
- **THEN** 不编译不执行 `test/real_e2e/` 下的任何文件

### Requirement: 非确定性重试

当 Judge 返回 `VerdictPartial` 时，测试框架 SHALL 用相同输入重新运行整个场景（新 session + 新 SSE 连接），最多 `Rubric.MaxRetries` 次。

重试 SHALL 在场景级别进行（完整重跑），不在单个 LLM 调用级别重试。

所有重试次数耗尽后若仍为 `VerdictPartial` SHALL 视为 `VerdictFail`。

#### Scenario: 重试后通过

- **WHEN** 第 1 次 Judge 返回 PARTIAL（score=0.68, MinScore=0.7），MaxRetries=2
- **THEN** 重跑场景；若第 2 次 Judge 返回 PASS（score=0.75），测试通过

#### Scenario: 重试耗尽

- **WHEN** 连续 3 次 Judge 返回 PARTIAL，MaxRetries=2
- **THEN** 测试失败，输出最后一次 Judge 的 Reasoning

### Requirement: 录制 drift 检测

`replay` 模式加载 JSONL 时 SHALL 读取 header 中的 `model` 字段，与当前配置的模型名比较。

若模型名不一致 SHALL 在测试日志中输出 warning（`t.Log` 级别），但不终止测试。

#### Scenario: 模型升级后 drift warning

- **WHEN** JSONL header 记录 `model: "qwen3.6-plus"`，当前配置模型为 `"qwen4.0-plus"`
- **THEN** 测试日志输出 `"Recording drift: recorded with qwen3.6-plus, current model is qwen4.0-plus"`，测试继续

### Requirement: 与现有测试并存

`test/real_e2e/` SHALL 不导入 `test/e2e/` 的任何代码。

`test/real_e2e/` SHALL 不修改 `test/e2e/` 的任何文件。

两个目录 SHALL 可独立运行：`go test ./test/e2e/...` 和 `go test -tags=real_e2e ./test/real_e2e/...` 互不影响。

#### Scenario: 并存运行

- **WHEN** 先执行 `go test ./test/e2e/...`，再执行 `go test -tags=real_e2e ./test/real_e2e/...`
- **THEN** 两套测试独立运行，无共享状态，结果互不影响
