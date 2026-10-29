# P0-B1 最小实现

2026-10-27。已实现持久关联、HTTP 查询/回报和协议测试；没有 Flux 调度、自动返工、ReviewGate、Outbox、执行恢复或 AgentKit UI。P0-A MCP 七个工具、模型选择和 Agent Loop 保持原样。

## 接入与范围

`internal/collab` 定义五类领域 DTO、Snapshot/EventPage 和可选 Repository Port。`conversation.sqliteRepository` 将可选端口转发给 Store；第三方或内存 Store 无需增加方法，不具备端口时 Task 路由不注册，capability 为 false。SQLite 只读连接也不提供端口，因为查询需要更新派生投影。

现有 Daemon 与 Embed 均通过 NewMux 接入，无需另一套 Runner。`GET /v1/runtime/capabilities` 增加 `task_links_v1`、`task_timeline_v1`。HTTP 增量轮询为本批唯一 Task 观察通道；没有新增 Task WebSocket。

B1 Task 接口要求通过现有 Runtime owner Bearer 验证。匿名模式、配对设备或请求体自报的 actor 不获得 Task 权限；没有修改用户认证配置。此处还不是多用户 ACL；Host 直接调用 Repository 时负责注入可信 Principal，LocalSessions 是访问本 Runtime 会话集合的显式授权。所有 Task 数据按 Principal.ID 隔离。报表中的 runtime_kind/native_session_id 是外部声明，不证明原生客户端身份或生命周期。

| API | 输入/结果 |
| --- | --- |
| GET /v1/tasks/capabilities | 当前 server_id/store_id、B1 capabilities、projection=read_through |
| POST /v1/task-agent-sessions | request_id、runtime_kind、可选 native_session_id；返回 Daemon 登记的 external SessionRef |
| POST /v1/tasks | request_id、goal、acceptance；workspace_path 或 workspace_session_id 二选一；返回 Task Receipt |
| GET /v1/tasks?workspace_ref=...&limit=100 | 当前主体的任务摘要，limit 1–200 |
| POST /v1/tasks/{id}/participations | request_id、role、session_ref；角色属于参与关系 |
| POST /v1/tasks/{id}/attempts | request_id、participation_id、turn_id、turn_request_id、iteration；仅关联已准入本地 Turn |
| POST /v1/tasks/{id}/artifacts | request_id、attempt_id、kind=patch/test_report、source_seq、可选 content_hash/truncated |
| POST /v1/tasks/{id}/plans | request_id、participation_id、submission_id、iteration、content |
| POST /v1/tasks/{id}/reviews | 同上，加 verdict 和 reviewed_artifact_id；只返回 recorded |
| GET /v1/tasks/{id} | 一致性 Snapshot |
| GET /v1/tasks/{id}/events?after=0&limit=100 | 持久化增量 EventPage，limit 1–200 |

遵循现有 `{trace_id,code,msg,data}` envelope；成功 mutation 为 HTTP 200，并返回 Receipt（id/task_id/revision/task_seq/status）。输入未知字段、尾随 JSON、超过 128 KiB 的请求和非法游标被拒绝。403 为权限/范围错误，404 为不可见或不存在，409 为幂等/唯一归属冲突。SQL 和请求内容不出现在公共错误中。

Create 可以直接使用已存在、经规范化验证的绝对 workspace_path，无需创建任何 Agent Session；也可使用 workspace_session_id 继承已有会话的 portable workspace ref（iOS/可迁移路径推荐）。二者必须选其一，锚点 Session 不会自动成为 participant。Task workspace_ref 由服务端派生；后续关联本地会话必须同一工作区，server/store 也必须匹配。外部 SessionRef 必须完整引用已登记记录，不能注入本地 SessionID、managed 声明或任意 capabilities。

默认使用流程：登记外部 reported Session → Create Task → Attach planner/executor/reviewer → Report plan → 通过原 Session 接口执行 → 将精确回执 LinkAttempt → LinkArtifact → Report review → Snapshot/Events。Task 接口不会代替原 Session 派发；无需 Task 的单 Agent 模式继续走原路径。

## 数据与事务

同一 SQLite 内新增 8 张表：coding_tasks、coding_task_participations、coding_task_attempts、coding_task_artifacts、coding_task_events、coding_task_requests、coding_task_external_sessions、coding_task_meta。领域文档采用 JSON，关系/所有者/查询与唯一约束使用独立列。TaskEvent 独立表，防止进入 Conversation 查询或 AgentEvent reducer；没有复用伪造的 Session 分区。

- 每次 mutation 在同一事务提交关系、Task 状态、revision、水位、事件和幂等回执。coding_task_requests 是同步 mutation 的回执去重表，不是派发 Outbox。
- 幂等键为 owner + operation/task scope + request_id；输入规范 JSON 的 hash 相同返回原回执，不同返回 409。外部回报还对 task/participation/submission_id 限制重复登记。
- 一个 Session 可属于多个 Task、承担多个角色；一个本地 session/turn 只能绑定一个 Attempt，由数据库唯一约束防止跨任务或并发双重归属。
- LinkAttempt 核对 turn_inputs 的 request_id/turn_id/accepted_seq，以及对应的 durable turn_accepted 事件；不查询 last_turn、不启动新 Turn。SourceRef 保存真实 server/store/session/turn/request。
- Bind、Snapshot、Events、List 在事务内按精确 Turn 补投影，只读取生命周期行，不加载 stdout、Thinking 或工具参数。每个 Attempt 的持久 checkpoint 与 TaskEvent 一起提交。失败回滚后下一次读取可安全重试，不重复里程碑。
- 事务先取得 SQLite 写锁，再读状态，避免两个连接从 WAL 旧 snapshot 升级写入时发生竞态。Snapshot 的子对象、状态、revision 与水位属于同一读取；Events 分页以同一事务内投影后的水位查询。
- Task seq 是独立表的单调行号，跨 Task 有空洞；与 Session seq 不可互换。after 超出 Task 水位或为负数/非整数会返回 400；已读到尾部返回空 events 和原 cursor。

这是查询驱动的派生状态恢复：重开后读取持久关联、源日志和 checkpoint。没有恢复执行、重新派发副作用或后台 projector。源 Session 已被正常删除时，未完成的 Attempt 标记 source_unavailable；不阻止原 Session 删除，也不编造完成结果。

## reported、managed 与验收

本地 Session 关联由 Daemon 赋予 managed；外部登记只能 reported。后者没有 link_attempt、cancel_turn、审批等能力，不能冒充本地 Turn。Planner/Reviewer 的回报形成新的 reported Attempt 和有内容/hash 的产物，但不构成原生执行轨迹。

本地工具产物只接受指向该 Attempt 精确 tool_finished 的 source_seq，capture_mode 为 reported_reference；调用方提供的 hash 只是声明。B1 不读取/冻结工作区，不宣称不可变 patch 或测试已通过。计划/审查正文存于 SQLite 产物，事件只携带产物元数据引用。Review 绑定被审查的 artifact ID/版本/hash 来源；verdict=pass 仍为 recorded，TaskAcceptance 保持 not_reviewed，不触发任何返工或完成。

对旧补丁回报 review 不会覆盖正在排队/运行的当前执行状态。Task 的 phase/state 是阶段观察，失败 Attempt 不自动等于 Task failed，completed Turn 也不等于 Task completed。待审批列表、聚合费用和 usage 在本批无法可靠获取时返回 null；不伪造为零或空队列。没有 Task 控制 API，不提供虚构的暂停/取消动作。

## 迁移和回滚

1. 部署前停止使用此 DB 的 Runtime，使用 SQLite backup/备份工具保存一致性备份；不能仅复制一个仍有未合并 WAL 的 `.db` 文件。
2. 新二进制首次以可写模式打开现有 DB 时，在单一事务中 CREATE IF NOT EXISTS 新表、索引及 schema_version=1，生成一次持久 cstore ID。没有 ALTER/重写旧 Session、Message、TurnInput 或 SessionEvent 数据。重复打开保持 StoreID 与数据不变；未知 Task schema version 拒绝启动，不擅自升级。
3. 常规回滚只需停服务并换回旧二进制，保留新增表。旧 Session 代码忽略这些表；以后回到 B1 时关联数据和 StoreID 保留。不要在新进程仍运行时清表。
4. 若明确要删除 B1 数据，先备份、部署旧二进制，再经操作者确认执行 [可选清理 SQL](migrations/p0b1-down.sql)。它只删除新表，不触碰 Session 数据；所有 Task 历史和幂等回执将丢失，再升级会产生新 StoreID。本轮仅在临时测试库执行该 SQL。

旧单 Agent 的推理、模型/Thinking 配置、工具权限、审批、派发、保存和事件协议均未改动；不会自动为旧 Session 创建 Task，也没有每 Turn 的 Task 查询或写入。实际增加的是 SQLite 初始化的附加表/索引，以及 HTTP 的独立可选表面。

## 验证证据

- SQLite：Session 跨 Task 复用、同 Session 多角色、reported 不能绑定本地 Turn、跨主体/工作区拒绝、精确 admission、同 Turn 并发唯一归属、晚绑定补投影、断线/重开增量查询、幂等并发、故障注入回滚 checkpoint、并发 Snapshot/Report 一致性、普通 Session 删除、旧库迁移与可选 down SQL。
- HTTP：owner 认证、shared-device 拒绝、无法自报 actor/managed/capabilities、分页与非法 cursor、Task 不进入 Session 列表、不支持 Port 时端点/capability 关闭。
- [同一 JSON fixture](contracts/p0b-task-example.json)：Go 与 [Foundation-only Swift 协议检查](../tests/protocols/TaskContract.swift) 都实际解码；覆盖角色复用、来源引用、null、大整数游标及未知事件。没有修改/编译 AgentKit UI。

Swift 可复现命令：

```sh
swiftc -module-cache-path /tmp/codeagent-task-swift-cache tests/protocols/TaskContract.swift -o /tmp/codeagent-task-contract
/tmp/codeagent-task-contract "$PWD/docs/contracts/p0b-task-example.json"
```

Go 使用离线、去除 API 密钥/live 标志的环境。新增 B1 race、SQLite/Collab/MCP 全包 race、Conversation 排除一项已复现基线后的 race、全项目 build 和相关 vet 通过。Runtime/Server/Agent 完整包仍为原 10 项基线失败；Conversation 的 TestSequencingEmitterCapsToolStreamPersistence 在本批前源码 overlay 同样失败（79,868 字节），单独登记，未修改断言。本批没有新增失败，也未运行全仓库测试或第三方模型。

命令、耗时、完整日志和本批独立 Diff 保存在 `/private/tmp/codeagent-p0b1-checks/`；修改前源码快照在 `/private/tmp/codeagent-p0b1-before/`。Task 事件、产物引用和计划/审查已持久化；本批不提供执行恢复或正式任务验收保证。
