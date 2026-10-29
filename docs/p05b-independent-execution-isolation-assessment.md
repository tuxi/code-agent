# Managed Collaboration：独立执行环境评估

日期：2026-10-29。范围：源码、本机身份及文件权限的只读检查；未部署、未创建用户/VM、未调用模型。本文是设计评估，不是隔离证明或准入凭据。

## 结论

旧 Job 的恢复与新任务的准入应当分开。旧 Job 继续保持不可安全归属；新任务能否执行，应取决于它是否拥有旧执行者无法写入、也无法借助控制接口间接写入的独立环境。

当前环境尚不满足这一条件。新 SQLite、临时目录、同用户另启 Daemon 均不构成独立执行边界。不能据此跳过原 Store 的 `recovery_required`。

最小候选方案是：**专用非管理员 macOS 登录用户内运行完整的 Talkify + Codex/MCP + Code-Agent Daemon**，使用该用户私有的工作区、状态库、配置、凭据和可信可执行文件。先离线做跨 UID 文件/控制入口负向测试，再决定是否授权真实模型闭环。无需先引入容器平台或修改恢复状态机。

这是有前提的候选方案，尚未通过实际跨用户验证。其威胁边界是普通旧用户进程；旧 Job 的历史记录没有可信权限身份，不能宣称它已被证明仅有普通权限。若必须防御旧执行者曾取得 root、跨用户管理权限或其他特权，则本机新增用户不足以证明隔离，应选择独立可信主机，或由可信主体持有管理面和存储的 VM，并另行验证。

## 1. 当前耦合在哪里

- `internal/session/sqlite/managed_tasks.go` 的 `PrepareRun` 在存在 ReviewPolicy 时调用 `rejectUnprovenWorkspaceProcesses`，发生在创建 Run、工作区 claim、工作流提交之前。
- `internal/session/sqlite/managed_recovery.go` 的 `ReconcileRun` 也调用同一检查。恢复与新准入复用的是同一套副作用风险判断，而不是要求先恢复某个旧 Run。
- 检查先处理与目标路径重叠的 Session，再扫描不能直接归属 Session 的后台子分区。`managed_process_ownership.go` 对唯一且工作区可信、与目标不重叠的父 Session 已可缩小范围；工作区未知的父 Session 返回 `unknown_owner_workspace`。
- 因此，本次属于“新准入被保守检查拒绝”，不代表新任务已有一个待恢复的执行 Turn。并非 Store 内所有历史 Session 都无条件阻塞；未知归属的后台执行记录才使风险范围无法缩小。

当前模型记录了 Server/Store、规范路径、工作区锚点、执行租约与 Run/Turn；没有 OS 强制隔离域的证明。路径不重叠不等于两个非沙箱进程无法访问对方目录。现有 claim/lease 约束遵守协议的 Runtime 请求，不能阻止脱离管理的历史 OS 子进程。

正确的分离方式不是给旧 Job 赋予“已安全”结论，而是在另一个经过验证的执行域中建立新 Task。原 Store 中的阻塞及证据保持原状。未来若要在同一 Store 中管理多个隔离域，需要可信的执行域绑定与验证入口；当前不增加该功能。

## 2. 本机实际检查

只输出身份、权限与签名标志，未输出令牌、环境变量或其他会话内容。

| 项目 | 只读检查结果 | 含义 |
| --- | --- | --- |
| 系统 | macOS 27.0，26A428 | 本次机器快照 |
| Talkify / Daemon | PID 48928 / 48973，均 UID 502、GID 20，父子关系成立 | 没有用户身份隔离；PID 仅用于本次观察 |
| 运行包 | 当前用户 DerivedData 下 Debug-MacDirect/Talkify.app，包及 codeagentd 均 UID 502、0755 | 不能作为独立环境的共享可信可执行文件来源 |
| 已签名 entitlement | 没有 `com.apple.security.app-sandbox`；`com.apple.security.get-task-allow=true` | 当前调试包不能宣称具有 App Sandbox 边界；后者也不代表已验证跨 UID 注入可行 |
| 原隔离工作区父目录 | `/private/tmp/codeagent-gui-workspace-__aaks0i`，UID 502、0700；repo 为 0755 | 可隔离普通其他用户，不能隔离旧的同 UID 非沙箱执行者 |
| Session/Flux | 当前项目 sessions.db、managed-collaboration/flux.db 均 UID 502、0644；项目目录 0755 | 状态持久化不是 OS 执行边界 |
| MCP grant | CodeAgentMCP 目录 0700，connection.json 0600，均 UID 502 | 能保护其他普通用户；不能隔离持有同 UID 权限的非沙箱执行者 |
| 以上目标 ACL | 所检查目录/文件未显示扩展 ACL 条目 | 仅覆盖已列目标；未递归审核整个 HOME 或工具链 |
| `/private/tmp`、`/Users/Shared` | root 所有、1777 | 不应存放跨域共享的敏感数据或控制凭据 |
| `/opt/homebrew/bin` | UID 502、GID 80、0775 | 新用户若执行该目录中的旧用户可替换程序，会重新引入旧域代码 |
| `/Applications/Xcode.app` | root 所有、0755 | 只是顶层元数据，尚未完整验证内部工具或供应链 |

非沙箱只读检查确认当前 UID 可读写 Session DB、Flux DB 和 grant；没有实际写入。此前在 Codex 工具沙箱内观察到的 `os.access(..., W_OK)=false` 不能当作 Talkify/旧 Job 的权限证据，已用非沙箱只读检查区分。历史 Job 的 UID/特权仍未知，不用当前 PID 身份补写历史事实。

### 控制面与启动链

`chater/Talkify/CodeAgentDaemon.swift` 通过 `Process` 启动内置 Daemon，没有切换用户；继承当前用户 HOME，加载该用户 shell 配置的环境，并启用 Claude MCP 配置继承。它生成新的 owner token，经环境交给 Daemon。

`LocalMCPConnectionGrant.swift` 将同一 token 写入当前用户私有 manifest。`internal/mcpbridge/connection.go` 校验文件、PID、Runtime/Store、过期和撤销；这些是现有授权的必要保护，并不是不同同 UID 程序之间的隔离。Server owner Bearer 不绑定 HTTP 调用者的 OS UID。loopback 也不是每个 macOS 用户独享的网络命名空间。

因此“新 UID 的 Daemon + 旧 UID 的 GUI/MCP 读取 owner grant”仍不充分：旧域可通过读取 grant 后调用 API，借新 Daemon 的权限写文件。后续保留同一用户 GUI 的产品方案需要独立设计受认证的控制代理/IPC 和凭据保护，不能仅换执行 UID。

## 3. 方案比较与最小成本

以下是工程估算，不包含模型任务耗时、账户登录问题、审批等待或发现新缺陷后的修复。

| 方案 | 是否满足本次目标 | 工作量与缺口 |
| --- | --- | --- |
| 新 tmp / 新 DB / 同 UID 新 Daemon | 否 | 只有数据分离，旧执行者仍可能写目录、替换程序、读取新凭据 |
| 仅给新执行进程加沙箱 | 单独不足 | 限制新进程能访问什么，不能据此证明旧非沙箱进程不能写新目录；需另补入站写入及控制面边界 |
| 专用登录用户，完整 GUI/MCP/Daemon 栈 | 推荐的最小验证候选 | 约 0.5–1.5 个工程日进行配置审核和离线权限/GUI验证；通常不需新增 Runtime 抽象。需要用户创建并登录独立账户、独立配置应用与凭据；不承诺当前对话透明迁移 |
| 新 UID 仅托管 Daemon，现有桌面继续控制 | 不是当前零改动路径 | 需特权生命周期装配、带调用者身份的 IPC/代理、owner 凭据不回流旧域、工具链与租约边界；设计和原型约 3–5 个工程日，硬化另计 |
| VM 内完整桌面栈，无可写共享目录 | 更重的备选 | 约 1–3 个工程日准备镜像、工具链、登录和离线验证；还要验证 host 上 VM 磁盘、配置、控制 socket 的所有权，旧用户可改这些时不能宣称强隔离 |
| 独立可信机器 | 本机特权无法界定时的备选 | 增加机器与账户配置、连接和验收成本；无需为本次先建设容器平台 |

Apple 文档把 App Sandbox 定义为限制应用访问资源；其本身不是对所有旧进程的反向写保护。上述不足是结合当前同 UID 权限作出的推论。[Apple App Sandbox 文件访问说明](https://developer.apple.com/documentation/security/accessing-files-from-the-macos-app-sandbox)

macOS 文件权限可以按用户与组控制读写；专用账户方案仍需验证实际权限和继承 ACL，而不能只依赖账户名字。[Apple 文件权限说明](https://support.apple.com/en-gb/guide/mac-help/-mchlp1203/mac)

Apple Virtualization 可将宿主目录暴露给 guest；若采用 VM，必须逐项验证共享目录与宿主管理权限，不能将“运行在 VM”视为已切断共享写入。[Apple Shared directories](https://developer.apple.com/documentation/virtualization/shared-directories)

## 4. 专用账户方案需要验证的边界

1. **执行身份**：新建非管理员登录用户；Talkify、Codex/MCP、Daemon 及受控测试子进程均核对实际 UID/GID。不要只改 HOME 或通过旧用户启动一个自称独立的进程。不使用 sudo 工具，不继承旧域已有 FD、socket 或凭据。
2. **私有状态**：在新 HOME 内建立独立 workspace、Session SQLite/WAL/SHM、Flux、ReviewBundle、缓存、临时目录、日志、模型配置与凭据；保护目录及祖先路径，检查 ACL、组、符号链接和硬链接。拒绝指向旧 HOME、Shared、旧 tmp 工作区和旧 Store 的链接。
3. **可信程序和配置**：将审核过的构建副本部署到旧 UID 不可写的新用户私有或受管理员保护目录；不直接执行旧 DerivedData 或旧 Homebrew 程序。审核真实解释器、动态库、插件、PATH、shell profile、Git hooks、工作区规则和自动继承 MCP。只使用新账户必要配置，不整体复制旧 HOME/secrets/cloud Runtime 连接。
4. **控制凭据**：新 Runtime/Store/Session/授权独立建立；新 grant 仅由新用户中的 Bridge 读取，不向本对话/旧账户导出。不得复用原 owner token 或把新 token 放入旧账户可读配置、命令行、日志、共享剪贴板。独立核对 TLS 和服务器身份；不提供未认证写入口。
5. **共享资源**：不开放旧域可写的源码挂载、工具缓存、模型代理、自动同步目录或执行型 MCP 服务；必要系统只读组件逐项登记。确认远程管理、屏幕控制和文件共享不会给旧执行域授予新的控制权。
6. **身份不混用**：新环境的 Accepted 是新 Task 的验收，不能写回为旧 Task 已恢复。原 Store、Session、MCP 配置和 `recovery_required` 证据全部保留；新环境也不导入无法归属的旧执行记录。

Talkify 现有单实例检查、按二进制路径清理旧 Daemon、应用配置恢复行为，需要在两个登录会话间实测。当前仅审核了其源码，不保证跨登录用户的实际行为。若发生跨用户实例误判，先停止，另行提交最小宿主修复；不关闭保护来凑通。

## 5. 下一步离线验收，尚未执行

在授权创建环境后，用无密钥的专用测试文件/测试服务验证，保留命令、执行 UID、退出码和时间，不接触原项目数据：

| 检查 | 必须得到的证据 |
| --- | --- |
| 新用户正向访问 | 能创建/修改新 workspace、读写新状态库及授权 manifest；确认真实 UID 和目标 inode |
| 旧用户文件负向访问 | 在已知完整路径时，仍无法读取新私有测试凭据、写/替换/rename/unlink 测试候选文件；覆盖目录祖先与 symlink 访问，不能只检查 `stat` |
| 新程序不可被旧域替换 | 旧 UID 无法写 bundle、Daemon、实际解释器及参与加载的配置/库/插件路径；测试用哨兵副本验证，不修改真实程序 |
| 控制面负向访问 | 从旧用户对新本机测试端点的无凭据/旧凭据请求拒绝；确认新 grant 无法读取；不得为做此测试披露新 token |
| 共享面审核 | 文件共享、缓存、MCP、同步和 VM 挂载（如采用）有明确清单，无跨域可写执行依赖 |
| 双 GUI 与持久化 | 新用户内脚本模型驱动相同 Daemon，完成 Execute/Review/Rework/PASS/Finalize；Talkify Accepted，重连及历史读取一致 |
| 原环境不变 | 原 Store 身份、旧证据和准入拒绝继续保留；不清库、不补猜测路径、不删除租约、不忽略旧 Job |

脚本模型应使用已有离线测试装配；若桌面包尚无脚本模型接入，单独评估测试入口，不能默认为生产 Provider 已可切换成脚本模型。先验证上述 OS 边界和无需模型的 Task API，再实施必要最小测试装配。最终真实模型验收仍需独立确认 Provider、数据范围和预算。

## 6. 本批结果与停止线

已完成：源码调用链检查、本机 UID/权限/ACL/签名元数据检查、方案与成本比较。未重复调查旧 Job，未更改历史记录、数据库、程序、授权或恢复逻辑；无新增模型调用。

**当前 isolation_verified=false。** 未创建独立用户，也未执行跨 UID 权限负向测试，因此不能宣称现有环境已安全隔离或可准入。本次只新增本文，无生产 Diff，不需要重新运行代码回归。

建议下一批仅授权“专用账户完整栈的离线隔离验证”，通过后才部署受管模型实验。若不接受独立登录桌面，或者无法把旧执行者权限限制在既定威胁模型内，应明确转向更强边界评估；不再循环重试原 StartRun，也不继续扩展旧历史恢复状态机。
