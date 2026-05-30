## Why

workhorse-agent 有 15+ 个 E2E 测试使用 `mockprovider`（脚本化 LLM 响应），覆盖协议合规、认证、SSE 流、会话机制——但从不调用真实 LLM。我们无法自动验证完整链路（用户消息 → DashScope API → agent loop → 工具调度 → 响应）端到端是否工作。

核心挑战：LLM 输出非确定性，传统字符串断言面对模型输出变化会频繁误报。需要一套测试策略：

1. 覆盖真实 provider 集成（DashScope qwen3.6-plus / glm-5）
2. 用 cross-model evaluation（GLM-5 评审主模型输出）替代字符串匹配，处理非确定性
3. 录制/回放机制控制成本和延迟（CI 用回放零 token，发布前用真实 API）
4. 可选执行：冒烟测试 30s、全套 10min，按需选取

## What Changes

- 新增 `test/real_e2e/` 包（独立于现有 `test/e2e/`，并存不干扰）
- RecordingProvider：`provider.Provider` 装饰器，支持 record/replay/live 三种模式
- SSE TraceCollector：从 HTTP SSE 事件流非侵入式收集交互轨迹
- LLM-as-Judge（GLM-5）：用 GLM-5 评估主模型交互轨迹，返回结构化 Verdict（PASS/FAIL/PARTIAL + 分数 + 推理）
- Rubric 体系：为每类测试场景定义评分标准和权重
- 测试场景：覆盖文件操作工具（Read/Write/Edit/Bash/Grep）、记忆子系统（memory_read/memory_write/session_search）、外部代理（ExternalAgent）
- JSONL 录制文件 + Judge 缓存文件，提交到 git，团队共享

## Capabilities

### New Capabilities

- `real-e2e-testing`: RecordingProvider record/replay 装饰器 + SSE TraceCollector + LLM-as-Judge 评估协议 + Rubric 评分标准 + 录制文件格式 + 执行模式控制（环境变量）+ 构建标签隔离

### Modified Capabilities

无。所有新代码在 `test/real_e2e/`，不修改任何生产代码或现有测试。

## Impact

- **新增代码**：~800 行 Go 测试基础设施 + ~300 行场景测试（不含录制文件）
- **工期估算**：2-3 天（10 个 task，每个 15-30 分钟）
- **新增依赖**：无（GLM-5 调用复用现有 Anthropic adapter 的 HTTP 直连方式）
- **新增配置**：3 个环境变量（`WORKHORSE_TEST_MODE`、`WORKHORSE_JUDGE_MODE`、`DASHSCOPE_API_KEY`）
- **构建标签**：`//go:build real_e2e`，`go test ./...` 默认不执行
- **成本**：replay + cached 模式 $0.00；live + llm 全套 ~$0.20
