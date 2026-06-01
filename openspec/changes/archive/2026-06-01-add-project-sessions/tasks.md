# Tasks: 项目级会话持久化与多活并发

## 0. message 持久化(T1/T4 地基,先做)

- [x] 0.1 `content_json` 序列化:实现 `provider.Message ↔ content_json` 无损
  round-trip,覆盖全部 5 类 block(`text`/`tool_use`/`tool_result`/`thinking` 含
  `signature`/`redacted_thinking`);加 round-trip 单测(Message→json→Message 全等)。
  → `internal/session/transcript.go`(`storedBlock` DTO + `marshalContent`/`unmarshalContent`)。
- [x] 0.2 `Session.AppendMessage` 在 `store != nil && !Ephemeral` 时落库(复刻
  `Emit`/`assignIdx` 判定),消息 id 用 ULID,`created_at` 单调;落库失败记录但不阻断 turn。
  → 排序键是 µs 精度的 `created_at`(ULID 仅同-µs 兜底);失败走 `slog.Error` 不阻断。
- [x] 0.3 压缩一致性:`Session.ReplaceHistory` 在事务里 `DeleteMessages(sessionID)
  + 批量重插`,保证 messages 表 == 模型上下文;补测试覆盖压缩后水合。
  → 新增 `store.Store.ReplaceMessages`(sqlite 用单事务 DELETE+INSERT);ReplaceHistory
  以 per-index µs 偏移保序;fakeStore 同步实现该接口方法。
- [x] 0.4 确认 message 写入触发 `messages_fts`(`session_search` 从此对真实会话
  生效),补一条 FTS 行填充断言。→ `TestMessage_AppendPopulatesFTS` 守护
  AppendMessage→trigger→FTS 组合(trigram MATCH)。

## 1. 持久化 schema 与查询(底座)

- [x] 1.1 加 migration v3:`sessions` 增 `title TEXT NOT NULL DEFAULT ''`;补
  `idx_sessions_workdir`(按 workdir 列举用);迁移 up/down 与测试。
  → `store.Session.Title` + v3 迁移 + CRUD 全链路;`TestSession_Title`。
- [x] 1.2 确认 sqlite 连接开启 `PRAGMA foreign_keys=ON`,补测试断言 DELETE 级联
  真正清掉 `messages`/`events`/`tool_calls`。→ pragma 已在 Open(`SetMaxOpenConns(1)`
  保证生效);新增 `PurgeSession`(硬删,先删 messages 触发 FTS trigger 再删 session
  级联 events/tool_calls);`TestSession_PurgeCascades`。
- [x] 1.3 `store`:新增按 `workdir` 列举(含 `messageCount`/`lastMessagePreview`
  聚合)、`projects` 聚合(distinct workdir + count + max updated_at)、`title`
  读写(`UpdateSession` 覆盖)。→ `ListSessionsByWorkdir`(extract_text 取末条预览)、
  `ListProjects`、`store.SessionSummary`/`store.Project`;`TestListSessionsByWorkdir`/
  `TestListProjects`。

## 2. 持久化水合(T1/T3/T4 核心)

- [x] 2.1 `Manager`:实现"按需水合"——新增 `GetOrHydrate`,未命中内存时回源 store
  重建 `Session`(state=Idle)。并发收口改为**全程持 `m.mu`**(store 已 `SetMaxOpenConns(1)`
  串行化,持锁做快速本地读最简且无竞态);`GetSession` 保持 live-only 避免读路径起 runner。
  → `TestManager_HydratesPersistedSession`、`buildHydrated`。
- [x] 2.2 水合时调 `ListMessages` 把历史 `provider.Message` 重灌进上下文,实现 T4。
  → 新增 `Session.RestoreHistory`(**非持久化**载入,避免每次重开 churn messages 表);
  测试断言 history 还原且 transcript id 不变。
- [x] 2.3 `GET …/stream` 与 `POST …/stream` 对"已存在但未加载(含 idle)"会话工作:
  改用 `GetOrHydrate`。→ `TestStreamPost_HydratesPersistedSession`、
  `TestStreamGet_HydratesPersistedSession`。
- [x] 2.4 水合失败干净回滚(持锁内未登记即无残留);store `ErrNotFound`→`session.ErrNotFound`
  →404,其它 store 错误→500。→ `writeSessionLookupError`、
  `TestManager_GetOrHydrate_MissingAndDeleted`。
  - 注:provider 名未持久化,水合用 runner factory 的默认 provider(沿用改动前行为);
    水合会话不计入 `max_concurrent`(打开旧会话应总成功)——两者列为开放问题。

## 3. status 投影与多活并发(T3)

- [x] 3.1 `statusOf`:6 态 → `idle|running` 投影(`Idle`/`Cancelled`→idle,其余
  →running),集中一处供所有端点复用;持久化未加载会话按 idle。
  → `Session.Status()` 方法 (session.go:525);`sessionMeta` 使用 camelCase。
- [x] 3.2 验证一个会话的 turn 服务端独立推进,与订阅者数量/有无订阅者无关;
  补多活并发测试(A、B 两会话并发各开 stream,互不干扰,running 在列表可标出)。
  → 独立推进由后台 runner(background ctx)+ 缓冲 Outbox 保证,已被现有
  `TestStreamGet_LastEventID_*`(无订阅者时仍落 5 事件)覆盖;`TestListByWorkdir_MultiActiveStatus`
  断言同项目下 A=running/B=idle 状态各自正确。

## 4. 五个 HTTP 端点 + camelCase 线缆(T2)

- [x] 4.1 重写会话投影为 camelCase `SessionMeta`(create/get/list/PATCH 统一返回);
  同步改 `internal/api/sessions_test.go` 等现有断言;grep 全仓对旧 snake 字段消费点。
  → `sessionMeta` 类型 + `metaFromLive`/`metaFromSummary` 投影函数。
- [x] 4.2 `GET /v1/sessions?workdir=<path>` → `{ "sessions": [SessionMeta] }`,
  source-of-truth 为 store(重启后仍可列出);无 workdir 时的行为与既有兼容。
  → `handleListSessions` 分支 `listSessionsByWorkdir`。
- [x] 4.3 `GET /v1/sessions/{id}/history` → `{ "messages": [HistoryMessage] }`,
  实现 `content_json → parts[]` **翻译**(非原样吐):wire 用 `id`/`name`/`input`/
  `output`/`content`/`text`,**不是**存储的 `toolUseId`/`toolName`;tool_call 按
  `toolUseId` 在 transcript 内 map 回填 output/status;reasoning part **物理带**
  `status:"done"`。
  → `handleHistory` + `buildHistory` + `unmarshalContentBlocks`。
- [x] 4.3a 投影不变量:`SessionMeta.title` 始终输出(可空串不可省略);
  `SessionMeta.status` 严格 ∈ `{idle,running}`(6 态投影后绝不漏出原态)。补单测。
  → `sessionMeta.Title` 为 `string`(非指针),零值为空串,始终序列化。
- [x] 4.4 `PATCH /v1/sessions/{id}` body `{ "title" }` → 更新后的 `SessionMeta`。
  → `handleRenameSession`(live + store 双路径)。
- [x] 4.5 `DELETE /v1/sessions/{id}`:改硬删 + 级联清 transcript;先取消在跑 turn
  再删;返回 2xx 空体。
  → `manager.DeleteSession` 已用 `PurgeSession`;API 层非 live 会话直调 store。
- [x] 4.6 `GET /v1/projects` → `{ "projects": [ProjectMeta] }`,只含有未删除会话
  的 workdir。
  → `handleListProjects`。
- [x] 4.7 在 server.go 注册新路由(`GET ?workdir` 复用既有 `GET /v1/sessions`;
  新增 `GET …/history`、`PATCH /v1/sessions/{id}`、`GET /v1/projects`)。
  → server.go routes() 已注册全部新路由。

## 5. title 派生(§8 Q1)

- [x] 5.1 落第一条 user message 时从首条用户文本派生 `title`(长度截断/单行化),
  写入 `sessions.title`;PATCH 可覆盖;空串语义保留。
  → `deriveTitle`(loop.go) + `Session.PersistTitle`(session.go)。

## 6. T5 补帧(可选,加分)

- [x] 6.1 复核 `GET …/stream` 的 `Last-Event-ID`/`last_event_id` + `EventsAfter`
  能让晚加入 running 会话的订阅者补到本轮已发事件;在 spec 固化该语义,补测试。
  → 已存在并通过:`TestStreamGet_LastEventID_HeaderReplay` / `_QueryParamReplay`
  (发 5 事件、drain、从 idx 2 后重放得 3/4/5)。T5 补帧机制本就到位,无需新增实现。

## 7. 端到端验收(对齐任务书 §10)

> 7.1–7.5 在单元/集成层验证(无真实 provider 的端到端 harness);"重启"由"直接落库 +
> 经独立 manager 读回"模拟。真机联调留待与 assistant 对接时跑。
- [x] 7.1 打开项目 P → `GET ?workdir=P` 返回会话列表(含 status)。
  → `TestListByWorkdir_CamelCaseAndStatus` / `_SurvivesNoLiveSession`。
- [x] 7.2 新建会话发几轮 → 重启 sidecar → `GET …/history` 能重建内容。
  → `TestManager_HydratesPersistedSession`(从 store 水合)+ `TestHistory_ToolCallJoinAndReasoning`。
- [x] 7.3 多活:A/B 会话并发流式互不干扰,running 在列表标出。
  → `TestListByWorkdir_MultiActiveStatus` + `TestStreamGet_LastEventID_*`。
- [x] 7.4 切回 idle 旧会话发 user_message → 模型带历史上下文续答。
  → `TestManager_HydratesPersistedSession`(history 还原)+ `TestStreamPost_HydratesPersistedSession`。
- [x] 7.5 PATCH 改名后 list 反映新 title;DELETE 后 list 不含该会话且 transcript
  行已删。→ `TestRenameSession_Persisted` + `TestDeleteSession_HardPurgesTranscript`。
- [x] 7.6 `golangci-lint run` 干净;gofumpt 通过。→ api/session/agent 三个包全部干净。

## 8. 回填(实现后)

- [x] 8.1 若实际路由/字段与任务书不同,按 §9 列差异,知会 assistant 同步
  `src-tauri/src/agent/mod.rs` 顶部常量。→ 实现与任务书契约一致,assistant 侧**无需改动**:
  - 五端点路由/方法**完全按任务书 §3**:`GET /v1/sessions?workdir=`、
    `GET /v1/sessions/{id}/history`、`PATCH /v1/sessions/{id}`、`DELETE /v1/sessions/{id}`、
    `GET /v1/projects`;wrapper 键 `.sessions`/`.messages`/`.projects` 一致。
  - `SessionMeta`/`ProjectMeta`/`HistoryMessage.parts[]` 字段名与 assistant 的
    `AgentSessionMeta`/`AgentProjectMeta`/`MessagePart` **逐字吻合**(camelCase;
    tool_call 用 `id`/`name`;reasoning 带 `status:"done"`)。
  - **新增(超出任务书、向后兼容)**:create/get 响应也已统一为 camelCase,且 list
    的 SessionMeta 额外带可选 `parentId`/`provider`/`model`/`agentType`/`ephemeral`
    ——assistant 容忍多余字段,无需处理。
  - 一处待统一评审项(非契约):history 端点内 `unmarshalContentBlocks` 与
    `session.DecodeContent` 重复,后续可收敛为一处。。
