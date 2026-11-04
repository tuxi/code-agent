# V1-d：桌面验收进行中

日期：2026-11-04。状态：项目 MCP 配置已切换，客户端重启后的原生工具加载、过期授权场景及用户续授权后的身份核验已通过；实际 Daemon capability 核验待完成。Daemon 尚未更新，仍为 NO-GO。没有新模型授权或调用，不发送 Turn。

## 本轮已验证

- Talkify 正在运行，其打包 Daemon 进程也在运行；只读取程序名/PID及二进制 build info，没有读取进程环境。实际 Daemon 的源码基线为 `ca92dc9a61cd97541604a54b4fd1c9ed932f1367` 且 modified=true，不能据此认定包含 C1/C2；必须由当前 capability 和二进制身份核验。
- 切换前项目配置指向旧固定 Session Bridge，二进制基线为 `9192a0b1619512d39a1a2491c2da771ee7da231c`，没有显式 profile 或 required=false；现已切换并加载下述 Manual 候选。
- 预检时本地 MCP 授权已于 `2026-10-31T19:31:45Z` 过期，记录的 PID 与正在运行的 Daemon 不同；先验证了过期拒绝，之后由用户恢复授权。报告不保存 Token。
- 实际 GUI 只读检查：当前服务器为“本地 CodeAgent 服务”，“协作任务”可打开并显示原三项桌面测试任务。未启动任何任务；详情点击时检测到用户同时操作，因此未继续改变窗口。这个结果只证明现有入口与列表可读，不是新 V1-d 的完成验收。

## 准入基线失败的调查与修正

`failingRequestCommands.SendMessageWithRequestID` 测试替身少了 effort 字符串参数，因而没有实现 Router 的 `RequestMessageTarget`；实际被路由到嵌入的旧 SendMessage，返回 nil，导致测试收不到预期拒绝。生产 Router 已有 generic request_failed 回报逻辑。

本轮仅修正测试替身签名并增加编译期接口断言，保留原“准入前拒绝/准入后使用 lifecycle”断言，未改生产 Router。独立候选快照运行 Router/验证约定/资产输入相关 `-race`，25 项顶层测试通过。Manual stdio 初始化、重绑及 Handoff 预检 9 项顶层 `-race` 通过，均为本机离线测试。

另外三项历史失败仍登记，不修改认证或 Provider 迎合测试：Provider 用例硬编码旧目录数量及旧模型首项；两项 pairing 用例对 bootstrap 字符串直接做 hash，而当前生产入口先按 base64url 解码再 hash 原始字节。它们需要专门校正/验证测试数据与当前协议，不可通过放松认证解决。本轮未改这些断言，也未宣称完整 Server 套件通过。

## 可审阅候选

目录：`build/manual-v1d-zjqqweum/`，由 C3 清单的独立源码快照及本轮测试替身修正构建，排除其他未提交 Runtime/工具/TUI 改动。

- `codeagent-mcp`：Manual 候选，已通过项目配置加载；未覆盖原 Bridge 二进制。
- `codeagentd`：arm64 native 候选，采用当前 Talkify 构建脚本的 external-link/native 参数。构建成功，ld 提示 PIE 下忽略 image_base；尚未在实际 App 内启动验收。
- `project-mcp.toml`：候选项目配置，Manual profile、enabled=true、required=false，绑定同目录全新 Git 测试工作区 `workspace`；不指向主源码工作区执行。本轮只核对连接和历史，不发送 Turn。该临时工作区不是 OS 隔离声明。
- `preflight.json`：候选 SHA256、测试计数和当前未部署状态。
- `project-config.before.toml`：原项目配置私有备份，0600；不在报告展开，不进入 Git。

候选目录在 /build 忽略范围内，未写入生产 Store、授权文件或已安装 App。文件哈希保存在 preflight.json。

## 项目 MCP 配置切换

用户已明确授权本次项目范围变更。已替换 `.codex/config.toml` 的同名 `codeagent_gui` 条目：Manual 候选二进制、隔离测试工作区、enabled=true、required=false；移除旧固定 Session/基线启动参数。其他解析后设置保持一致，未全局注册，未续授权或修改 Daemon。

变更前后 SHA256、路径和操作范围记录在候选目录 `config-switch.json`；原始文件另存 `project-config.approved-before.toml`，权限 0600。变更后重新解析 TOML，核对目标表与候选一致，候选二进制哈希仍匹配。

用户已重启 Codex 并回到当前聊天。当前工具清单实际暴露 11 个原生 codeagent 工具，未使用临时 stdio 驱动。连续两次原生 `codeagent_connection_info({})` 均返回 `isError=true`、`error.code=grant_expired`、`retryable=false`，并提示用户授权后核对原 Runtime/Store/workspace；第二次调用仍能返回相同结构化错误，连接没有因过期授权关闭。用户消息和本轮正常工具调用/回复未被 MCP 初始化阻断。未续授权，未发送 Turn，未调用模型。

该结果只覆盖当前过期授权场景，不代表 Runtime 离线、授权替换、有效授权身份核验或候选 Daemon 部署均已通过。

用户随后在 Talkify 恢复授权，并仅提供原连接文件路径。未读取或展示文件内容；原生 `codeagent_connection_info({})` 返回 `identity_verified=true`、`execution_bound=false`。Server 为 `srv_31ff12dc-6485-453d-a565-53da1fe86f03`，Store 为 `cstore_16a5c66010fa0840a975cb9ff13173e8`，与预检身份一致。工作区为候选目录下 `workspace`，HEAD 为 `ca92dc9a61cd97541604a54b4fd1c9ed932f1367`；没有创建或绑定 Session。该响应未暴露 capability 列表，不能据此声称当前 Daemon 已支持 durable_validation_contract_v1。

## 后续桌面步骤

1. 已完成：用户授权项目配置切换并重启 Codex；原生工具加载、连续过期授权工具错误及普通聊天可用已实际验证。
2. 用户恢复授权后的原生身份核验已完成；实际 capability 核验仍待完成。续授权不代表允许任何模型任务或扩大工作区范围。
3. Daemon 替换前单独核对运行任务和一致备份，再确认可停止/重启 Talkify 的时机；不直接覆盖运行中的程序。安装候选后核对能力、旧 Session 与 Handoff。没有完成这些步骤前，不标记“部署完成”。
4. 新模型任务及外发范围另行授权；原 Store recovery_required、G1/G3 和 Managed 安全门槛保持不变。

本轮未提交或安装 Daemon。下一步完成实际 capability 核验和安全部署准备，不发送 Turn。

## Talkify 再次重启后的重连缺陷

用户重启 Talkify 并恢复授权后，当前旧 Bridge 原生查询返回结构化 `grant_replaced`，未阻断聊天。能力核验因此尚未完成。用户同意优先修复“无需每次重启 Codex”的显式重连路径。

修复及离线证据见 [显式重连修复](manual-collaboration-v1-reconnect.md)：扩展原 connection_info，不增加工具、Daemon 或数据库。独立快照 race 84 顶层通过、9 skip、0 失败，vet/build 通过。候选和仅修改 command 的项目配置已准备；尚未应用，未替换运行中程序。需要用户批准配置切换并重载新版 MCP 一次，再实际验证同一 MCP 连接内的授权重连，不能将离线通过记为桌面已完成。
