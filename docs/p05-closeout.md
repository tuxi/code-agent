# P0.5 协作工作区阶段性收尾

2026-10-29。结论：**P0.5-A 核心只读展示已阶段性验收**；P0.5-B 最小创建/关联/启动表单已实现并完成前述元数据验收。真实模型的受管 Execute → Review → Rework → Finalize 桌面闭环仍未通过，原 Store 的恢复保护不变。

## 提交边界

| 仓库 | 本轮提交范围 |
| --- | --- |
| code-agent | `44324f3`：受管 executor model 选择、严格准入、后台子分区归属核对、结构化错误及回归；随后提交只读回放宿主、完整产物与验收材料 |
| AgentKit | `ba3e38d`：独立 Task 数据端口/投影、父任务与冻结产物视图、最小命令表单、可选执行模型、HTTP 契约与 live replay 测试 |
| chater | `568fa93`：Mac/iOS 协作任务入口、本地 MCP 显式授权/撤销、对应三条菜单文案 |

AgentKit 的原有模型选择、Composer、Conversation 事件展示等修改仍留在工作区；Talkify 的签名设置和其他配对文案也保持未提交。本轮没有笼统暂存工作区，没有提交凭据、连接 manifest、构建产物、数据库或用户配置。只读回放的测试连接与服务期限见实施报告。

历史分阶段报告中的“本批未提交”描述保留为当时记录；本文件记录后续收尾提交，不改写此前验收事实。

## 本次提交前复核

- Go：`go test -race ./internal/collab ./internal/collabflux ./internal/session/sqlite ./internal/mcpbridge ./internal/taskreplay` 全部通过。
- Runtime/Server：`go test -race ./internal/runtime ./internal/server -run 'TestManaged|TestTask|TestNamespacedTurn|TestExecutor' -count=1` 通过。
- `go build ./...` 通过；collab/collabflux/sqlite/mcpbridge/taskreplay 与两个相关 CLI 的 `go vet` 通过。
- AgentKit 工作区定向测试：26 项，25 通过、1 项 live replay 因未指定显式连接而跳过，无失败。
- **AgentKit 暂存快照独立验证**：通过 `git checkout-index` 导出暂存文件，排除未提交的模型/Composer 修改，使用现有 Package.resolved 和缓存构建；同样 26 项、1 跳过、0 失败。首次缺少被忽略的锁文件时停止在解析前；补入已有锁文件后通过，没有更新依赖版本。
- **Talkify 暂存快照联合构建**：关联上述 AgentKit 快照，MacDirect 在独立临时 DerivedData 构建成功；禁用包更新、自动解析和应用签名。使用已有 daemon 资源，不下载、执行、部署或覆盖正在运行的应用。未包含签名工程设置改动。
- Go/Swift 的 managed timeline 与 executor-model fixture 均逐字节一致；新增回放保留原始 bundle bytes，hash 不依赖 DTO 重编码。
- `git diff --check` 通过。没有运行整个 Go 测试树，没有声称既有基线失败全部修复；本批没有重新做 iPhone/iPad 真机验收。

日志：`/private/tmp/codeagent-p05-closeout-go.log`、`codeagent-p05-closeout-runtime-server.log`、`codeagent-p05-closeout-swift.log`、`codeagent-p05-index-swift.log`、`codeagent-p05-index-talkify-build.log`。临时日志用于本机复核，长期可重建依据是提交中的源码、fixture 和报告。

## 验收证据与复现

- [只读回放实现、启动命令及返回原 Daemon 步骤](p05-readonly-replay-implementation.md)
- [两轮完整回放导出](contracts/p05-replay-export.json)：真实离线脚本执行产生的 Snapshot、22 条 TaskEvent、两份 bundle 和原始 blob 字节。
- [35 秒服务暂停恢复记录](investigations/p05-replay-outage-20261029.json)
- [P0.5-A 实现与最初桌面验收](p05a-implementation.md)
- [P0.5-B 最小交互与边界](p05b-minimal-interaction.md)

用户已核对 Task 详情、两轮审查与不同的冻结内容、Accepted、页面重开、生产/回放切换无重复或串数据。故障测试验证真实 HTTP 超时、恢复后 Snapshot 不变和零增量；用户观察到侧栏加载后自行恢复。没有把它扩大成 Task 页所有错误视觉表现、物理断网、TCP reset、整 App 重启或真实模型编码质量验收。

## 下一项独立验收

收尾后先评审真实模型 Managed Collaboration 的最小执行方案，明确可信写入边界、Provider、数据范围和预算，再决定是否实施。专用 macOS 用户只是候选，不自动创建；不重试已知被拒绝的原 Run，不清库、不忽略未知历史 Job、不增加 force-resume、不扩大 Runtime 权限。

本轮仅做本地提交，没有推送、部署、模型调用、应用重启或生产数据库修改。
