## 1. 调研与基线

- [x] 1.1 grep `BuildSystemPrompt` 全部调用点，统计改造面，据此在 design D2a / D2b 间定夺并记录结论
- [x] 1.2 采集旧组装顺序下 system prompt 的 baseline 字节串：空 base / 有 base、有/无 environment、有/无 memory 各组合，作为「内容集合不变」的对照
- [x] 1.3 确认 `prompt` 包 `boundary_test` 允许的 import 集合，约束实现不引入新依赖

## 2. 实施顺序调整

- [x] 2.1 按选定方案修改组装逻辑：D2a 在 `internal/agent/loop.go:391-402` 反转为 `base → environment → memory` 的 append；或 D2b 在 `internal/prompt` 的 `SystemPrompt` 模板新增 `{{.Environment}}`/`{{.Memory}}` 占位符并调整 `BuildSystemPrompt` 签名
- [x] 2.2 若选 D2b，更新所有 `BuildSystemPrompt` 调用方传入结构化三段输入
- [x] 2.3 保留 memory 块的稳定分隔符，仅改变其在整体中的位置

## 3. 测试

- [x] 3.1 更新 `internal/prompt` 的 byte-stable 测试期望值为新顺序，并比对 1.2 的 baseline 证明仅顺序变化、内容集合一致
- [x] 3.2 新增 scenario 测试「静态 base 作为缓存前缀」：base+environment 相同、memory 不同的两输入，最长公共前缀覆盖整个 base+environment 段
- [x] 3.3 新增 scenario 测试「仅有 base 段」与「empty memory 无 memory section」
- [x] 3.4 确认同会话 memory 块逐字节稳定的断言仍通过

## 4. 验收

- [x] 4.1 `go build ./...` 与 `go test ./...` 全绿
- [x] 4.2 `boundary_test` 通过；`gofmt -l` 无输出；`golangci-lint run` 干净
- [x] 4.3 人工/集成层核对一次 provider 请求体 system 字段，确认 base 段位于最前
- [ ] 4.4 更新受影响 spec（agent-loop、prompt-memory）并归档 change
