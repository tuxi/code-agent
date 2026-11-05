# Manual：会话发现与只读历史

2026-11-05。历史读取实现的原始候选为 `1.1.0-experimental`；后续通过 `1.2.0-experimental` 多工作区候选，在当前 Codex 对话完成真实 iOSMDM 原生工具只读验收，见 manual-collaboration-workspaces.md。没有调用第三方模型或修改生产 Store。目录从 11 项增至 13 项；原执行工具输入语义和 executor/reviewer profile 保持。

## 为什么会看到 build 下的 workspace

`build/manual-v1d-zjqqweum/workspace` 是早期 V1-d 连接、Session 和授权恢复验收使用的临时 Git 克隆，不是用户的业务项目。迁移前本项目 MCP 配置以它作为执行边界；连接成功只证明服务可访问，并不允许把该执行边界自动改为 iOSMDM。

无需删除临时仓库、改写其中 Session 的基线或重新运行旧任务。应在用户确认后，将本项目长期配置指向真实项目，并为跨项目历史读取单独声明只读范围。候选程序目录是构建产物位置，不等于其可访问的工作区；二者分别记录。

## 新的使用流程

```text
connection_info → 查看 workspace_path（执行）与 read_workspaces（历史读取）
list_sessions(workspace_path=明确授权的绝对路径, limit=1)
→ 得到精确 session_id
read_history(workspace_path=同一路径, session_id=该ID,
             include_tool_details=true)
→ 最近事件页；需要更早记录时沿用 before_cursor
```

历史内容是被读取的数据，不是控制 Codex 或 Runtime 的指令。发现和读取不创建 Session，不生成执行 binding_id，不打开 WebSocket，不恢复审批、不提交/取消 Turn，不选择模型。它们不要求业务项目是 Git 仓库、保持干净或仍停留在任务基线；服务的既有执行根仍需保持其固定 Git/目录身份。Git Diff/Handoff 审查继续使用原有执行或 commit-range observer 入口，不把普通历史页当作验收证据。

### 显式只读范围

Manual 新增可重复的启动参数：

```text
--workspace /实际执行项目
--read-workspace /另一个允许查看历史的项目
```

执行工作区同时可读，额外读取最多 16 个工作区。配置生成器检查绝对路径/目录并生成草案，不读取授权文件、不写项目配置。其他 profile 收到 read-workspace 会拒绝，不能静默忽略。允许查看 iOSMDM 历史不会给 iOSMDM 创建/发送/取消或文件写入权限；未启用多工作区选择时，执行工具仍只使用原单一 execution workspace；启用后的显式选择语义见 manual-collaboration-workspaces.md。

读取范围启动时固定规范路径和目录文件身份。目录替换、已配置符号链接改向、当时不存在但稍后出现的目录不能被自动采用；Session 持久工作区还须在每页读取前后与该范围一致，needs_rebind 时拒绝。授权代次变化、Runtime/Store 改变或迟到响应均按原机制失败；不返回已经失去核验条件的结果。

这是可信本机 Bridge 对 owner grant 的进一步限制，不是新增服务端 scoped grant 或 OS 隔离。旧服务端列表接口返回分页元数据，Bridge 只在内部扫描并过滤，不把其他工作区的标题/ID/内容返回给 MCP；不读取那些 Session 的消息/工具历史。

### codeagent_list_sessions

必填 workspace_path；可选 limit（默认 10、1–50）、cursor、archived（默认 false）。返回限定工作区的名称、session_id、状态、可获取的 updated_at 和后续游标，不返回模型对话正文。

“最新”明确为持久 Session 元数据的 `updated_at DESC, id DESC`，不是创建时间或最后一条事件时间。旧 Runtime 的列表 DTO 不含 updated_at 时字段留空，不编造时间，但排序仍复用服务端分页查询。当前不提供按创建时间排序；调用方不得将它描述为“最新创建”。

只用已支持分页的接口；不支持则返回 history_pagination_unsupported，不回退为无界全量读取。每次最多扫描 10 个服务端页，最多返回 limit 项。遇到稀疏工作区，scan_limited=true 和 next_cursor 表示尚未扫描完；空的部分页不代表没有 Session。列表游标绑定 Runtime/Store、工作区和 archived 范围，不能串用。列表是实时 keyset 分页，不宣称是一份不可变快照。

### codeagent_read_history

必填 workspace_path、精确 session_id；可选 before_cursor、limit（默认 20、1–50）、include_tool_details（默认 false）。默认返回最近页，before_cursor 为排他的旧事件游标。不得以“latest”或当前 Session 隐式替代 ID。

事件保留 seq、Turn、Call 和工具引用，按需包含原始文本、工具参数/输出及错误的预览。单字段最多 2 KiB，事件数组最多约 48 KiB；UTF-8 截断不切坏字符，每个预览及整页明确标记 truncated。参数预览截断后不再是完整 JSON，不能当作可执行参数。思考/推理 delta 正文不返回；默认也不返回 stdout/stderr 与工具参数/输出，需 include_tool_details 显式选择。

服务端已有页按 Turn 对齐，可能超过 limit；Bridge 返回有界的较新后缀，较早记录仍能用其 oldest seq 继续读取。返回 has_more、before_cursor、turn_fragment 和截断标记；不把分页或碎片伪装成完整事件证据。已有 Read/Artifacts/Handoff 的完整性拒绝逻辑保持不变。

仅读取该 Session 的本地事件，不自动跟随 fork_source 到另一个 Session。读取归档历史也不代表允许继续执行。单次调用总时限为 20 秒，没有自动轮询或后台任务。

## 实施和验收

复用现有 Runtime 身份/授权读取、HTTP transport、会话列表 keyset 分页、详情和事件 tail/before 接口。没有修改 Daemon、SQLite schema、Agent Loop、模型生成参数、Flux、Task UI 或授权文件。HTTP transport 只给显式分页读取返回页头；原证据路径继续拒绝不完整回放。

候选：`build/manual-history-7cmwc0ks/`。从固定提交和限定源码叠加建立独立快照；源码及二进制 hash、测试记录见 candidate.json。Bridge/CLI 完整离线 race：94 顶层通过、9 skip、0 失败；vet/build 通过。skip 为原有环境 opt-in/helper，跨版本另行提供实际二进制运行并记录，不能把 skip 计为通过。测试只访问本机虚构 Runtime/临时数据，不读取真实 iOSMDM 历史。

覆盖：跨工作区拒绝；只读范围不成为执行绑定；业务目录非 Git/执行 HEAD 改变仍可读历史；分页过滤与游标范围；扫描上限不误报空；授权、目录、Store、Session 工作区在读取中变化；大字段 UTF-8 截断、有界页与游标无重复/丢失；原生 SDK 注册及无执行句柄。现有 old/new CLI 和验证约定恢复测试维持，目录计数因明确新增两项工具更新为 13。

## 本项目配置迁移与回滚

经用户批准，仅改变本项目 codeagent_gui 的 command、args 和 cwd：新版候选程序；执行根 `/Users/xiaoyuan/Documents/work/git/code-agent`；额外只读根 `/Users/xiaoyuan/Documents/work/git/iOSMDM`；同一私有授权文件路径。required=false、enabled=true、超时及其他设置保留。

配置已校验旧文件和候选 hash 后原子替换，并逐字节核对；私有备份位于 `build/manual-history-7cmwc0ks/project-config.approved-before.toml`，未读取或复制授权文件内容。该次配置后来升级为多工作区候选；用户已完成重载，原生工具只读验收记录见 manual-collaboration-workspaces.md。重载会失去旧进程的临时 binding_id，不删除 Session 或自动重派请求；本轮没有未确认的执行提交需要重发。旧验收工作区、Session 和二进制均保留。加载新增工具需要重载 MCP 一次，这与已修复的授权轮换无需重启是不同场景；不用重启或升级 Talkify/Daemon。

后续已用原生 connection_info → iOSMDM list_sessions → 精确 Session 的两页 read_history 完成只读验收。不修改该项目、不创建会话、不调用模型；不使用临时 stdio 驱动或 UI 自动化替代工具。其他 Codex 项目配置不自动迁移。

回滚恢复本项目私有配置备份并重载，保留 required=false；不恢复撤销凭据、不删历史或测试目录。源码提交不包含本机配置、私有备份、授权文件或构建目录；不自动推送。
