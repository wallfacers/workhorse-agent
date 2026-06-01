## Context

workhorse-agent 的 system prompt 当前由四段按固定顺序拼接：`BasePrompt → CancelledNote → Environment → Memory`。其中 BasePrompt + CancelledNote 是静态前缀（所有 session 共享），Environment 和 Memory 是动态段（每 session 不同但 session 内不变）。

项目目前没有项目级指令文件发现机制。用户无法在项目目录放置 AGENTS.md 来传达项目特定的编码规范、架构约束或行为偏好。Memory 子系统（MEMORY.md / USER.md）仅支持全局级 `~/.workhorse-agent/memories/`，且字符数受限，不适合放置项目级的大段配置。

opencode 的 instruction.ts 实现了完整的 AGENTS.md 支持，包含系统级注入和 Read 工具就近注入两种通道，是该功能的事实标准参考。

## Goals / Non-Goals

**Goals:**

- 支持从项目 workdir 向上发现 AGENTS.md / CLAUDE.md 文件，注入 system prompt
- 支持全局级 `~/.workhorse-agent/AGENTS.md` 作为用户级默认指令
- Read 工具读取文件时，自动注入附近子目录中的 AGENTS.md（proximity injection）
- Session 级别去重，避免同一文件重复注入
- 遵循现有 prompt-cache 友好的模板组装模式

**Non-Goals:**

- 不支持 config.yaml 中的自定义指令路径 / glob / URL（留给后续扩展）
- 不支持热重载（指令在 session 创建时冻结，和 memory 一致）
- 不对 AGENTS.md 内容设置字符数上限（人写文件，不截断）
- 不修改 Edit、Write、Glob、Grep 等工具（仅 Read 做就近注入）

## Decisions

### D1: 文件名搜索策略 — First filename wins

**决策**: 搜索列表 `["AGENTS.md", "CLAUDE.md"]`。在项目目录树中，第一个有匹配的文件名类型胜出，不再搜索后续文件名。

**理由**: 和 opencode 策略一致。避免同一项目中混合 AGENTS.md 和 CLAUDE.md 导致的歧义。AGENTS.md 是市场标准，CLAUDE.md 仅为兼容 Claude Code 用户习惯的 fallback。

**替代方案**: 搜索所有文件名并合并 → 拒绝，因为多个不同文件名可能产生矛盾指令，且语义不清晰。

### D2: Prompt 模板位置 — Environment 和 Memory 之间

**决策**: `<instructions>` 段插入在 `<environment>` 之后、`<memory>` 之前。

**理由**:
- Environment（系统/工具信息）和 Instructions（用户项目偏好）是不同类别，分开更清晰
- Instructions 和 Memory 都是「用户定义的内容」，归为一组放在动态段
- Instructions 在 Memory 前更自然：先项目级约束，再个人记忆
- 不影响静态前缀（Base + CancelledNote）的 cache 命中率

**模板变更**:
```
Before: Base → Cancel → Env → Memory
After:  Base → Cancel → Env → Instructions → Memory
```

### D3: 就近注入通过 Read 输出追加实现

**决策**: 在 Read 工具返回的 `tools.Result.Output` 字符串末尾追加 `<system-reminder>` 块。

**理由**:
- 不需要修改 `tools.Result` 结构体或 `provider.ContentBlock` 类型
- `<system-reminder>` 是 Claude 等模型已认知的标记格式
- 和 Claude Code 处理 CLAUDE.md 的方式一致
- 注入内容走 tool_result 通道，不碰 system prompt，不破坏 prompt cache

### D4: Resolver 状态存储在 Session 上

**决策**: `InstructionResolver` 存储在 `session.Session` 结构体上，通过 `tools.Env` 传递给 Read 工具。

**理由**: Resolver 需要跟踪哪些路径已注入（去重），这个状态的生命周期是 session 级别的。和 `MemorySnapshot` / `EnvSnapshot` 的存储模式一致。

**替代方案**: 存在 Loop 上 → 可行但 Loop 不暴露给工具层，需要额外传参。存在独立全局 map → 需要 session ID 做键，增加 GC 负担。

### D5: 项目级 walk 范围 — workdir 到 git root

**决策**: 从 session workdir 向上 walk，找到第一个包含 `.git` 目录的祖先目录作为上界。

**理由**: monorepo 场景下 workdir 可能在子目录，但指令文件通常在 repo root。walk 到 git root 而不是 filesystem root 避免意外加载系统级文件。

**fallback**: 如果没有 git root（非 git 项目），walk 到 workdir 本身（只检查 workdir 这一层）。

### D6: 全局文件 — ~/.workhorse-agent/AGENTS.md

**决策**: 单一文件 `~/.workhorse-agent/AGENTS.md`，不存在则跳过。

**理由**: 方案 A（根目录直接放文件）比方案 B（子目录）更简洁，和项目根目录的 AGENTS.md 同名同格式，直觉一致。未来如需多个文件可通过 config.yaml 的 `instructions` 字段扩展。

## Risks / Trade-offs

**[Risk] AGENTS.md 内容可能很大 → 无上限**
→ Mitigation: 这是用户主动写的项目配置，和 `.editorconfig` 或 `tsconfig.json` 同类。如果文件大到影响上下文窗口，那是用户的问题而非系统问题。Read 工具输出有 1MiB truncation 保护，但系统提示词注入路径不受此限制。

**[Risk] Walk 到 git root 可能读取到 workdir 之外的文件**
→ Mitigation: 只读取文件内容到 system prompt，不执行文件。Read 工具就近注入的路径仍然在 workdir 内（walk 从被读文件向上到 workdir root，不超出 session workdir）。系统级加载可以超出 workdir（到 git root），这是预期行为。

**[Risk] prompt cache 分片 — 不同项目不同 instructions 内容**
→ Mitigation: Instructions 在 Environment 之后（已经是动态段），不影响 Base+CancelledNote 静态前缀的 cache 命中。同一 session 内 instructions 不变，跨 turn cache 仍然有效。

**[Trade-off] Session 级去重 vs Per-message 去重**
→ 选择 session 级（同一文件整个 session 只注入一次）。更简单，覆盖 95% 用例。极端场景（同一文件被多个子代理并发读取）可能遗漏，但实际风险低。
