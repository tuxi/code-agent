# P0.5 受管协作桌面验证准备

2026-10-29。状态：原生 MCP 与桌面表单已验证；用户授权的本次真实实验在 Run 准入阶段连续返回 409，已停止，模型请求为 0。详见 [实际实验报告](p05b-managed-qwen-desktop-20261029.md)。

用户选择 `opencode-go/qwen3.8-flash`；为受管 Run 补充了可选 `executor_model`，实现和离线结果见 [模型选择说明](p05b-executor-model-selection.md)。新版桌面表单及当前对话原生 MCP 均已通过只读核对。用户随后明确授权本次范围与观测预算；已创建独立 Task、关联角色并回报计划，但未创建 Run 或执行 Turn。授权实验因持续失败停止，后续重试须在修复与重新确认后进行。

## 原生 MCP 恢复实证

用户在约 16:03 提供截图，`/mcp` 菜单仍未显示 `codeagent_gui`，但本轮工具清单已实际暴露该 namespace。直接通过当前对话原生工具调用（未使用临时 stdio 驱动）验证：

- `codeagent_list_models` 成功：默认仍为 `openrouter/openrouter/free`；选择的 `opencode-go/qwen3.8-flash` configured=true、available=true、verified=false。
- `codeagent_read_task` 成功：原 Task 仍 waiting/planning、snapshot_task_seq=5；本地 Participation 指向原 Server、Store 和 Session。
- 本次实际调用成功确认原生连接已恢复，不再要求重启、全局注册或重新授权。菜单未列出该项的具体原因尚未确定，不将其展示情况等同于工具不可用。
- 两次调用均为本机只读操作，没有启动模型、派发 Turn 或创建 Run。下列失败诊断保留为恢复前历史。

## 最新连接诊断

- 项目 `.codex/config.toml` 的 `codeagent_gui` 已启用，`codex mcp get codeagent_gui --json` 能读取预期命令和原 Session 参数，disabled_reason=null。用户配置中本仓库 trust_level=trusted，未改变信任或访问范围。
- 当前桌面日志明确记录：2026-10-29T07:38:30.776Z 本对话 `codeagent_gui` starting，下一条日志在 07:38:30.777Z 显示 failed，错误为初始化握手时 connection closed。因此不是“配置没有读取”，也没有证据表明应全局注册。子进程当时为何关闭仍待进一步错误信息确认。
- 用户反馈 `/mcp` 无该项后进一步核对：授权文件最后更新于 07:51:27 UTC（北京时间 15:51:27），晚于上述失败约 13 分钟；最近日志没有该项的新启动记录。当前会话的持久 cwd 正确，仍为 code-agent。新授权下再次 `--check-connection` 成功。因此下一步仅重新启动 ChatGPT 并重进本会话，让它在有效授权之后重新加载；保持 Talkify 和授权不变。此时间顺序不能单独证明 15:38 子进程的具体退出原因，但说明旧失败未验证新授权。
- 同一配置的独立 stdio Bridge、TLS/Store 身份校验及模型目录读取已成功；真实 Talkify 已显示新模型选择表单。这些成功不替代当前对话原生 MCP 加载验收。
- 桌面自动化工具明确拒绝操作 `com.openai.codex`，无法代点客户端的 MCP 重新连接。已请用户通过当前对话 `/mcp` 查看该项，若有 Restart/重新连接则仅操作该项；保持 Talkify 与原授权不变，不再盲目重启两个 App。
- 只读复核隔离仓库 HEAD 仍为 `36b06b1aef49efbe10c572310281e5c842cfa88f`，只有原 `gui_smoke/` 未跟踪目录；未创建 `managed_smoke/`，未修改实验文件。

官方说明：项目级 MCP 配置适用于 trusted 项目，客户端可通过 `/mcp` 查看连接。来源：[OpenAI MCP 文档](https://learn.chatgpt.com/docs/extend/mcp)。

## 较早检查记录

- Talkify 已验证 P0.5-A 读取与 P0.5-B 的真实 Task 创建/Executor 关联。
- 两个现存 GUI smoke Task 的目标均明确禁止执行，不将它们改成受管 Run。
- 原隔离 Session 为 `20261027-165016-b3490cad`，工作区 `/private/tmp/codeagent-gui-workspace-__aaks0i/repo`，HEAD 仍为 `36b06b1aef49efbe10c572310281e5c842cfa88f`。
- 工作区已有先前实验留下的 `gui_smoke/`，保持不变。
- 当前原生 MCP 的 list_models/read_task 均返回 `local MCP connection is closed; authorize a new connection`。源码确认旧 binding 的 context 已关闭，不能自动追随 replacement grant；应恢复等范围授权并重载 Bridge，保持现有 Daemon 运行。
- 用户恢复授权/刷新后，当前对话仍返回旧连接关闭。以相同 manifest、workspace、Session、baseline 启动新的只读 CLI 检查成功：`https://127.0.0.1:57461`，原 Server `srv_31ff12dc-6485-453d-a565-53da1fe86f03`、Store `cstore_16a5c66010fa0840a975cb9ff13173e8` 均匹配。沙箱内 PID 检查会误报无效；获准在沙箱外检查后通过，没有绕过服务端认证。
- 新临时 stdio Bridge 仅调用一次 `codeagent_list_models`，结束后关闭；当前默认 `openrouter/openrouter/free`，Provider `openrouter`，wire ID `openrouter/free`，available=true、verified=false、reasoning_effort=low、context_window=200000。没有调用 Provider。该诊断不替代本对话原生 MCP 的恢复。
- 下一操作为仅重启 ChatGPT 以释放旧 Bridge，保持 Talkify/授权不变；本轮免费路由实验授权另行请求，收到前不派发。

## 拟定最小任务

重新创建独立 Task，精确关联原隔离 Session 的 Executor 与本次 Codex reported Planner/Reviewer。文件范围仅 `managed_smoke/labels.py`、`managed_smoke/test_labels.py`，不复用旧实验文件。

实现标准库 Python 函数：输入必须是 list，元素必须是 str；逐项 NFC 规范化、Unicode 空白折叠、casefold；删除空值并按首次出现顺序去重，返回新 list，保持输入不变。测试覆盖空列表、空字符串、连续/非 ASCII 空白、中文、大小写、Straße/STRASSE、组合/分解重音、无效输入以及输入不变。

验证命令：`/usr/bin/python3 -B -m unittest discover -s managed_smoke -p test_labels.py -v`。

Codex 审查必须检查实际冻结 bundle、文件内容和工具执行证据，并独立运行测试；没有模型自述验收。若实际发现问题，提交绑定该版本的 request_changes，允许预算内有限返工；若首轮合格可提交 PASS，不为展示返工而捏造缺陷。

## 启动前与验收

1. 恢复本对话真实 MCP，验证相同 Server/Store/Workspace/Session；查询 Runtime 默认模型及命名空间、Provider、能力。不根据旧 UI 菜单推断实际默认模型。
2. 确认现有受管能力 ready、Session 没有运行 Turn/待审批或占用租约；没有充分证据时不重复派发。
3. 取得本轮明确的 Provider、必要源码/历史上下文外发范围、请求/Token/费用/时长与返工上限授权。历史授权均不沿用；Runtime 保留原有生成配置。
4. 通过 MCP 创建计划与 Participation，用户可在 Talkify 查看。启动采用现有 GUI 确认或原生 MCP；启动前确定唯一派发方，不能两个客户端各提交一个新启动意图。
5. 同时核对 Talkify Session 工具与审批、父 Task 的真实 Attempt/阶段、不可变 ReviewBundle。审批由用户处理。
6. PASS 回报后继续读取权威 Snapshot，直到 `task_acceptance=accepted`；Flux success、review recorded 或 Turn completed 均不单独代表任务通过。
7. 只读重开父任务，确认计划、审查、Gate/Finalize 和对应历史冻结内容。记录实际调用/Token/耗时/费用（不可获取时标未知）、Diff、测试与差异。

本文件是准备方案，不是模型调用许可或真实受管桌面验收报告。当前未派发任何 Turn、未调用 Provider、未修改隔离工作区文件。
