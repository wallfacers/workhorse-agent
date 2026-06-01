# Design: 项目级会话持久化与多活并发

## 背景与现状盘点

真实持久化边界(已核对生产调用链):`sessions` 与 `events` 表已落盘,
**`messages` 表从未被生产代码写入**(`store.AppendMessage` 零生产调用,loop 只写
内存 `s.history`)。所以缺口分两层:先补 message 持久化地基(D9),再补 manager/api。

| 任务书能力 | store/sqlite(实况) | 缺口 |
|---|---|---|
| T1 重启后列出/重建 | `sessions`✅;`messages`**❌从不写** | message 落盘(D9)+ `ListSessions()` 只返回内存 |
| T2 history | `ListMessages` 存在但表为空 | message 落盘(D9)+ 端点 + `ContentBlock→parts[]`(D4) |
| T2 rename | — | 无 `title` 列;PATCH 端点不存在 |
| T2 delete 删 transcript | `DeleteSession` + CASCADE | 现走软删 `deleted_at` |
| T2 projects | 可由 `workdir` 聚合 | 端点不存在 |
| T3 status + 重开 idle | `State()`、`EventsAfter`、`MaxEventIdx` | `GetSession` 只查内存,无法水合 |
| T4 续聊真连续 | — | **message 落盘(D9,events 替代不了)** + 水合灌回上下文 |
| T5 补帧 | `events` + idx | `stream_get` 已支持 `Last-Event-ID`,基本满足 |

## 跨仓契约核对(已对 workhorse-assistant 验证)

已读 assistant 仓消费侧代码,核对结论:

- **Rust 桥全程透传**(`src-tauri/src/agent/mod.rs`:`into_json()` → `serde_json::Value`),
  不做强类型解析 ⇒ 字段契约**完全由前端 TS 锁定**,Rust 无需对齐,任务书 §9『回填』
  机制可用。
- **SessionMeta / ProjectMeta 字段逐字吻合** `src/ipc/agent.ts` 的 `AgentSessionMeta`
  / `AgentProjectMeta`(D1/D8 正确)。两个硬约束:① **`title` 非可选**,sidecar 必须
  永远带 `title`(可空串,不可省略);② **`status` 严格 `'idle'|'running'`**,绝不漏出
  6 态(D2 是硬约束)。
- **history 是裸 cast**(`SessionProvider.tsx` `coerceHistory` 仅校验 `.messages`
  是数组后直接当 `ChatMessage[]` 渲染,无 per-part 映射/校验)⇒ **`parts[]` 字段名
  对不上 = 静默渲染空,不报错**。两个由此而来的硬性要求落在 D4。

## 决策

### D1 — 线缆形状统一为 camelCase(取代 snake_case 双轨)

assistant 的硬契约是 camelCase。为求"最完美一致性",**所有会话相关端点**
(create/get/list/history/projects)统一用 camelCase 投影,而非新旧两套并存。
现有 `sessionView`(`parent_id`/`created_at`)重写为 `SessionMeta`;现有
`sessions_test.go` 断言同步更新。assistant 对现有端点仅读 `.id`,不受影响。

`SessionMeta`(create/get/list/PATCH 统一返回):

```json
{
  "id": "ses_01J...", "workdir": "/home/user/proj",
  "title": "重构登录流程", "status": "idle",
  "createdAt": "2026-05-31T08:00:00.000Z", "updatedAt": "2026-05-31T09:12:00.000Z",
  "messageCount": 42, "lastMessagePreview": "好的,我先看一下…"
}
```

`parentId`/`provider`/`model`/`agentType`/`ephemeral` 等内部字段保留为可选 camelCase
附加字段,不影响 assistant(它容忍多余字段)。

### D2 — status:6 态 → idle|running 投影

状态机 6 态 → 对外二元:`Idle`、`Cancelled` ⇒ `idle`;`Thinking`、`AwaitPerm`、
`Executing`、`Compacting` ⇒ `running`。投影函数集中在一处(`statusOf`),供所有
端点复用。**持久化但未加载**的会话一律按 `idle`(无在跑 turn)。

### D3 — 持久化水合(本变更的架构核心)

`Manager` 引入"按需水合"。命中未加载会话的入口有三:`GetSession`、
`GET …/stream`、`POST …/stream`。流程:

```
请求 id ──► m.sessions 命中? ──是──► 直接用(live)
                │否
                ▼
        store.GetSession(id) 命中且未删? ──否──► 404
                │是
                ▼
        rehydrate: 新建 Session(meta from row, state=Idle)
        ListMessages(id) ─► 重建 agent loop 的模型上下文(D5/T4)
        登记进 m.sessions ─► 之后与 live 会话同路径推进
```

水合出的会话 `status=idle`,直到收到 `user_message` 才转 `running`。
`ListSessions(workdir)` 不必把每个会话都水合成 live ——列举走 store 投影即可
(`messageCount`/`lastMessagePreview` 由聚合查询得到),只有真正交互的会话才水合。

并发安全:直接复刻 `CreateSession`(manager.go:90-116)已验证的"占位槽"模式——
`m.mu` 下若命中既有 `activeSession` 直接返回;否则塞入 `m.sessions[id] = nil` 占位、
解锁后做 store IO,成功则换成真正的 `activeSession`,失败则 `delete` 占位槽干净回滚。
注意:`nil` 占位槽当前会让并发的 `GetSession` 返回 `ErrNotFound`——水合场景需让"正在
水合"(短暂等待/重试)与"真不存在"区分开,否则一条 stream 在水合、另一条会误报 404。
`max_concurrent` 是否把水合计入额度需定夺(见开放问题)。

### D4 — `ContentBlock → parts[]` 映射(history)

存储里每条 message 的 `content_json` 是 `[]provider.ContentBlock`。映射规则:

| ContentBlock.Type | → part |
|---|---|
| `BlockText` | `{ "type":"text", "content": <text> }` |
| `BlockThinking`/reasoning | `{ "type":"reasoning", "text": <text>, "redacted": <bool> }` |
| `BlockToolUse` | `{ "type":"tool_call", "id":<id>, "name":<tool>, "input":<obj>, "status":"done"\|"error", "output":<...> }` |
| `BlockToolResult` | 合并进对应 `tool_call` 的 `output`/`status`(按 `ToolUseID` 关联),不单独成 part |

`reasoning` 持久化时 `status` 视为 `done`。tool_use 与其后续 tool_result 跨消息
关联:`tool_calls` 表虽有 `output_json`/`is_error`,但**它同样没人写**(与 messages
一样悬空),所以走"在 transcript 内按 `toolUseId` 用 map 回填"的扫描法更现实——
assistant 角色消息里的 `tool_use` 成 `tool_call` part(status 待定),后续 user 角色
消息里的 `tool_result` 不单独成 part,按 `toolUseId` 回填其 `output`/`status`
(`isError ? 'error' : 'done'`)。**最小可用**先产出 `text`(必要时加 `tool_call`)
即让 UI 重建可用,`reasoning` 可后补。

**⚠️ 两个由 assistant 裸 cast 推出的硬性要求**(前端 `src/session/types.ts` 的
`MessagePart` 逐字读这些键):

1. **wire 字段名 ≠ 存储字段名**:history 输出的 tool_call part 必须用 `id` / `name`
   / `input` / `output` / `status`,而**不是** D9 存储用的 `toolUseId` / `toolName`。
   history 端点必须**翻译** content_json → parts,不能把 messages 表原样吐出去
   (否则前端读 `id`/`name` 得 undefined,tool_call 渲染空白)。
2. **reasoning part 必须物理带 `status: "done"`**:前端 reasoning union 含 `status`
   字段;任务书"持久化时 status 视为 done"指的是该字段必须**出现在 JSON 里**,不是
   概念上 done。`text` part 用 `content`(非 `text`),reasoning part 用 `text`——别混。

### D5 — T4 续聊真连续

前提是 D9 已让 message 落盘。水合时调 `ListMessages(id)` 把历史 `provider.Message`
序列(经 D9 的 `content_json` 无损还原,含 thinking `signature`)重灌进 loop 的内存
消息切片(loop 既有的 thinking 剥离 / token 计账规则照常套用)。这样切回旧会话
发 `user_message`,模型带着完整历史续答,而非从零开始。

> ⚠️ events 表替代不了:thinking 的 `signature` 按设计不进 SSE/events,从 events
> 重建的历史缺签名会被 Anthropic round-trip 拒绝。故 T4 强依赖 D9。

### D9 — message 持久化(T1/T4 地基)

现状:`store.AppendMessage`(写 `messages` 表)生产零调用,transcript 只存在内存。
本变更让 `Session.AppendMessage` 在 `store != nil && !Ephemeral` 时落库——复刻
`Emit`/`assignIdx` 的判定,集中在 `Session` 一处,而非散落到 loop.go 的 6 个调用点。

`content_json` 序列化须无损 round-trip 全部 5 类 block:

| BlockType | 持久化字段 |
|---|---|
| `text` | `text` |
| `tool_use` | `toolUseId`、`toolName`、`input`(raw JSON) |
| `tool_result` | `toolUseId`、`output`、`isError` |
| `thinking` | `thinking`、**`signature`**(API round-trip 必需) |
| `redacted_thinking` | `redactedData` |

**暗礁——压缩会让 messages 表与模型上下文漂移**:loop 压缩后调
`ReplaceHistory(summarised)`,但 `messages` 是 append-only,不会跟着改;重启水合后
`ListMessages` 给的是压缩前全量,与压缩后真正喂模型的上下文不一致。取定方案:
**`ReplaceHistory` 时在事务里 `DeleteMessages(sessionID) + 批量重插`** 压缩后的序列,
保证"库 == 模型上下文"。代价:`GET …/history` 将看不到压缩点之前的原文——是否需要
为 UI 保留原始 transcript 列入开放问题(若需要则改"原文表 + 上下文快照"双层)。

**副作用(正向)**:message 落盘会激活 `messages_fts` 触发器,`session_search` 工具
从此对真实会话生效(当前它镜像空表,等于悬空)。本变更不专门处理该工具,但需在
测试中确认 FTS 行随 message 写入而填充。

bonus:`messageCount` / `lastMessagePreview`(SessionMeta)也由 `messages` 表聚合得到,
同样依赖 D9。

### D6 — DELETE 改硬删 + 级联(对齐 Claude Code / opencode)

Claude Code 把会话存成 `~/.claude/projects/<编码workdir>/<sessionId>.jsonl`,
删除会话=删那个文件;opencode 同理。映射到 sqlite:`DELETE` **硬删 `sessions`
行**,`messages`/`events`/`tool_calls` 经 `ON DELETE CASCADE` 一并清除。先取消
在跑的 turn(级联到工具/子进程/子 session)再删,语义对齐既有"销毁运行中会话"。
注意需确保连接开启 `PRAGMA foreign_keys=ON`(否则 CASCADE 不生效)。

### D7 — title 派生

新增 schema migration **v3**:`ALTER TABLE sessions ADD COLUMN title TEXT NOT NULL
DEFAULT ''`。title 在落第一条 user message 时派生(截取首条用户文本,做长度截断/
单行化);此后 `PATCH` 可覆盖。空串由 assistant 显示为"未命名会话"。

### D8 — `GET /v1/projects` 范围

只返回**有 session(未删除)**的 `workdir`(§8 开放问题3 选定)。本仓无独立项目
注册表,"注册过但零会话"的路径无来源,纳入需另起注册机制——本轮不做。
`ProjectMeta = { path, sessionCount, updatedAt }`,由聚合查询得到。

## 风险 / 待验证

- **message 持久化是新写路径**:`content_json` 序列化/反序列化必须与现有内存
  `provider.Message` 严格等价(尤其 thinking `signature`、`input` raw JSON 不被
  二次转义);加 round-trip 单测(Message → content_json → Message 全等)。落库失败
  的处理策略(吞掉只记日志 vs 阻断 turn)需定夺——倾向不阻断 turn,记录并继续。
- **压缩 / `ReplaceHistory` 的持久化一致性**:确认每条压缩路径都重写了 messages 表,
  否则水合后上下文漂移;并发下 turn 仍在跑时压缩与 AppendMessage 的落库顺序要稳。
- **camelCase 切换的回归面**:现有 `sessions_test.go`、可能的 e2e 断言需同步;
  务必 grep 全仓对 `created_at`/`parent_id` 等会话字段的消费点。
- **水合的并发竞态**:两条并发 stream 同时打开同一未加载会话,需 double-check
  锁保证只水合一次;水合失败(store 错误)要干净回滚不留半个 live 会话。
- **CASCADE 生效前提**:确认 sqlite 连接 `foreign_keys` pragma 已开。
- **大 transcript 的 history**:本轮一次性拉全量(无分页),超长会话内存/带宽风险
  记入开放问题,前向兼容预留游标参数空间。

## 开放问题(实现时定夺并回填任务书 §8/§9)

1. title 截断规则(字符上限 / 是否去首尾空白 / 多行折叠)。
2. history 是否预留 `?cursor=`/`?limit=` 以便日后分页(当前全量)。
3. 水合出的会话在多久无活动后可从内存回收(避免内存里堆积所有历史会话)。
