# 模型发现、选择与命令执行可靠性

实现范围：补齐 P0-A 基础接口。没有 TaskAttempt 新表、Supervisor、Flux ReviewGate 或 AgentKit UI 改动；没有运行新的第三方模型实验。

## 共用模型目录

现有 `GET /v1/runtime/models` 继续来自 Settings / Provider Registry 的展开结果。保留 `runtime-model-catalog/v2`、`default_runtime_alias`、connections 与原有能力字段；增加：

- `default_model`：可选择的 connection/model 命名空间 ID，wire model 本身包含 `/` 时完整保留。
- 模型 `id`、`configured`、`available`、`unavailable_reason`、`verified`、可选 `verified_at` / `actual_model`、`configuration_id`。
- `verification_scope: process`、`verification_max_age_seconds: 86400`。

`configured` 表示该条目来自已应用配置。`available` 只检查受支持的协议、有效 endpoint、有效凭据或明确的无认证配置；不会探测远端模型是否存在、余额、服务状态或权限。仅声明 injected/keychain 并不代表凭据已经注入。

`verified` 表示当前进程在最近 24 小时观察到该路由/模型/默认 reasoning 配置的有效模型响应，附实际时间；不是下一次请求成功的保证。缓存最多 1024 项、重启清空。更改 endpoint 或 reasoning 配置会改变 configuration_id，历史成功不会直接沿用；实际密钥值不进入 fingerprint。凭据轮换本身不抹除同一配置的历史成功。持久化的 model_finished 事件仍可用于跨重启审计；本版不把历史事件重新投影为实时 verified。

目录不含 endpoint、API Key 或原始 resolver 错误。可用性和配置指纹逻辑在 Settings 层共享；MCP 不维护另一份模型白名单，也不直接读取用户全局配置。

## 派发模型选择

增加只读 MCP `codeagent_list_models`，投影同一个 HTTP 目录。`codeagent_send_to_session` 复用已有可选 `model`：

```json
{"session_id":"s1","request_id":"work-1","message":"执行已批准的修改与测试","model":"my-provider/vendor/model"}
```

示例 ID 仅为协议说明。省略 model 时原样交给 Runtime 使用其默认模型；Bridge 不填充猜测值。HTTP 目录从 Executor 的 ModelResolver 读取实际默认选择，因此 CLI --model 或 host Reconfigure 覆盖不会被误报为 settings 文件的旧默认。创建会话不改变模型，选择只发生在派发新 Turn 时。正在执行的 Turn 保留已构造 Runner；查询目录和后续选择不热切换运行中的模型。

显式选择在 MCP 派发前检查目录；未知、不可用条目明确报错。原有 Direct Provider 未知模型回退默认的路径改为 `model_unavailable`。Runtime 复用有效 resolver 在构造 Runner 时再次检查可用性；错误不尝试另一个模型。未配置模型、空模型目录继续保留既有错误。保留 Gateway 的历史 bare wire-model 兼容路径，但带 provider 命名空间的未知 ID 必须报错。

保留友好 alias 与 AgentKit opaque alias。旧版 HTTP v2 目录没有新 ID 时，Bridge 生成供发现使用的 connection/wire ID，实际派发其已声明 runtime_alias，避免旧 Runtime 把陌生 ID 回退到默认。新目录发送原始选择 ID。幂等重试冻结首次派发的 payload，不因目录变化重新选择模型。

## 身份、用量与验收

Provider 响应解析保留真正的 `actual_model`（OpenAI-compatible、Responses、Ollama，流式与非流式）；上游没有报告时保持缺失，不使用请求 ID 冒充。

`model_finished` 增加 `model_selection` 和 `model_succeeded`，沿原 EventStore / HTTP / WebSocket 持久化和回放，仍携带 prompt/completion/total/cached tokens 和 elapsed_ms。Selection 包含 requested_model（默认继承时 null）、resolved_model（命名空间 ID）、provider（连接 ID）、actual_model 和 configuration_id。Session metadata 的 last_model_call 保存最后一次身份/usage 摘要，不添加 Session schema 或 TaskAttempt 表。MCP artifacts 的 model_calls 提供每次逻辑模型调用的这些证据。重试的物理请求次数仍由既有 Provider telemetry 负责；不根据最后一份 usage 推算未报告的失败请求费用。

模型调用成功、工具命令完成、测试通过、开发任务验收是不同事实：

- `model_succeeded` 只表示获得有效模型响应。
- foreground run_command 的 status / exit_code / note 描述命令执行。
- `turn_finished` 只表示 Agent Loop 结束。
- artifacts 的 `task_acceptance` 固定为 `not_reviewed`；P0-A 不替 Codex 作审查 verdict。

## 超时、取消和回收

问题机制：标准 `exec.CommandContext` 默认终止直接子进程；孙进程继续继承 stdout/stderr 时，`Cmd.Wait` 还可能等待管道 EOF。前次实验还观察到 Git 的 Xcode shim 反复触发 xcodebuild。这里修复可复现的进程/管道机制，并不宣称证明了前次全部阻塞原因。

新增共享 `processexec.Configure`：Unix 命令创建独立 process group，超时/取消杀死该组，结束后清理残留组成员；`WaitDelay=1s` 限制父进程退出后的管道等待。用于 run_command 的直接命令与 sh -c 路径、后台 job 取消和 Bridge Git 证据采集。前台结果明确区分 completed / failed / timed_out / cancelled；异常管道关闭不能变成 exit_code 0。

回归测试启动真实父子测试进程：子进程继承两个输出描述符并持续写 heartbeat，分别覆盖 deadline、主动 cancel、父进程先退出；验证有界返回且子进程停止工作。前台工具集成测试覆盖单命令与 compound shell 两条路径。

边界：Unix group 无法收回主动脱离该组的进程；非 Unix 保留直接子进程取消和管道等待上限，本版没有实现 Windows Job Object。同步输出回调仍应由上层保证不阻塞；WaitDelay 不会强制终止任意阻塞的 Go 回调。

## 离线验证

新增模型发现/选择/可用性、真实 Daemon → stdio MCP、actual model 解析、身份/usage、进程树取消测试。普通测试不调用第三方模型，live driver 默认跳过；live driver 改为显式 CODEAGENT_P0A_SETTINGS 文件，不再内置 DeepSeek endpoint、模型或 reasoning 参数。

相关完整包测试中的既有失败在临时基线克隆复现，保留原有 registry.go 修改作对照：Runtime 的 4 项 Flux、事件输出预算；Server 的 Provider 模板数量、2 项配对、pre-admission rejection；Agent 的 Gateway asset 保留测试。它们没有在本次改动中修复。新增功能使用聚焦回归和 race 检查，未运行全仓库测试。

下一次真实验证需要重新明确接收方、选定模型、数据范围及调用预算；此前一次性 DeepSeek 授权不延续。

最终结果：上述 6 个直接相关包的完整 race 测试、Runtime/Server/Agent/MCP 的功能聚焦 race 测试、Conversation 模型相关测试、`go build ./...`、相关包 `go vet` 与 `git diff --check` 均通过。最终相关完整包失败集合与临时基线完全一致，共 10 项；没有新增失败。验证汇总保存在 `/private/tmp/codeagent-foundation-validation.json`，完整审查 Diff 在 `/private/tmp/codeagent-foundation-review.patch`（排除原有 `.gitignore` / `internal/app/registry.go` 改动，未跟踪的 P0-A 文件以完整内容提供）。
