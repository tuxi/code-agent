# V1-b 实现与验收记录

日期：2026-11-03。候选实现，未部署；不是 Manual Collaboration V1 的生产验收结论。

设计依据：[V1-b 设计](manual-collaboration-v1b-design.md)。本批只调整 Bridge/CLI，不改 Agent Loop、模型参数、Flux、Task UI 或生产 Store。

## 已实现

- 新增 `--profile manual`，MCP 启动只固定本项目工作区/Git 身份；不读取 grant、不连接 Runtime、不创建 Session、不固定任务基线。无授权或 Runtime 离线时仍能 initialize/tools/list，调用时返回安全诊断。
- Manual 原生工具目录固定为 11 项，新增的唯一工具为 `codeagent_bind_session`。每次任务调用必填 binding_id 和精确 Session；不提供 current/latest 目标回退。
- 绑定是进程内不可变随机句柄，固定 Client、Session、原基线、Runtime/Store 和 grant 代次。重复绑定同一元组复用 Client；旧进程句柄、身份错配或旧基线冲突明确拒绝。
- 授权恢复需要显式 replace_binding_id。同 Session 替换在旧读写租约结束后发布新句柄；旧响应保留原 binding_id，旧句柄失效。未知准入记录和验证约定不因替换而被清空。
- 同工作区本服务已知的活跃/未知写入阻止新绑定的写入；未决 Session 创建也会阻止再次创建/发送。目录重建、Git 身份变化、Session 混用及 Store 漂移拒绝。
- 手工发送的未知请求不会自动重写到 WebSocket；先通过 lookup_admission 读取原请求。查到可信 Receipt 后可记录核对结果，不触发执行。
- 创建/绑定请求的取消与已发布 Client 的生命周期分离；单次工具回复结束不会关闭后续任务连接。
- 新生成的无 Session 项目配置显式选择 manual，包含 required=false；原 attach/reattach 配置显式选择 executor。旧 CLI 默认 executor、原 22 个工具和 7 个 Reviewer 工具保持兼容。新增 managed profile 是既有固定绑定服务的显式入口，不增加受管编排。

## 目标与恢复引用

首次先调用 connection_info，取得已核对的 server_id/store_id/head_commit。创建时传 request_id、server_id、store_id、baseline_commit；创建本身不发送 Turn。

成功返回示意：

```json
{
  "binding_id": "opaque-process-local-handle",
  "id": "session_example",
  "session_id": "session_example",
  "workspace_path": "/absolute/project",
  "server_id": "verified-server",
  "store_id": "verified-store",
  "baseline_commit": "original-full-commit"
}
```

其他任务工具带 binding_id/session_id，读取及 Handoff 再带精确 turn_id。发送沿用稳定 request_id 和原 development.validation_commands；不修改模型生成参数。

MCP 进程重启后，使用 codeagent_bind_session 显式传原 Session、原 baseline、原 server_id/store_id，取得新句柄。原句柄不会自动复活；不能改成当前 HEAD 来假装恢复旧执行。

授权代次改变时，在同一进程的 bind_session 中增加 replace_binding_id。不同 Runtime/Store 不能用替换迁移；旧未知请求仍要求核对。提交后 HEAD 改变的新任务使用新 Session/新基线，长期配置不必重写。

## 兼容配置与部署边界

构建候选后，可用 `--print-project-config --workspace /绝对工作区 --connection-manifest /已有私有授权文件` 输出新 manual 配置。此命令不读凭据、不联网、不写配置；只打印片段供合并。显式旧 attach/reattach 参数仍生成兼容 executor 配置。

一个项目配置只允许一个规范化工作区，不自动扩展到同仓库的其他 worktree。授权路径必须使用已有私有 grant，不能将 Token 放在 argv 或聊天里。

manual 与旧 attach/reattach/observe/baseline/delivery、runtime-url/token-env、bundle/export 参数冲突会报错，不忽略旧任务参数。现有生产配置和已安装二进制未替换；TimelineKit 的旧绑定继续保持禁用。

本批没有数据库迁移。回退仅切换候选二进制与备份配置；不删除 Session、事件或工作区。旧程序不认识 manual profile，回退时应停用新条目或恢复兼容 executor 配置，不能恢复 required=true。

## 已覆盖的离线证据

- 标准 MCP initialize/list 在 grant 缺失、过期和 Runtime 离线下保持连接；没有创建 Session/输入帧副作用。
- 实际 CLI stdio 子进程在无 Runtime 授权下成功握手，诊断错误不终止 MCP。
- 两个 Session 的独立绑定、并发重复绑定及并发发送；错误 Session/句柄拒绝，没有命中另一个目标。
- 授权替换后未知发送保留，原请求不重派；在途延迟读取保留旧身份，替换等待租约释放。
- 活跃 Turn、待审批、创建回执丢失、工作区重建、Store 改变均阻止错误写入；待审批不自动处理。
- 原生 MCP create/send/handoff 往返，Bridge 服务重建后显式绑定并拒绝重发旧 request_id。
- 同一工作区提交后，旧绑定拒绝续写，新任务绑定新基线，无需改长期配置。
- 授权在身份探测中途替换时，不读取新的错误 Session；证据回放更新准入映射与另一绑定的写入检查共享锁，race 检查覆盖并发回放/发送。
- 新 Manual profile 连接真实 Daemon、SQLite、TurnExecutor 和固定本机脚本 Provider，执行 go version，并核对 Handoff 的精确 Turn、退出码、validation=complete、acceptance=not_reviewed。

脚本 Provider 不代表真实模型质量。离线驱动为验证状态显式调用 wait 工具，不将其包装成真实 Codex 零轮询验收。真实 Codex/Talkify 的新配置部署、故障矩阵和用户操作体验尚未验收。

最终验证：仅包含 V1-a/V1-b 文件的独立临时快照通过两包 `go test -race`，74 项顶层测试、0 失败；Bridge 77.352 秒、CLI 3.402 秒。快照 `go vet` 和候选 CLI 构建通过，源码 hash 与当前本批文件一致。排除工作区其他未提交的工具/权限修改及 G1/G3 草案。日志：`/private/tmp/codeagent-manual-v1b-isolated-tests.jsonl`；快照范围：`/private/tmp/codeagent-manual-v1b-location.txt` 指向目录内的 scope.json；候选二进制：`/private/tmp/codeagent-manual-v1b-candidate`，未替换安装包。

## 保留限制

1. Registry/创建回执去重仍在内存；不宣称跨进程 exactly-once。已知未知请求 fail closed，跨重启仍须可信准入核对，不能盲目重发。可靠的未决记录与验证约定持久化归 V1-c。
2. 绑定上限为每进程 64 个（含退休句柄），达到时拒绝继续分配，不自动淘汰未知工作。重启前必须核对未决任务。
3. Session 创建丢失回执后，尚无通用的安全清除未决创建入口；本批不猜测“新出现的那个 Session 就是它”。允许只读核对，阻止新写入。
4. 原执行基线变化后，用既有 Reviewer/commit-range/bundle 入口审查历史交付；manual bind 不放宽原执行 HEAD 校验。
5. 只约束本 Bridge 的已知写入；不提供跨进程 workspace 排他、OS 隔离或最小权限 scoped token。原 owner grant 范围和 Managed recovery_required 均未改变。
6. 普通聊天不受协作故障影响还依赖客户端 required=false 设置；真实桌面验证仍是独立门槛。

本批未提交、推送、部署或调用第三方模型。发布资格仍需 V1-c 和 V1-d，不因候选离线通过就宣称生产化完成。
