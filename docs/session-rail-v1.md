# Session rail v1：当前后端契约

状态：分批实现中。完整 `session_rail_v1` capability **尚未启用**。

## 入口与作用域

所有入口复用现有认证及 `{trace_id, code, msg, data}` envelope。
工作区从 conversation 读取，查询参数不能替换工作区。客户端把 ID 作为 URL path segment 编码。

| GET 路径 | 当前行为 |
|---|---|
| `/v1/conversations/{id}/session-rail` | 六组摘要、Runtime/进程/会话/工作区身份、各组状态和 revision |
| `/v1/conversations/{id}/connectors` | 当前工作区有效 MCP 与当前会话已注册的客户端工具 |
| `/v1/conversations/{id}/connectors/{connector_id}` | 同一清单中的单连接及工具目录 |
| `/v1/conversations/{id}/changes` | 工作区相对 HEAD 的完整净差异清单及快照身份 |
| `/v1/conversations/{id}/changes/{snapshot_id}/{file_id}` | 缓存中对应版本的 patch；失效返回 409 `stale_snapshot` |
| `/v1/conversations/{id}/outputs?cursor=...&limit=...` | 跨轮产物目录，根摘要最近 5 项；完整目录分页 |
| `/v1/conversations/{id}/outputs/{output_id}` | 产物的来源记录与不可变预览版本 |
| `/v1/conversations/{id}/context/request?snapshot_id=...` | 实际组装后的请求组成及工具集合，缺省读取最新尝试 |
| `/v1/conversations/{id}/context/sources?kind=memory\|skill\|system&snapshot_id=...` | 请求中的实际来源清单 |
| `/v1/conversations/{id}/context/tools?origin=mcp\|system\|unknown&snapshot_id=...` | 请求实际携带的工具定义及来源 |
| `/v1/conversations/{id}/context/sources/{source_id}/content?snapshot_id=...` | 保存的加载版本或工具 schema，不读取磁盘新版本 |
| `/v1/conversations/{id}/jobs/history?cursor=...&limit=...` | 会话已记录任务及当前 registry 的分页目录，limit 1–100 |

`session-rail` 根摘要中 Changes 最多预览 3 个文件、Background tasks 最多预览 3 项；总数不随预览截断。
Connectors 根摘要省略 tools 内容；长工具目录通过连接详情读取。
不存在/未接通的数据标为 unavailable/error，不把它变成 empty 或精确零。
工作区需要 rebind 时不读取 MCP 配置或 Git；返回明确状态。

## Connectors

复用 WorkspaceRegistry 和原有 manager，读取不调用 `Get()`，不创建 store、加载技能、连接 MCP、ping 或调用工具。
冷工作区可以读到配置名称和来源，但没有连接证据时 status 为 unknown，工具数量为 null。
正在初始化/重载时返回 connecting/loading，避免让读取等在慢服务握手后面。

MCP 配置优先级：local > project > user > 可选 Claude user > host > plugin。
同名服务整项覆盖。初始化与 reload 共用同一路径，插件服务在 reload 后继续保留。
配置来源为安全标识，不返回 command、env、headers、endpoint 或原始连接错误。

MCP row：

- `connector_id`：`mcp:<server name>`，kind=mcp，source 标识最终有效配置层。
- `status`：unknown/connecting/connected/disconnected/failed/disabled。
- `last_checked_at`：SDK 连接状态的观测时间；不是额外健康探测时间。
- `health=not_probed`：未执行额外探活。SDK 连接关闭后更新 disconnected。
- `discovered_tool_count`：握手/发现成功时的工具数；发现失败时为 null。
- `registered_tool_count`：实际注册到此 workspace registry 的工具数，经过 allowlist 和冲突检查。
- `available_tool_count`：已连接时的注册数；断连后为 0。
- `catalog_visible_tool_count`：workspace registry 的 direct 数量。
- `exposed_tool_count`：由成功模型响应对应的 request snapshot 计算，附带 `exposure_snapshot_id`。
  没有成功请求或工具来源不明时为 null；不是 catalog 数，也不证明执行权限。

服务端基于工具实现身份核对注册结果，不依赖名字前缀或“同名即已注册”。
profile 禁止的 stdio 服务显示 disabled；配置错误保留安全诊断，失败服务保留在清单中。
当前清单以有效配置为准，已禁用插件的完整来源诊断仍有后续工作。

客户端 row：kind=client_capability，ID 使用独立 `client:` 命名空间。
只列当前会话实际 register_tools 的定义，`client_capabilities_state=registered_tools_only`。
注册不等于权限已授予或当前可执行；status=unknown，可用/请求暴露数量为 null。
不会硬编码一个“电脑控制”入口，也不会把 code-agent Runtime 伪造成 MCP server。

## Changes

当前支持以仓库/工作树根作为会话目录。嵌套子目录返回 `workspace_not_git_root`，
不把父仓库变化混入当前项目。Git 子进程由所属 Runtime 执行，客户端无需本地 git 或相同绝对路径。

- baseline=HEAD；无首个 commit 时使用空树基准，HEAD kind=unborn。
- staged 与 unstaged 按最终工作区相对基准比较，不逐次累加编辑量。
- 支持 rename 及 previous_path；特殊文件名不使用换行分隔解析。
- 二进制、冲突、子模块和未跟踪项不伪造文本行数：每项 added/removed 为 null，并累计 uncounted_files。
- 顶层 added/removed 仅累计可统计文本项；uncounted_files 非零时不能声称行数覆盖所有变化。
- 非 Git 返回 is_git_repository=false、head.kind=not_git；读取失败与非 Git 分开。

读取前后比较 status、diff、HEAD 和分支，变化时返回 `workspace_snapshot_changed`。
不宣称文件系统全局原子事务；接受的 diff 和文件清单来自同一稳定观测，详情读取缓存内容。
Git 读取 5 秒超时，每次命令输出上限 8 MiB；超限失败，不悄悄截断成完整清单。
最多缓存 8 个会话，根摘要缓存 2 秒，详情最长 5 分钟；快照替换/逐出/过期后要求刷新。
工作区 rebind 后旧路径的快照不能继续使用。所有成功响应带 `Cache-Control: no-store`。

## Outputs

从已记录的工具写入证据与 preview_artifact 建立会话级目录；跨所有轮次，不依赖客户端聊天分页。
新 tool_finished 增量携带 output_evidence_version=1 和 file_writes；仅复用已有可信 Wrote / NestedWrites，
未通过工具名或自然语言猜测写入。新写入复用既有资产协议生成可预览引用，越界路径不会获得新读取权限。

- 同一会话相对路径合并为一行，保留来源 turn/call/sequence；相同 basename 的不同目录独立。
- live_workspace 保存写入报告，不保存历史文件字节；旧报告不能当作不可变文件版本。
- 已知产物的删除更新最后报告状态；单独删除已有输入文件不会把它变成新产物。
- 不可变预览按 artifact ID 和 revision 合并；重复/乱序回放不会覆盖更高 revision。
- 读取文件和上传附件不自动进入目录。availability=not_checked；预览/内容读取负责最终可用性检查。
- scope=conversation_recorded_outputs，total_count 统计已知逻辑产物。recorded_evidence 只说明已记录通道，
  不宣称捕获了所有 shell / 第三方工具的文件系统写入；这些通道仍需要明确的产物声明。
- 旧 tool_finished 或旧 terminal diffstat 有限清单使 coverage=legacy_incomplete。
  只保留可证明的路径写入，不用被截断的 files_changed 假造完整产物总数；缺失 call ID 保持空值。
- limit 为 1–100；游标绑定会话、工作区身份及目录 revision，变化时 409；根摘要只取最近 5 项。

## Context 请求与来源

在所有 prompt override、TransformContext、context hook、日期/Git/reference 注入之后捕获 Runtime
交给 provider 的实际 model.Request。记录 invocation ID、模型、窗口、持久消息 revision、实际工具集合，
以及 byte/3 + message overhead 的估算组成。估算不是 provider tokenizer 的测量，不做强行比例归因。
图片等多模态成本尚未估算时，显式 multimodal_unestimated=true。

项目指令按 Builder 的真实注入边界匹配，区分全局/祖先/工作区的不同来源，即便同名同内容也不合并。
技能正文只在成功加载后登记；清理/压缩后内容不再匹配时，不计作仍加载。可用技能索引单独标识。
工具 origin 来自运行时接口而非名称前缀；hidden/plan-mode/tool-free 请求以实际 req.Tools 为准。

组成与测量分开，但必须能对齐同一请求：

- 根 Context 的 request 优先选择上次成功测量对应的快照；没有测量时选择最新请求。
- prompt_tokens、measured_at、measurement_snapshot_id、measurement_model、measurement_window 绑定同一 invocation。
- latest_request_snapshot_id 单独标记最新尝试；失败请求不会替换旧测量或把旧数值挂到新组成上。
- history_state 只表示持久消息相对展示快照是否改变，不保证下一次日期/Git 等临时内容完全相同。
- 旧会话仅有 token gauge 时维持 legacy_without_provenance，不伪造时间或文件来源。

来源正文/schema 在 checkpoint metadata 中按版本保存一次，请求事件仅保存引用和统计。
目录限制 2048 项及约 4 MiB 正文，旧版本被逐出时返回 410；未 checkpoint 即重启也可能仅剩引用。
来源详情必须属于指定会话的指定请求，校验内容 hash；不接受任意文件路径，不触发模型、加载技能或重连 MCP。
客户端从根摘要打开详情时必须传所展示的 snapshot_id，避免使用变化后的默认最新请求。

## 尚未交付

所有外部命令/第三方工具的产物声明、证据投影的增量缓存、磁盘来源变化提示，以及 AgentKit DTO/facade、
macOS 六组页面和任务入口迁移。读目录目前要求 EventKindStore；未支持的存储返回 unavailable，不全量回放巨大 stdout。
未完成全部语义与客户端联调前，静态和动态 hello 都抑制完整 session_rail_v1 capability。

## 验证范围

- server/mcp/jobs 包 race 测试；conversation 包测试。
- Runtime ConnectorSnapshot / WorkspaceMCP / Job / Background 相关 race 测试。
- 改动包 vet，`go build ./...`。
- 使用真实 MCP SDK 测试服务验证连接、关闭、allowlist 和插件 reload；使用实际 Git 仓库验证
  staged/unstaged、rename、binary、untracked、特殊文件名、unborn、merge conflict、submodule 与快照隔离。

Runtime 全包有上一批已对照 HEAD 复现的 5 个既有失败；本批只运行相关 Runtime 回归，不宣称全仓测试通过。

第三批新增：来源目录 JSON/checkpoint 重开、真实文件工具写入、请求 transform / hidden / tool-free、
技能正文清理、120 项分页、旧报告兼容、乱序预览、失败请求测量归属及新增端点认证测试。
agent 包 race 回归仅排除 TestRunTurnWithAssetsPreservesUserGatewayAsset；该旧失败已在未修改 HEAD 副本复现，未更改断言。
