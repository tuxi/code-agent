# P0.5 最小产品验收路线与离线宿主接入评估

2026-10-29。本批只做只读复核、现有离线回归和路线设计。专用 macOS 用户方案保留，不创建账户、不迁移桌面栈。未部署新宿主、未修改生产权限、未调用第三方模型。

## 结论与顺序

可以先完成 GUI 数据链路验收，将真实模型执行列为独立验收。无需为展示冻结 Diff、审查和 Accepted，先解除旧 Store 的准入阻塞。

建议顺序：

1. **现有 Talkify + 现有 Daemon：生产元数据和已存在历史的只读验收。** 不提交 Run；不在生产库制造 Accepted 或冻结候选。
2. **现有离线进程测试：重新生成完整受管执行数据。** 使用真实业务组件与脚本 Provider，仍保留原准入和 Gate。
3. **独立只读测试端点 → 同一个 Talkify 的外部 Runtime 连接：补齐 ReviewBundle/审查/Gate/Finalize 的桌面展示验收。** 标明离线测试数据，不替换 embedded 端点，不读取生产 owner grant，不接收执行请求。
4. 前三项通过后，另行评估和授权真实模型执行环境及预算。第 3 项通过只能称为 GUI 数据链路通过，不能称为真实模型或生产 Managed Collaboration 全部通过。

## 本轮实际证据

- 原生 MCP `read_task` 读取 `ctask_5e59806b-fb58-4691-81e4-c274a1653950`：waiting/planning、not_reviewed、execution_not_started、revision=5、snapshot_task_seq=12；只有 reported 计划 Attempt，没有 managed Run/执行 Turn。
- `read_task_events(after=12)`：events=[]、next_cursor=12、resync_required=false。未发起 start/send/review 等写调用。
- 本轮桌面工具读取 Talkify 超时，没有获得新的 GUI 状态；不冒称已重新完成截图或界面验收，不要求用户反复重启来弥补此项。
- 离线重新执行：

```sh
GOPROXY=off GOSUMDB=off \
CODEAGENT_TASK_FIXTURE_OUT=/private/tmp/codeagent-p05-product-acceptance-20261029.json \
go test -race ./internal/collabflux \
  -run '^(TestManagedDaemonMCPFullLoopAndBridgeRestart|TestExportManagedTimelineFixture)$' \
  -count=1 -v
```

- 两项通过：8.72s、7.83s；包报告 18.445s，含 race instrumentation，不含编译等待。脚本 Provider 不调用网络模型。
- 新导出：`fixture_only=true`，Accepted、两份 ReviewBundle、request_changes/pass、22 条事件，cursor=22；48,451 bytes。
- 导出 SHA256：`7379945e6f753b09e0798d0b3c36b2be0d003b1a35fe2b593727e2447f0cb587`。这是整个导出文件的校验值，不是各 ReviewBundle 原始 blob hash 的替代品。

## 现有宿主能否直接接 Talkify

**有现成连接通道，但测试宿主不能原样接入。** Talkify 已有“Runtime Servers → 添加服务器”，AgentKit `RuntimeServerConnection.external` 支持 macOS loopback、本连接专属 credential target、Server 身份与现有切换流程。无需改生产 Daemon 的执行身份或权限，也不应覆盖原 embedded connection。

实际缺口：

- `internal/collabflux/daemon_recovery_test.go` 的 helper 是 Go test 子进程，装配生产 HTTP Mux、TurnExecutor、SQLite、Flux 和 MCP。它不是发布版 `codeagentd` 主程序/完整 settings/Provider 装配的桌面测试。
- helper 的 RuntimeInfo 只填 schema/server_id；AgentKit preflight 要求 product/Agent Wire 兼容字段，并读取 health、Runtime capabilities、model catalog。当前 helper 没有提供完整的 UI 连接契约。
- helper 使用源码中固定的测试 token、自动临时目录和测试结束即销毁的生命周期，不适合导出为用户 GUI 服务。不得复用固定测试 token 作为新桌面授权。
- `actualLoopBuilder` 注册通用文件和 shell 工具，脚本 Provider 给出固定调用；它仍不是 OS 沙箱。现有 helper 继承测试环境与工具配置，因此不能将“已有 t.TempDir 和全绿测试”升级成严格文件权限隔离证据。
- 当前共享 JSON 只有最终 Snapshot、完整事件页和两份 bundle。它足够验收终态历史展示，但不足以宣称按时间恢复了每个中间 Snapshot，也不含完整 Session HTTP/WS 历史。

## 最小接入范围：先生成，再只读展示

首批建议只增加**测试专用的只读回放宿主**，不向生产 `codeagentd` 加测试模型开关，不接入生产 Store，不要求改 Agent Loop。它展示来自真实离线执行的输出，并非把静态手写 Accepted 当执行结果。

宿主契约：

- 独立 loopback 端口、随机短期本地测试凭据、明确的“P0.5 Offline Replay”连接名；保留数据来源、原离线 Server/Store/Task/Attempt/Turn 身份。endpoint 变化不重写这些绑定，不伪装成原生产 Store。
- 提供完整但如实标识为测试的 RuntimeInfo、读取能力和无第三方凭据的离线模型目录，以满足现有 preflight。只发布实际提供的读取能力；不发布 task_links_v1、managed start 或可写执行能力。
- GET 白名单：health、Runtime info/capabilities/models、Task capabilities/list/snapshot/events，以及按 artifact id 读取被冻结的 bundle。未知路径拒绝；所有 POST/PUT/PATCH/DELETE、WebSocket 写入、send/start/approval/recovery 接口拒绝。客户端隐藏按钮之外，服务端也必须拒绝。
- UI 仍使用现有 CodingTaskStore 与 CodingTasksView，不注入 Conversation Reducer。当前新建按钮已受 task_links_v1 控制，启动按钮受 managed/review capabilities 控制；回放端点不发布这些能力。必要的“离线回放”标识是测试呈现，不是生产任务状态。
- 复用原离线 API 导出的已验证产物，保留 hash/版本/来源对应关系；不得重编码 Swift/JSON 后声称得到原 blob hash。若增加脱离原服务的产物校验，需保存相应原始字节或使用已有可验证导出，不把校验移除。
- 第一批静态终态即可验证两轮审查、冻结 Diff、Gate/Finalize 和 Accepted；增量/断线测试可分页读取真实事件、断开再恢复同一宿主，核对无重复、无回退。若要展示动态阶段，需在离线运行时记录真实中间 Snapshot，不能按事件猜造权威状态。
- 不承诺首批可以跳转离线 Executor Session：Task fixture 不含完整会话。保留生产原 Session 跳转验收；离线 Session 跳转如需支持，另加只读 Session detail/events/订阅导出，不能跳到生产同名 Session 或伪造工具轨迹。

这条路线可以在当前登录用户中验证 GUI 数据展示，不改变生产执行权限。连接记录属于本地客户端设置变化，应在无人需保留当前页面时切换；不自动关闭现有会话或停 Daemon。退出测试后通过现有连接流程回到原 embedded Runtime，只移除测试连接/其专属凭据，不恢复生产整库。

## 文件写入边界与安全声明

两个问题分开：

1. **离线功能测试的可重建写入范围**：每次有独立 state、workspace、scratch HOME/TMP、输出目录。任务固定为 main.txt，生产 Store、原 repo 和旧 Session 不在目标清单；测试进程退出后终止子进程并保留本次证据。只读回放服务不加载模型/执行器，也不执行来自 GUI 的消息、命令或审批。
2. **阻止其他旧进程干扰的 OS 强隔离**：同 UID 仍不能提供，不能因换库或目录就声称满足。本路线不据此给生产任务放行、不证明旧 Job 安全。

现有回归的写入范围来自受检查的确定性脚本和测试代码，不是内核权限边界。本轮未把它作为 OS 隔离验收。若下一批扩展成可长驻、可交互的执行宿主，应先补最小测试约束：

- 干净环境白名单，不继承 API keys、旧 owner grant、全局 Git 配置、hooks、外部 diff/fsmonitor 或额外 MCP；不从生产配置自动构造 Provider。
- 测试工具只接受确切文件与固定验证操作；文件使用规范路径和无跟随符号链接的打开/写入检查，拒绝绝对越界、`..`、symlink/hardlink 逃逸。禁止通用 shell、后台进程、任意命令和下载。
- 日志、Git 元数据、SQLite/WAL、Flux、artifact、模型缓存/用户配置、临时文件均明确归入测试目录；记录实际写入和子进程清单。
- 以越界文件、链接、未授权 endpoint 访问和非法 GUI 写请求做负向回归。工具白名单属于测试能力约束；若验收要求内核强制文件写入边界，还必须增加经过负向验证的进程级 OS 约束，不能用白名单冒充。未完成前只做受信脚本回归和无执行入口的 GUI 回放。

只读回放对外没有执行能力，可先推进，不以长驻执行宿主硬化作为 GUI 展示的前置条件。也不将生产 owner Bearer 转换成测试读取权限；两边凭据完全分开。

## 已验证与待验证矩阵

| 能力 | 当前证据 | 仍缺什么 |
| --- | --- | --- |
| Task API/MCP 与 SQLite 持久化 | 独立进程测试通过；生产 metadata API/MCP 已验证 | 无需模型可继续验证计划/普通 review 回报与幂等；不能当 ReviewGate PASS |
| 真实 Agent Loop + 工具 + Flux 两轮闭环 | 本轮独立进程 race 复验通过，request_changes/pass/Accepted | Talkify 实际展示这份离线任务数据 |
| MCP 重启、已完成 Task 重启后不重做 | 本轮完整闭环测试包含该检查 | 当前 Talkify 连接该测试宿主后的窗口重建与网络恢复 |
| 10 个 SIGKILL 窗口、未知副作用冻结、候选改变拒绝 Finalize | B2-c 既有独立进程故障测试证据；本轮未重复跑完整矩阵 | 不等于发布版桌面部署故障验收 |
| Go/Swift Task fixture、权限/身份/迟到响应/冻结版本投影 | 既有 Swift 单测和 HTTP URLProtocol 测试 | 真正 GUI 上的 401/403/身份切换及冻结 Diff 视觉验收 |
| 生产 Task 列表、计划、角色、原 Session 跳转/重入 | 既有 Talkify 桌面验收通过；本轮 MCP 状态未变 | 本轮未重新获得桌面工具画面；计划/审查的新回报实时显示可独立验收 |
| 历史第一轮 bundle 与第二轮区分 | 离线真实数据 + Swift 投影通过 | Talkify 点击两轮 Reviewer，核对对应冻结内容、版本及布局 |
| 原生产旧 Job 的恢复保护 | 原判定保留，未重试 Run | 本阶段不解除、不修复归属 |
| 真实第三方模型受管桌面闭环 | 未通过；此前未准入 | 另一个环境、Provider/数据/预算授权和完整验收 |

## 最小验收清单与预计实施量

**A：现有生产 UI，只读与元数据。** 在原 Task 检查计划、角色、execution_not_started、not_reviewed、usage/cost 未知、历史水位；重开页面和 Session 跳转。若测试新计划/普通 review 回报，使用独立标记的 metadata-only Task，不修改旧计划、不启动 Run，不将 reported review 展示成 Accepted。权限负面测试走测试宿主/既有契约，不借此申请更大生产权限。

**B：离线回放 GUI。** 加最小独立读取宿主与端点契约测试，连接 Talkify，检查两轮 Review、两个冻结 bundle、系统 Gate/Finalize、Accepted。测试断开/重连、401/403、重复分页和身份改变；确保清除旧视图，不将旧 PASS 覆盖新状态。手动或桌面截图留证，缺哪个画面就标哪个待验收。

估算：A 为一次约 30–60 分钟的桌面核对；B 约 1–2 个工程日，包括宿主读取契约、随机凭据、拒绝所有写入的回归和桌面核对；未计后续发现的 UI 缺陷修复。优先从最终 fixture 只读展示开始，不先做完整实时脚本执行桌面环境。

若要求“旧执行者不能改新文件/程序，不能读取新凭据或借接口写入”，且不引入其他 OS 强制边界，则不同 UID、私有文件/可信工具链和私有控制凭据是不可替代条件；同 UID 普通目录做不到。**专用账户不是 GUI 数据链路验收的前提**，只在该更强目标下保留为候选。旧进程若可能具有 root，专用账户仍不够，需要更强可信执行环境。本批不立即创建任何环境。

本轮仅新增本文；没有生产实现 Diff。下一实施起点建议 B 的“离线产物 → 只读 HTTP → Talkify Task UI”，不增加恢复状态机，不对旧 Store 增加豁免。
