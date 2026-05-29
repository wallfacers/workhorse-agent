## Context

现有 E2E 测试（`test/e2e/`）使用 `mockprovider`（FIFO 队列回放脚本化响应），验证 HTTP 协议、认证、SSE 流、会话状态机——但不验证 LLM 集成。真实链路中，模型的工具调用决策、输出格式、错误处理都依赖于模型能力，mock 测试无法覆盖。

项目通过 DashScope Anthropic 兼容 API 接入 `qwen3.6-plus`（主模型）和 `glm-5`（评审模型）。需要一套利用这两个模型的自动化测试。

## Goals / Non-Goals

**Goals:**

- 真实 LLM 端到端测试：用户消息 → DashScope API → agent loop → 工具执行 → SSE 响应
- Cross-model evaluation：GLM-5 评审主模型输出，替代字符串匹配
- 录制/回放：CI 零 token 稳定运行；发布前真实 API 验证
- 可选执行：smoke / integration / full 三级；build tag 隔离
- 与现有 mock E2E 测试并存，零侵入

**Non-Goals:**

- 替换现有 mockprovider 测试（两者互补）
- 模型性能基准测试或 benchmark
- 自动重新录制（模型升级后需手动 `WORKHORSE_TEST_MODE=record`）
- CI 中自动调用真实 API（CI 只跑 replay + cached）
- 多模型交叉评审矩阵（MVP 只用 GLM-5 评审）

## Decisions

### D1 · 非侵入式 Trace 收集：消费 SSE 事件流

不修改 agent loop、orchestrator 或任何生产代码。TraceCollector 从测试 HTTP 服务器的 SSE 端点收集事件，与真实客户端使用相同的接口。

**vs 内部 hook**：修改 agent loop 添加 hook 更精确，但破坏"测试不侵入生产代码"原则。SSE 是公开协议，测试行为 = 客户端行为，最真实。

### D2 · LLM-as-Judge：用 GLM-5 评估主模型输出

传统字符串匹配 / 正则断言面对 LLM 输出的非确定性会频繁误报。用 GLM-5 做结构化评估（Verdict + Score + Reasoning）：

- Judge prompt 包含：用户消息、完整交互轨迹（工具调用 + 结果 + 模型输出）、评分标准（Rubric）
- Judge 返回 JSON：`{verdict, score, reasoning, suggestions}`
- Verdict 语义：PASS（≥ MinScore）、FAIL（低于阈值或 Required 项失败）、PARTIAL（接近阈值，可重试）

**vs 多模型交叉评审矩阵**：MVP 用单一评审模型（GLM-5），足够覆盖。多模型矩阵（如 qwen 评 glm、glm 评 qwen）是 V2。

### D3 · 录制/回放：Provider 装饰器

RecordingProvider 实现 `provider.Provider` 接口，包装真实 Provider（Anthropic adapter）。三模式：

| 模式 | 行为 | 用途 |
|---|---|---|
| `replay` | 从 JSONL 读取预录响应 | CI |
| `record` | 调真实 API + 写 JSONL | 发布前/调试 |
| `live` | 调真实 API，不录制 | 快速手动验证 |

录制文件格式：JSONL，第一行 header（test name + model + timestamp），后续每行一个 `Stream()` 调用（request + events）。

### D4 · 分层可选执行

三个独立控制轴：

| 轴 | 环境变量 | 值 | 默认 |
|---|---|---|---|
| Provider 模式 | `WORKHORSE_TEST_MODE` | `replay`/`record`/`live` | `replay` |
| Judge 模式 | `WORKHORSE_JUDGE_MODE` | `llm`/`cached`/`off` | `cached` |
| 测试级别 | `-run` 正则 | `*_Smoke`/`*_Integration`/`*_Full` | 全部 |

构建标签：所有文件统一 `//go:build real_e2e`，`go test ./...` 不触发。

### D5 · Rubric 体系：每类场景独立评分标准

Rubric 由一组 Criterion（name + description + weight + required）组成。Judge 按每项 Criterion 评分，加权求和得总分。任何 Required 项失败 → 整体 FAIL。

三级场景覆盖：
- **文件工具**：Read/Write/Edit/Bash/Grep + 多工具组合流程
- **记忆子系统**：memory_read/memory_write/session_search + 跨会话持久化
- **外部代理**：ExternalAgent 调用 + 错误处理

### D6 · Judge 缓存：确定性哈希键

Judge 结果按 `(trace JSON + rubric JSON)` 的 SHA-256 哈希缓存。相同轨迹 + 相同标准 = 命中缓存，不调 API。缓存文件提交到 git。

### D7 · 非确定性重试：场景级重跑

`VerdictPartial` 时整个场景重跑（新 session + 同输入），最多 `MaxRetries` 次。不在单个 LLM 调用级别重试——非确定性在场景层面。

## Risks / Trade-offs

| 风险 | 缓解 |
|---|---|
| GLM-5 Judge 本身可能不稳定 | 缓存机制 + `cached` 模式 CI 不依赖实时 Judge；`MaxRetries` 应对偶发 PARTIAL |
| 录制文件随模型版本漂移 | header 记录 model name，replay 时 drift warning；模型升级后手动重录 |
| Judge prompt 对某些场景评估不准 | Rubric 可细化调整；Required 字段保底关键项 |
| 测试耗时长（LLM 推理 + Judge 推理） | replay 模式秒级；live 模式 smoke < 60s, full < 5min |
| JSONL 录制文件体积 | 每 turn 一行 JSON；典型场景 < 50KB |

## Migration Plan

不涉及迁移。新增 `test/real_e2e/` 目录，不修改任何现有代码。

## Open Questions

- **Judge prompt 优化**：初始 prompt 可能在复杂场景下评估不准，需实测后迭代
- **录制文件 drift 自动检测**：当前仅 log warning，是否需要 CI check 阻断过期录制
- **多 Judge 模型支持**：当前硬编码 GLM-5，未来是否需要可配置 Judge 模型
