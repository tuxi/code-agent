# Case 002 · Codex 异步派发与恢复审查（计划）

2026-10-30 23:45 修订。状态：**Draft / 未授权执行**。计划内容已确认，但尚未获得模型调用与源码外发授权；派发方（Codex 或 WorkBuddy）待定，见 [派发可行性评估](002-dispatch-feasibility.md)。

主实验只回答一个问题：**Codex 派发后能否结束当前轮次，执行期间零轮询，最后用一次 Handoff 恢复审查。**

WorkBuddy 独立复核列为可选 002-B，不并入主流程。

## 0. 本轮修订

| 项 | 调整 |
| --- | --- |
| 功能定位 | 明确面向**支持项目级 TOML MCP 配置的客户端**，与 WorkBuddy 必须走 UI 的行为区分开 |
| 基线 | 不再假定旧值，实测核验后固定为 `240f7ce`（含 P0-C 与 P0.1） |
| 模型 | 改为 `opencode-go/deepseek-v4.1-flash`（用户 23:18 确认）；派发前仍需 Codex 用 `codeagent_list_models` 核对目录中存在 |
| 提交 | 由 Owner 在首次 Turn 结束后创建候选提交；不要求 Code-Agent 自动提交 |
| 审查定位 | 改称 **first-candidate independent review**，不称盲审 |

## 1. 功能定位：给谁用

`--print-project-config` 生成的是 **TOML 片段**，面向支持项目级 MCP 配置文件、且允许用 `command` + `args` 启动本地 stdio 服务的客户端：

- 适用：Codex CLI / Desktop 的 `.codex/config.toml`、Claude Code 类客户端。
- **不适用：WorkBuddy。** 实测结论（P0-C 记录）：WorkBuddy 没有可手写的明文 MCP 配置——`~/.workbuddy-ai/mcp.json` 不被读取，`connectors/default/mcp.json` 未被修改，真实定义在账户级加密 store，只能经 UI 添加并信任。

所以这个功能解决的是"Codex 侧如何生成 reviewer 配置"，不解决 WorkBuddy 接入。文档措辞要避免让读者以为打印一份 TOML 就能把 WorkBuddy 接上。

## 2. 任务与文件范围

**T2-1**：`codeagent-mcp --print-project-config` 支持 `--evidence-bundle`，生成无凭据的 reviewer 配置。

| 文件 | 行 / 字节 | 改动 |
| --- | --- | --- |
| `cmd/codeagent-mcp/project_config.go` | 40 / 1,884 | 新增 bundle 分支；该模式不输出 manifest / workspace 绑定参数 |
| `cmd/codeagent-mcp/main.go` | 168 / 7,897 | 把 `--evidence-bundle` 透传给 `projectConfig` |
| `cmd/codeagent-mcp/project_config_test.go` | 41 / 1,370 | 新增 bundle 用例；旧三种模式输出逐字节回归 |
| `docs/p0c-multi-client-bridge.md` | 249 / 14,878 | 补一节"如何生成 reviewer 配置（TOML 客户端）" |

合计 498 行 / 26 KB。除上述 4 个文件外**只读**。

验收标准：

1. `--print-project-config --evidence-bundle /abs/x.json` 输出 TOML，`args` 仅含 `--experimental-p0a --evidence-bundle /abs/x.json`。
2. 输出中不出现 `connection-manifest`、`--workspace`、token 相关字段。
3. bundle 路径必须绝对；相对路径报错。
4. 新建 / attach / reattach 三种旧调用输出**逐字节不变**。
5. 生成器不读凭据、不联网、不写文件。

## 3. 基线（实测核验，2026-10-30 23:12）

```text
HEAD    = 240f7cef4de1fc6223bbafa5d753fb238d262c85
branch  = main（ahead 11 of origin，未推送）
worktree= 无已跟踪文件改动，仅 7 个与本任务无关的未跟踪文件
```

该提交包含 P0-C（observation + reviewer + bundle）、P0.1（分页与体积优化）及相应文档，是目前唯一"已验收且包含 P0-C/P0.1"的提交，因此作为新任务基线。

未跟踪文件不进入任务工作区：隔离副本由本地 `git clone` 得到并 `checkout 240f7ce`，基线干净且可复现。

## 4. 隔离工作区与 Session

| 项 | 计划值 |
| --- | --- |
| 工作区 | `/tmp/case002/repo`（**已创建**），本地克隆后 `git checkout 240f7ce` |
| 分支 | `case002-candidate`（已创建，指向基线，无提交） |
| 基线 | `240f7cef4de1fc6223bbafa5d753fb238d262c85` |
| Session | 新建，不复用 Case 001 的 `20261030-052252-b8f78ce8` |
| 远端 | **已移除 origin**，`git push` 物理不可行 |

基线自检（2026-10-30 23:22，在任务工作区执行）：

```text
git status        = 0 个改动
go build ./...    = 通过
go test ./cmd/codeagent-mcp = ok
```

说明四条校验命令在基线上本来就是绿的，执行后若变红，是本次改动导致，不是环境问题。

## 5. validation_commands（发送时声明，禁止管道）

```sh
go build ./...
go vet ./cmd/codeagent-mcp
go test ./cmd/codeagent-mcp -count=1
go test ./internal/mcpbridge -count=1
```

四条独立执行，不接 `| tail` / `| grep`。Case 001 的教训：管道使 `exit_code` 变成 `tail` 的，证据不可信。真实退出码与失败输出一律保留，不改写。

## 6. 执行模型、外发范围与预算

| 项 | 计划值 | 状态 |
| --- | --- | --- |
| 模型 | `opencode-go/deepseek-v4.1-flash` | 已确认 |
| Provider 可用性 | 需确认 provider 当前可用、凭证有效 | **未确认** |
| 外发范围 | 上表 4 个文件 + `go.mod` + 任务文本 | 待授权 |
| 预算 | 见 §6.2，**建议值，非硬限制** | 待授权 |

外发**不含**：`~/.codeagent/reviews/` 下的证据包、Talkify 的 `connection.json`、TimelineKit、其他未列出的仓库文件。

### 6.1 模型可用性

本地 `~/.codeagent/settings.json` 中 `opencode-go` provider 存在且 `enabled=true`，`base_url=https://opencode.ai/inference/go/openai/v1`，但 `models` 字段为 `null`——模型目录不静态枚举。

因此 `deepseek-v4.1-flash` 是否在目录中，必须在派发前由 Codex 调用 `codeagent_list_models` 确认。该工具在 executor 面，reviewer 面不提供。查不到就停止，不要改用未确认的模型名。

### 6.2 预算（建议值，不是硬限制）

以下数字是**建议预算**，用于判断"这次调用是否值得继续"，**不是**可自动执行的截断规则：

| 项 | 建议值 |
| --- | --- |
| 模型调用次数 | 15 次 |
| 累计输入 | 300k tokens |
| 累计输出 | 20k tokens |

必须说清的边界：

- **当前没有已验证的硬预算能力。** Runtime 不提供按 Turn 的 token 或调用次数上限，Bridge 也没有 budget_exhausted 协议（P0-A 文档已记录）。因此不得声称"触顶自动截断"或"超限即中止"。
- 实际约束方式是**人工判断**：执行期间由用户在 Talkify 观察，异常时手动取消；审查阶段由 Codex 在 Handoff 里核对用量。这属于过程约束，不是系统强制。
- 用量数据以实际可取得的证据为准。拿不到精确值时标 **unknown**，不用估算值冒充，也不把目录价换算成账单。
- 费用同样处理：能拿到账单就记实际值，拿不到就写 unknown。

## 7. 操作步骤（单 Turn）

**A · Codex 规划与派发**

1. 原生 `codeagent_connection_info` 核对 workspace / Session / Runtime / Store。
2. 任务文本写清：目标、允许修改的 4 个文件、验收标准、四条校验命令、禁止事项（不合并、不推送、不新增 MCP 工具、不改 bundle schema）。
3. `codeagent_send_to_session`，稳定 `request_id`，带 `development.validation_commands`。
4. 拿到持久准入回执（session / turn / cursor）后**结束当前回复**。不 wait、不 read、不设唤醒。

**B · 执行期**

5. 用户只在 Talkify 观察、处理审批。Codex 主动轮询目标 **0**。

**C · 恢复审查**

6. 用户确认结束后回到 Codex 对话。
7. Codex 只调用**一次** `codeagent_get_handoff`。
8. 按需 `get_artifacts`：先 summary，再按 `path` + `offset` 分页读 Diff。
9. 给出审查结论。本轮**只安排一个 Turn**，不主动安排返工；确需返工另立计划。

## 8. 候选冻结（Owner 执行）

首次 Turn 结束后：

1. **由 Owner** 在 `case002-candidate` 分支创建候选提交，记录 `baseline..candidate` 两个 SHA。
2. 不要求 Code-Agent 在执行期间自动提交；Bridge 工具指令默认禁止 commit，这条不改。
3. 候选提交不合并 main、不推送。
4. 从 `baseline..candidate` 导出证据包，记录 `bundle sha256`；此后审查只引用该 bundle 与这对 SHA。

## 9. 审查定位：first-candidate independent review

准确说法是**首次候选版本的独立复核**，不是盲审。

`read.events` 保留该 Turn 的指令与执行者自述。指令是需求（不是缺陷结论），自述是被审对象的声明（不是独立发现），所以比 Case 001 的返工 Turn 干净得多；但审查者仍能看到"要做什么"和"执行者说做了什么"。这不足以称为盲审。

要接近盲审，得在导出时裁掉指令与自述，那会改动证据语义，本阶段不做。

## 10. 记录方式

| 指标 | 采集方式 | 目标 |
| --- | --- | --- |
| Codex 主动轮询次数 | Codex 对话工具记录 | **0** |
| 恢复审查 Handoff 调用数 | 同上 | **1** |
| send 次数与 request_id | Receipt | 1 次成功 |
| 人工步骤 | 逐步记录：授权、审批、候选提交、导出、验收 | 全部列明 |
| 校验命令退出码 | `handoff.commands[].exit_code` + stdout | 4 条真实值 |
| 实际模型调用次数 / Token | Handoff 或 Runtime 可取得的证据 | 取不到则 **unknown** |
| 费用 | 实际账单 | 取不到则 **unknown** |
| 审查发现 | 具体缺陷或 PASS，附证据引用 | 不预设必须发现 Bug |
| 最终验收 | 用户结论，与审查结论分开记录 | 互不替代 |

## 11. 停止条件

- 校验命令非零退出且原因不明。
- Executor 要求改动范围外文件。
- 审批长时间无人处理。
- 预算或授权到期。
- Provider 不可用。

出现任一情况即停止并保留证据，不放宽约束、不自动重试。

## 12. 待确认

1. Provider `opencode-go/deepseek-v4.1-flash` 当前可用性与凭证状态（由 Codex 用 `codeagent_list_models` 核对）。
2. 源代码外发范围与预算是否批准（第 6 节）。
3. 是否确认基线 `240f7ce`。
4. 是否同时准备 002-B（WorkBuddy 用同一 bundle 独立复核）。

## 13. Codex 交接包

工作区已就绪，Codex 直接使用以下参数，不需要重建。

**绑定参数**

```text
workspace : /tmp/case002/repo
baseline  : 240f7cef4de1fc6223bbafa5d753fb238d262c85
branch    : case002-candidate（已存在，无提交）
```

**派发前**

1. `codeagent_list_models` 确认 `opencode-go/deepseek-v4.1-flash` 存在；不存在即停止。
2. `codeagent_create_session`（新 Session，不要复用 Case 001 的）。

**任务文本（可直接使用）**

```text
任务：为 codeagent-mcp 的 --print-project-config 增加 --evidence-bundle 支持。

背景：目前该生成器只能输出 executor 绑定（connection-manifest + workspace +
session），无法生成 reviewer 配置。本功能面向支持项目级 TOML MCP 配置的客户端
（如 Codex 的 .codex/config.toml），不涉及 WorkBuddy——后者只能通过 UI 添加连接器。

允许修改的文件（其余只读）：
- cmd/codeagent-mcp/project_config.go
- cmd/codeagent-mcp/main.go
- cmd/codeagent-mcp/project_config_test.go
- docs/p0c-multi-client-bridge.md（新增一节）

要求：
1. 新增 --evidence-bundle 模式，输出 args 仅含
   --experimental-p0a --evidence-bundle <绝对路径>。
2. 该模式输出中不得出现 connection-manifest、--workspace、token 字段。
3. bundle 路径必须是绝对路径；相对路径返回错误。
4. 新建 / attach / reattach 三种旧调用输出必须逐字节不变（补回归测试）。
5. 生成器不读凭据、不联网、不写文件。

验证命令（每条独立执行，不要接管道）：
  go build ./...
  go vet ./cmd/codeagent-mcp
  go test ./cmd/codeagent-mcp -count=1
  go test ./internal/mcpbridge -count=1

禁止：合并 main、推送远端、新增 MCP 工具、修改 internal/mcpbridge 业务逻辑、
修改 bundle schema、提交（候选提交由 Owner 在审查前统一创建）。
```

**send 参数**

```text
request_id : case002-t2-1-turn1
model      : opencode-go/deepseek-v4.1-flash
development.validation_commands :
  ["go build ./...",
   "go vet ./cmd/codeagent-mcp",
   "go test ./cmd/codeagent-mcp -count=1",
   "go test ./internal/mcpbridge -count=1"]
```

拿到回执后结束回复，不要 wait / read / 设唤醒。
