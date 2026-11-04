# V1-c/C3：兼容验证与发布记录

日期：2026-11-04。C3 为离线候选收尾；生产发布仍为 **NO-GO**，等待 V1-d 桌面验收及既有基线失败处置。未部署、未提交、未推送、未调用第三方模型。

## 候选身份与复现

权威清单：`docs/contracts/manual-collaboration-v1c-candidate.json`。候选尚无提交 SHA；以固定基线 `ca92dc9a61cd97541604a54b4fd1c9ed932f1367` 加逐文件 SHA256 标识，包含必要的 V1-a/V1-b/C1/C2 前置源码，排除工作区其他 Runtime、工具、TUI 及 G1/G3 草案改动。不能把基线 SHA 当成候选或已部署版本。

执行 `bash scripts/verify-manual-v1c.sh` 可在临时目录复现。脚本先检查清单内源码/hash，再从本地 Git 创建两个独立克隆，构建基线和候选 Bridge，以及共享测试入口的 Runtime 宿主；依赖下载关闭。所有测试 SQLite、工作区、随机凭据和端口均由测试创建，不读取生产连接授权或 Store。保留临时输出，不自动清理用户目录。

Runtime 宿主仅组装各版本的真实 SQLite、Repository、TurnExecutor 和 NewMux，使用确定性的无工具 Runner，不构造 Provider。它用于验证版本边界、准入和存储，不能证明模型质量、OS 隔离、生产启动参数或 Talkify 打包 Daemon 的行为。补充回归中的既有真实 Loop 测试仅连接本机脚本 Provider。

Go module 依赖与输入源码哈希、构建二进制哈希、AgentKit/Talkify 观测 HEAD 和协议版本分别记录。UI HEAD 只代表只读观测；本批没有构建或验收这些 UI 版本。二进制哈希属于本次构建，路径/工具链变化后需要重新记录，不承诺字节级复现。

## 兼容矩阵

| 组合/故障 | 实测要求 | 证据 |
| --- | --- | --- |
| 新 Manual Bridge → 旧 Runtime | 带 development 返回 validation_contract_unsupported，0 执行；MCP 连接仍可列工具 | C3 跨版本独立进程 |
| 新 Manual Bridge → 旧 Runtime，不带 development | 原协议完成，validation=unknown / not_declared，acceptance=not_reviewed | C3 跨版本独立进程 |
| 旧 Bridge → 新 Runtime | 原工具可调用，相同请求重复回执指向原 Turn，执行计数 1 | C3 真实基线 CLI + 新 Runtime 进程 |
| 旧 Store → 新 Runtime | Store 身份不变，sqlite_master schema hash 和原 payload_hash/accepted_seq/state/接受 payload hash 不变 | C3 关闭旧进程再打开同一临时 DB |
| 新合同 Turn → 新 MCP 进程 | 重新绑定后一次 Handoff 恢复原约定 hash，缺验证仍 partial，无重派 | C3 退出并重启真实 CLI 子进程 |
| 不支持的底层 Store/错误能力配置 | HTTP 与 Hello 降级；Router 不回退到旧输入入口 | C1/C2 负向回归 |
| 损坏/歧义/截断、人工检查及取消/失败 | 拒绝不完整来源；不伪造验证或验收通过 | C1/C2 回归 |
| 实际桌面普通聊天、授权更新、GUI 审批/回放 | 待 V1-d，不能由 CLI 或进程测试代替 | 未验收 |

C3 只新增测试入口、兼容用例、复现脚本和清单/文档，不改变生产 Agent Loop、模型参数、Flux、Task UI、授权或恢复策略。

## 实际验证结果

1. 手动建立固定基线/候选独立克隆，构建真实旧/新 Bridge CLI 和两版本 Runtime 测试宿主；跨版本测试 1 项顶层、2 个子场景全部通过，未构造 Provider、无第三方模型调用。
2. 再从源码清单运行 `bash scripts/verify-manual-v1c.sh`，重新建立另一套克隆并重复构建、跨版本进程测试和相关回归。跨版本 2 个子场景通过；回归 57 项顶层测试，56 通过、1 跳过、0 失败，无 DATA RACE 报告。跳过项为需要专用环境变量启动的 stdio 辅助进程测试；实际 stdio 初始化在父测试中执行通过。
3. 定向 vet 和候选 codeagentd 构建通过；Shell 语法检查和 `git diff --check` 通过。未运行全仓或 Server 完整套件；C2 的 4 项基线失败继续登记，不能据此报告全仓全绿。
4. 初次独立进程日志：`/private/tmp/codeagent-v1c-c3-cross-version.jsonl`。复现脚本日志：`/private/tmp/codeagent-v1c-c3-repro.log`，其中记录本次输出目录。输出中的 `cross-version.jsonl`、`regression.jsonl`、`input-manifest.json` 为详细证据。临时路径可失效，因此候选源码清单、结果和二进制 SHA256 保存在仓库 manifest 中；脚本可重建测试数据。

本次源码与固定基线兼容；没有把候选安装到 Talkify，也没有检查正在运行的生产 Daemon 二进制。AgentKit/Talkify HEAD 已只读记录，未作本批构建或 GUI 验收。

## 发布前操作顺序（本批未执行）

1. 固定三个仓库的候选提交、协议版本、构建参数和实际待安装二进制哈希；候选提交前需核对清单，不能把不相关 WIP 混入。当前清单的 commit=null 是明确的未提交状态。
2. 先核对运行中 Turn、后台工作、待审批和 recovery_required。只有用户确认合适时机才停止 Talkify/Daemon；不覆盖正在运行的程序，不清理旧历史。
3. 在确认写入停止后备份 Session SQLite、私有 Flux SQLite 和身份/配置，保留一致的恢复点；若仍存在写入或无法证明备份一致性，停止升级并说明，不用单独复制一个仍活动的 .db 文件冒充一致备份。凭据备份保持原私有权限，不能写入报告或仓库。
4. 先升级 Daemon，再升级 Bridge；核对原 Server/Store 身份、旧 Session 和 runtime capabilities。项目 MCP 保持客户端原生可选配置（Codex required=false）；不自动扩大为全局注册，不续授权或迁移工作区。
5. V1-d 先做不调用模型的桌面验收：下游停止/失效时普通对话仍可发送；恢复后原生工具可列出；原 Session/Handoff 只读恢复、来源和状态准确；错误不会关闭 MCP。任何需要重启、恢复授权或新建测试 Session 的动作按当次实际影响安排。
6. 六项 V1 要求全部验收后才修改发布状态。真实模型任务单独确认 Provider、数据范围与预算；Managed 的 G1/G3 和原 Store recovery_required 继续独立保留。

## 回退决策

- 发现故障先停止新准入，保留原请求身份和已知回执；断连不等于未提交，不换 request_id 重做。
- 无带约定的待执行/执行中输入时，才评估回退候选二进制。先核对原事件及 inbox；不能仅因 schema 没变就认定旧二进制理解新请求身份。
- 存在带约定的 accepted/queued/running 输入、接受事件损坏/缺失或未知副作用时，保持能理解约定的新版本和现有恢复保护，交由人工核对；不运行旧版本恢复这些输入。
- 已完成新约定的历史仍应保留新版本只读能力或先导出证据；旧版本可能忽略 metadata，不能用它宣称持久约定或验证仍然完备。
- 不删 Session/事件/租约，不重写 payload_hash，不清 recovery_required，不恢复过期/撤销授权。需要恢复备份时，必须核对备份点之后的新历史并取得单独操作确认，不能覆盖新历史。

## 剩余验收

四项 Server 基线失败在 C2 已独立复现：Provider 目录数量、两项 pairing bootstrap、pre-admission rejection。C3 不修改其断言或逻辑；尤其 pre-admission rejection 与生产可用性有关，发布前需要修复或明确处置，不能因它是基线失败而默认豁免。

V1-d 桌面验证、独立授权的真实异步任务、Codex 额度效果测量均未完成。C3 的离线兼容结果不等于 Manual Collaboration V1 已上线，也不改变 P0.5 的冻结结论。
