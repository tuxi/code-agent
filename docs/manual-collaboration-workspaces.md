# Manual 多工作区选择

2026-11-05。用户已批准并应用本项目 MCP 配置切换，重载后的原生工具只读验收通过。与上一批历史读取共同使用 Manual `1.2.0-experimental`，保持 13 项工具；没有新增调度器或 MCP 工具，没有修改 Daemon、Agent Loop、模型参数或 Talkify。

## 用户体验与权限

启用后，同一个已加载的 MCP 连接可以选择多个项目，各自创建或绑定 Session，不需要等待其他独立项目完成，也不需要为切换项目重启 Codex。MCP 在不同 Codex 项目中是否可见，仍取决于该客户端的配置发现范围；本实现不会自动安装全局配置或修改其他项目。

这是可信本机调用方在现有 Runtime owner 授权内显式选路，不是新的服务端项目授权、OS 沙箱或 Managed 准入豁免。启动参数 `--allow-workspace-selection` 是独立的权限选择：允许调用方在运行期间选择现有 Git 根目录。必须由用户明确批准启用，只用于用户指定的项目；不可把该模式提供给不可信第三方客户端。旧配置默认保持单工作区范围。

历史读取参数 `--read-workspace` 仍然只有只读意义。查询历史不会把项目注册为执行根，也不会创建、启动或恢复 Session。多工作区模式下需另行显式选择执行项目。

## 调用流程

1. `codeagent_connection_info({})` 查看连接身份和 `workspace_selection`。
2. `codeagent_connection_info({"workspace_path":"/absolute/project-A"})` 明确选择项目，不创建 Session 或派发 Turn。返回该项目的 `head_commit`、统一 Runtime/Store 身份和已选择项目列表。
3. `codeagent_create_session` 或 `codeagent_bind_session` 携带相同的 `workspace_path`、精确 Runtime/Store/baseline 以及原有必填参数。多工作区模式下省略路径会拒绝，未选择的项目也会拒绝。
4. 保存返回的 binding_id。send/read/wait/handoff/admission/cancel 使用原 binding_id 和精确 Session/Turn 路由，不存在“当前项目”或“最新 Session”的执行回退。
5. 项目 B 重复第 2–4 步，可与独立项目 A 并行。成功发送后结束 Codex 当前回复，仍不默认轮询；用户在 Talkify 观察和审批，完成后回原对话读取一次 Handoff。

`--workspace` 保留为启动时的初始项目，不再是启用模式下唯一可用项目。已有单工作区配置和省略 workspace_path 的原调用保持兼容；多工作区模式要求 create/bind 显式传路径。connection_info 在未提供路径时只检查初始项目，不隐式采用最近选择的项目。

## 一致性和并发边界

每个项目复用原 ManualService、Session Client、基线检查和写入检查；没有另一套执行机制。独立仓库的准入锁和不确定提交记录分别保留，一个项目的执行中状态不阻塞另一个独立项目。同项目多个 Session 仍不能通过新建绑定绕过写入或审批检查。

写入检查覆盖同一 Bridge 连接内已知的 Session，不是跨进程全局租约；它不控制其他客户端或本地进程。不能把它作为执行安全隔离的证明。

所有项目共享同一个已验证 Runtime/Store 和授权代次。打开新项目不能自动采用轮换授权或更换 Store；用户重新授权后沿用已有显式 reconnect，再分别替换旧 Session binding。旧 binding、未知准入和稳定 request_id 不被清空；重连本身不派发任务。创建请求 ID 在本连接内跨项目复用会拒绝，防止丢失回执后换项目重复创建。

目录和 Git 身份固定。符号链接改向、目录替换、Git 公共目录身份改变会拒绝；嵌套执行根及共享 Git 元数据的 linked worktree 暂不作为独立并行执行域接纳。后者需要共享写入域设计，不能用两个路由绕过原写入检查。本版不声称不同仓库拥有 OS 文件访问隔离，也不能据此放开生产 Store 的 Managed recovery_required。

每连接最多 32 个执行根、每根沿用 64 个 binding 上限；不会驱逐未完成绑定。选择注册使用短注册锁，网络检查不会持有 binding 查询使用的注册锁。项目服务生命周期属于 MCP 连接，选择请求结束或取消后不关闭已建立的项目服务。

## 加载与回滚

用户明确批准后，已核对原配置、候选配置和二进制 hash，仅原子替换本项目 command/args，增加 `--allow-workspace-selection`，其他设置及 required=false 保留；未读取授权文件内容。私有备份为 `build/manual-multi-l5m2j3r0/project-config.approved-before.toml`。首次安装新程序/schema 仍需要重载 MCP 一次；之后在该连接中选择项目不需要重载，不需重启 Talkify。首次加载不代表所有 Codex 项目均已安装该 MCP。

回滚恢复私有配置备份并重载原程序；保留所有 Session 和工作区，不删除目录、不恢复撤销授权、不重发未知请求。回滚或 MCP 进程重启后临时 binding 无效，需要按原路径/身份/基线显式绑定恢复。

## 离线验收记录

最终候选 `build/manual-multi-l5m2j3r0/`，基于固定提交及限定源码叠加构建。`candidate.json` 保存源码、二进制和日志 hash；`change.patch` 为源码改动，`project-mcp.toml` 为已批准应用的配置内容。

固定快照 Bridge/CLI race 回归：102 项顶层通过、117 项子测试通过、9 项既有 helper/opt-in 跳过、0 失败；两包均通过。独立跨版本二进制进程测试另跑：1 项顶层及 2 项子测试通过、0 跳过/失败。build、vet 和候选 CLI 配置生成通过。跨版本使用既有 Runtime fixture，新功能不改 Runtime。测试仅用虚构授权、本机端口和临时仓库，模型调用为 0。

覆盖两个独立仓库仍在执行时分别准入、同项目第二写入者拒绝、跨项目 Session/请求 ID 拒绝、授权轮换和显式绑定恢复、只读范围不升级、选择请求结束后存活、已有项目路径失效不阻塞另一已选择项目、路径别名先后登记后改向均拒绝，以及嵌套根/共享 Git 元数据拒绝。新增别名测试曾因故障注入顺序错误失败，已保留原失败候选并调整准备顺序；没有放宽拒绝断言或改生产行为迎合测试。

## 原生工具只读验收

用户重载后，在当前 Codex 对话中直接调用原生工具，无临时 stdio 驱动。connection_info 返回 workspace_selection=true，并核对原 Server/Store 身份一致；先选择 iOSMDM，再选择 code-agent，同一连接的 execution_workspaces 同时保留两者，无需中途重载。

通过 list_sessions(limit=1) 找到 iOSMDM 最近更新会话 `20261105-081355-ee987ed9`（Integrate iMDMPro3 Logic into mdm-audit）。read_history 读取最近 3 条事件，游标为 2659935；切回 code-agent 后，仍按 iOSMDM 的精确 Session/游标读取更早 2 条，游标为 2659933，序号排他且无重复。has_more=true/turn_fragment=true 保留，未将两页描述为完整历史或验收证据；思考正文不输出。

验收未创建或绑定 Session，未发 Turn、取消、审批或模型请求，也未修改业务源码。真实双项目并行执行仍只有离线 fixture 证据，本次只证明当前桌面连接的项目选择、身份和历史分页链路；不等同于全部生产发布验收或其他 Codex 项目已自动安装 MCP。机器可读结果见候选目录 desktop-readonly.json。
