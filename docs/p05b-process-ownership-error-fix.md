# 子流归属与结构化错误修复

2026-10-29。本批修复真实受管桌面实验暴露的两处问题；只做离线回归及现有 Store 的只读预检，没有启动模型或受管 Run，没有更新运行中的 Talkify Bundle。

## 改动

`managed_recovery.go` 继续先检查与目标工作区重叠的所有 Session：后台、client tool、subagent 或 workflow 历史仍按原保守策略拒绝。随后使用新增的 `managed_process_ownership.go` 检查没有同 ID sessions 行的 bracket 分区。

合法 Job 子流不会因缺少同 ID Conversation 而直接成为孤儿。检查要求事件 Kind、child SessionID、TurnID、CallID 和持久行一致，并有唯一现存父 Session 的匹配 bracket。只有父工作区为可解析的绝对路径且与目标无重叠时，才允许忽略该子流。没有用 job_finished 或 PID/租约过期作为进程退出证明。

以下情形保持 `recovery_required`：真正失去父会话、缺少/损坏关联字段、多个父会话、工作区未知、目标路径重叠以及超过有界扫描限制。历史未归属记录不会被删除或猜测绑定。没有新增恢复/force-resume 协议。

扫描显式使用已有 session/kind/id 索引先过滤 bracket，避免对大型 EventStore 的无关 token/output payload 做全表读取；不新增索引或迁移。

Server 的 `writeTaskError` 在普通 ErrConflict 之前保留 `recovery_required` 与 `managed_workspace`。MCP 非 2xx 响应转换为有类型的 RuntimeAPIError，SDK 通过 isError 文本块输出可解析 JSON：

```json
{"error":{"code":"recovery_required","message":"Execution requires recovery reconciliation; do not resubmit or replay side effects.","http_status":409}}
```

只接受与 HTTP 状态匹配的 Task 错误代码白名单，消息使用 Bridge 本地固定文本；错误正文最多读取 16 KiB，未知代码/HTML/损坏 JSON/过大正文均变成通用错误。不会反射私密原始 msg、debug 或正文。可选 trace_id 只接受规范 UUID。`errors.Is` 保留领域边界语义；没有新增自动重试。

## 实际验证

- `go test -race ./internal/collab ./internal/collabflux ./internal/session/sqlite ./internal/mcpbridge -count=1` 通过，包含原有 SIGKILL/恢复回归。
- `go test -race ./internal/runtime ./internal/server -run 'TestManaged|TestJobEventSink|TestTask' -count=1` 通过。
- 最终固定拒绝原因分类后，SQLite `TestManaged|TestPrepareRunWithUnrelated` race 回归再次通过。
- `go build ./...`、受影响包 `go vet` 通过；最终 `build/codeagentd` 与 `build/codeagent-mcp` 已构建。
- 实际 `JobEventSink` + SQLite 产生双分区 bracket，再执行 PrepareRun、同请求幂等和 ReconcileRun 的离线集成测试通过，不仅是手写 JSON fixture。
- 15 种归属场景覆盖不同工作区的 running/finished Job 和 Task、本工作区/嵌套路径/符号链接、真正孤儿即使有 finish、父 Session 删除、Call/Turn/child/Kind 不匹配、多个父会话、缺工作区及缺 CallID。
- Server 验证边界错误码与私密细节隔离；Bridge 验证 envelope/裸响应兼容、白名单、HTTP 状态绑定、无效或过大正文；原生 MCP 内存传输验证 isError 的 JSON 内容。
- PrepareRun 的幂等与 Store 关闭/重开后恢复通过；正常单 Agent 的 Loop、生成参数、Session 协议及 AgentKit UI 未修改。

本机日志：

- `/private/tmp/codeagent-ownership-errors-tests.log`
- `/private/tmp/codeagent-ownership-errors-race.log`
- `/private/tmp/codeagent-ownership-runtime-server-race.log`
- `/private/tmp/codeagent-ownership-final-recovery-race.log`
- `/private/tmp/codeagent-ownership-real-sink.log`
- `/private/tmp/codeagent-ownership-readonly-audit.log`

## 当前真实数据仍有一个安全阻塞

新增显式 opt-in 的 `TestManagedReadOnlyOwnershipAudit`：只有同时给定现有 DB、Store ID、Session ID 和工作区，才执行诊断；使用 NewReadOnly、query_only、只读事务和 10 秒上限，不迁移、不预留执行、不写库、不调用模型。正常测试默认跳过。

对当前 Store 的实际预检约 0.49 秒完成：

```text
read_only=true ownership_check_passed=false recovery_required=true
reason=process ownership unknown_owner_workspace
```

诊断测试通过只表示只读检查成功完成，**不表示受管准入通过**。

进一步有界元数据查询找到一个旧父会话 `20260809-113038-ea2450ff`：它有后台 bracket 历史，但 workspace_path、workspace_root、workspace_rel、workspace_ext_id 全部为空。没有读取其消息或工具正文，也没有把它归到某个猜测工作区。

这条记录不能按“已证明属于其他工作区的正常子流”放行；新代码继续拒绝是预期的安全边界。它是目前发现的剩余阻塞，不声称已排除其后可能还有其他未知记录。不能靠清库、填一个猜测路径或另一套 Daemon 绕过它。

因此本批没有再次提交真实模型 Run，也没有要求用户再重启 Talkify。继续重试之前，需要补齐该旧会话可验证的工作区归属或经过独立设计/验证的人工恢复证据；当前没有 force-resume。

## 发布与回滚

本批无数据库 schema 变化，无生产数据写入，无新权限或默认模型改动；没有提交、推送或部署。新增二进制尚未进入运行中的 Talkify Bundle。

待剩余数据阻塞处理后，再通过既有 Chater 构建脚本和 App 构建阶段装配新版 daemon，选择无需保留执行任务的时机重启；恢复同范围 MCP 授权，先检查原 Session/Store，再使用固定模型和有限预算重试。不要直接覆盖仍在执行的 daemon 或通过主工作区代替隔离仓库。

回滚仅涉及本批 Go 源码/二进制；数据库没有迁移或清理需要撤销。旧版本会重新把正常子流视为全 Store 孤立记录，可能再次拒绝受管准入；不得为了回滚而丢弃 Task/Session 历史或解除未知执行保护。
