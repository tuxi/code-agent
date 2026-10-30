# P0-C：MCP Bridge 多客户端只读审查

2026-10-30。状态：**C1 + C2 已实现；真实 daemon 上已完成 observe 绑定与证据导出；离线 reviewer 读取通过；C4 暂缓**。

目标只有一个：让 WorkBuddy 以真正的只读 Reviewer 身份审查已交付的 TimelineKit Case 001，同时不改弱执行 Bridge 的准入校验、不把 owner 凭据交给第三方客户端。

## 结论

- 历史审查不能复用执行绑定。执行绑定要求 `HEAD == baseline`，历史审查要求"读取已记录的提交区间"，两者安全语义不同。本实现新增独立的 observation 模式，**执行路径的 `HEAD == baseline` 与干净工作区校验一字未改**。
- 审查证据不再取自"当前工作区"，而取自 `baseline..delivery` 提交区间。当前 HEAD、分支、未提交修改只作为独立字段上报，不参与 diff。
- 第三方客户端**不接触 daemon 和 owner 凭据**：owner 侧一次性导出冻结证据包（JSON + SHA-256 sidecar），reviewer 侧只读该文件，无网络、无 token。
- Reviewer profile 只有 7 个只读工具；注册层不暴露写工具，方法层对 12 条写路径返回 `ErrObserveOnly`。

## 一、Step 0 离线兼容性探测（已完成）

用真实二进制在 `--evidence-bundle` 模式跑完整 MCP 握手：无 daemon、无网络、无凭据。

| 协议版本 | 协商结果 | 工具数 | `tools/list` 体积 | Instructions |
| --- | --- | --- | --- | --- |
| 2025-11-25 | 2025-11-25 | 7 | 2437 B | 304 B |
| 2025-06-18 | 2025-06-18 | 7 | 2437 B | 304 B |
| 2025-03-26 | 2025-03-26 | 7 | 2437 B | 304 B |
| 2024-11-05 | 2024-11-05 | 7 | 2437 B | 304 B |

对照：executor 面 22 个工具，`tools/list` 为 **30,568 B**。reviewer 面为 **2,298 B**（本地测量），线上 2,437 B，相差一个数量级。

一个实测发现：体积主要由 SDK 自动派生的 `outputSchema` 贡献，而不是工具描述。7 个只读工具带 outputSchema 时是 12,178 B，去掉后是 2,298 B。reviewer 面因此把输出声明为 `any`：返回的 JSON 内容完全不变（仍在 `structuredContent` 与 `text` 中），只是不再声明冗余 schema。弱客户端不需要解析它。

结论：协议层不是风险，工具面体积才是。探测脚本在 `/tmp/codeagent-probe/probe.py`（未入库，避免把临时工具当产品）。

## 二、C1：历史观察绑定

新增 `--observe-session`，必须同时给 `--baseline-commit` 与 `--delivery-commit`。

`internal/mcpbridge/observe.go`：

- `validateCommitRange`：两个提交都必须是完整 SHA、都必须存在于仓库、baseline 必须是 delivery 的祖先、两者不得相等。任一不满足直接拒绝，不降级、不猜测。
- `NewObserverFromManifest`：复用 manifest 的 server_id / store_id 身份核对，绑定 session 后进入只读模式。不恢复 admission、不重连控制 socket、不要求队列空闲。
- `rangeDiff` / `rangeFiles`：`git diff baseline delivery`，与当前 HEAD 无关。
- `headState`：单独报告 `head_commit` / `head_moved` / `worktree_dirty`。

证据对象新增字段（`Artifacts` 与 `Handoff` 同构）：

| 字段 | 含义 |
| --- | --- |
| `evidence_source` | `workspace`（执行）或 `commit_range`（观察） |
| `delivery_commit` | 被审查的交付提交 |
| `head_commit` / `head_moved` | 当前 HEAD，以及它是否已离开交付提交 |
| `worktree_dirty` | 工作区是否有未提交修改 |
| `evidence_complete` | 证据是否完整（截断即为 false） |

**明确没有做的事**：没有把 `HEAD != baseline` 改成"允许"，也没有让 observe 模式回写执行绑定。`client.go` 的执行分支保持原样，observe 走独立的 `delivery != ""` 分支。

### 真实 Case 001 验证（只读 Git）

对 `/Users/xiaoyuan/Documents/work/git/TimelineKit` 执行区间读取：

```text
range = 34e59c4e5bec71e3a33ff6ec6c8f36e35de1cdad..8def8a27da4b5b496d4172ff44e22f1d21c4e2ad
files = 8
diff_sha256 = 21bb4e94b4e187871c9bfd631b05b1660b49d023228ae95591305706c10bde2f
head_moved = false    worktree_dirty = true
```

与独立执行的 `git diff 34e59c4..8def8a2 | shasum -a 256` 完全一致。8 个文件为 3 个实现文件、2 个测试文件、3 张验证截图，与 Case 001 记录相符。

值得注意：该工作区 `worktree_dirty=true`（既有的未跟踪 shared scheme）。旧绑定会因为"干净工作区"直接拒绝，新绑定把这一点作为字段上报，且**不污染 diff**。这正是本次要修的问题。

## 三、C2：Reviewer Profile

`internal/mcpbridge/reviewer_tools.go`，7 个工具：

`codeagent_connection_info`、`codeagent_read_session`、`codeagent_wait_for_update`、`codeagent_get_handoff`、`codeagent_get_artifacts`、`codeagent_read_task`、`codeagent_read_review_bundle`。

三道边界，缺一不可：

1. **注册层**：写工具根本不注册，客户端即使手工构造 `tools/call` 也没有目标。
2. **方法层**：`Create` / `Send` / `Cancel` / `AttachEmptySession` / `CreateTask` / `RegisterTaskAgent` / `ParticipateTask` / `ReportTask` / `LinkTaskTurn` / `LinkTaskArtifact` / `StartTaskRun` / `ReviewTaskRun` 首行返回 `ErrObserveOnly`。
3. **绑定层**：observe 模式只能读一个已存在的 Session，不能新建、不能改绑。

`codeagent_wait_for_update` 在离线证据包上立即返回，不空转：冻结证据不会变化。

## 四、C3：最小只读授权方案（不建认证体系）

原提案是"reviewer 用只读 profile 连 daemon"。这条路不成立：stdio 模式下 Bridge 是客户端拉起的子进程，owner token 就在它读取的文件里，profile 只能防误操作，防不住客户端。

因此改为**证据包**：

```text
owner 侧（持有凭据，只跑一次）
  codeagent-mcp --observe-session <id> --baseline-commit <sha> --delivery-commit <sha> \
                --export-evidence /abs/case-001.json --export-turn <turn_id>
  → case-001.json (0600) + case-001.json.sha256

reviewer 侧（无凭据、无网络、无 daemon）
  codeagent-mcp --evidence-bundle /abs/case-001.json
  → 7 个只读工具，从文件读取
```

- 导出内容：精确 Turn 的事件、commit-range artifacts、handoff、可选 Task Snapshot 与不可变 ReviewBundle。
- 完整性：sidecar SHA-256 必须匹配，否则拒绝打开；篡改或替换会被发现，而不是被"审查"。
- 一致性：`session_id` 在 bundle / read / artifacts / handoff 之间必须一致，`turn_id` 同理；请求其他 Turn 直接报错。
- `client_id` 不当作授权。`codeagent_register_task_agent` 只记录"自报身份"，属于归属信息，不是凭据。这一点在代码里没有混用：reviewer 面根本不注册该工具。

证据包仍然是一个本地文件，owner 导出后交给谁由用户决定；它不是租户隔离，也不是长期凭证。

## 五、C4：暂缓

没有加 `--listen`。原因：豆包客户端是否支持 stdio 未验证，提前开放本机 HTTP 服务只会增加攻击面。若确认只支持远程 URL，再单独设计 loopback 绑定、认证与 Origin 校验，不复用本次代码。

## 六、验收状态

| 验收项 | 结果 |
| --- | --- |
| 只读面恰好 7 个工具、全部 `readOnlyHint` | 通过（`TestReviewerSurfaceIsReadOnly`） |
| executor 面仍保留写工具且更大 | 通过（同测试） |
| 12 条写路径全部返回 `ErrObserveOnly` | 通过（`TestObservationModeRefusesEveryWritePath`） |
| HEAD 前进 + 工作区 dirty 时 diff 不变 | 通过（`TestCommitRangeEvidenceIgnoresCurrentHeadAndWorktree`） |
| 反向区间、缺失提交被拒绝 | 通过（同测试） |
| 证据包哈希校验、篡改拒绝、Turn 越界拒绝 | 通过（`TestEvidenceBundleIsFrozenVerifiedAndReadOnly`） |
| Case 001 真实区间 diff 与 `git diff` 一致 | 通过（`TestObserveRealRepositoryRange`，opt-in） |
| 四代协议握手与工具面体积 | 通过（离线探测） |
| 真实 daemon 上的 observe 绑定 | **通过**（`--check-connection`，`identity_verified=true`） |
| 真实 daemon 上的证据导出 | **通过**（`--export-evidence`，sha256 `576b827d…`） |
| 离线 reviewer 读取真实证据包 | **通过**（7 工具、`bundle_verified=true`） |
| 离线 reviewer 尝试写操作 | **通过**（协议层 `unknown tool "codeagent_send_to_session"`） |
| WorkBuddy / 豆包客户端实际接入 | **未运行**（需用户在其客户端配置） |

回归命令：

```sh
go test ./internal/mcpbridge ./cmd/codeagent-mcp -count=1 \
  -skip 'Test(LiveDaemonRoundTrip|DaemonStdioToolRoundTrip|ExistingDaemonReadOnlySocketReconnection|ExistingSessionHandoffReadOnly)$'
go vet ./internal/mcpbridge ./cmd/codeagent-mcp
go build ./...
```

## 七、Case 001 导出与审查（实测通过的命令）

以下命令已在本机实际执行成功。导出只读 daemon 与 Session，不提交 Turn、不修改工作区、不重启 Daemon。

```sh
cd /Users/xiaoyuan/Documents/work/git/code-agent
MANIFEST="$HOME/Library/Application Support/Talkify/CodeAgentMCP/connection.json"
WS=/Users/xiaoyuan/.codex/worktrees/videoeditor-thumbnails-layout/TimelineKit
BASE=34e59c4e5bec71e3a33ff6ec6c8f36e35de1cdad
DELIV=8def8a27da4b5b496d4172ff44e22f1d21c4e2ad
SID=20261030-052252-b8f78ce8
TURN=turn_18e33a6ab0de1858_45

# 1. 先验证身份与绑定，不提交任何东西
./build/codeagent-mcp --experimental-p0a --connection-manifest "$MANIFEST" \
  --workspace "$WS" --observe-session $SID \
  --baseline-commit $BASE --delivery-commit $DELIV --check-connection

# 2. 导出冻结证据包（0600 + sha256 sidecar）
./build/codeagent-mcp --experimental-p0a --connection-manifest "$MANIFEST" \
  --workspace "$WS" --observe-session $SID \
  --baseline-commit $BASE --delivery-commit $DELIV \
  --export-evidence /tmp/codeagent-case001/case-001.json \
  --export-turn $TURN --exported-by codex

# 3. 第三方客户端只读审查（无凭据、无网络、无 daemon）
./build/codeagent-mcp --experimental-p0a \
  --evidence-bundle /tmp/codeagent-case001/case-001.json
```

`--workspace` 必须是 Session 自己的 `workspace_path`。填错时 Bridge 现在会直接报出该路径，而不是只回一句"不匹配"：

```text
session_observation: existing session does not match the bound workspace
(bridge root=/Users/xiaoyuan/Documents/work/git/TimelineKit):
workspace_path=/Users/xiaoyuan/.codex/worktrees/videoeditor-thumbnails-layout/TimelineKit;
pass --workspace with the session's own workspace_path
```

### 实测结果

```text
bundle      = /tmp/codeagent-case001/case-001.json (243,953 B, 0600)
bundle_sha  = 576b827de6438d32a86828b2586ac5e8e3d9565ce3f19a06ca7b72d6cd814aa5
range       = 34e59c4e..8def8a27
diff_sha256 = 21bb4e94b4e187871c9bfd631b05b1660b49d023228ae95591305706c10bde2f
changed     = 8 files（3 实现 / 2 测试 / 3 截图）
commands    = 9 条记录（swift test 等，含 exit_code）
status      = execution completed / validation unknown / acceptance not_reviewed
```

`diff_sha256` 与独立执行的 `git diff 34e59c4..8def8a2 | shasum -a 256` 逐字节一致。

一个必须记录的事实：`validation_status=unknown` 不是缺陷。该 Turn 在发送时没有声明 `development.validation_commands`，Bridge 因此不做任何 PASS 推断——这正是"缺失检查保持 unknown"的设计。要让下一次审查拿到 `complete`，发送时必须声明校验命令并指示 Executor 执行。

## 八、WorkBuddy 侧配置

```json
{
  "mcpServers": {
    "codeagent_reviewer": {
      "command": "/Users/xiaoyuan/Documents/work/git/code-agent/build/codeagent-mcp",
      "args": ["--experimental-p0a", "--evidence-bundle", "/tmp/codeagent-case001/case-001.json"]
    }
  }
}
```

参数里没有 token、没有 manifest 路径、没有 workspace 路径。reviewer 的启动参数里不存在任何可被读取的凭据。

## 九、已知边界

- 证据包是**导出时刻的快照**，不随后续变化更新；`head_moved` / `worktree_dirty` 记录的是导出时的工作区状态。
- 证据包本身是普通文件。交给哪个客户端、要不要长期保留，是用户的决定；它不是租户隔离，也不是长期凭证。
- 只读面不提供 `codeagent_lookup_admission` 与 `codeagent_list_models`：前者服务于"发送方避免重复派发"，后者服务于模型选择，都与审查无关。
- 没有跨进程派发租约。两个 executor Bridge 仍可能在同一 workspace 各建 Session，这属于独立风险，本次未处理。
- 同一 Session 的后续 Turn 不在本包内。需要新证据就重新导出，不要复用旧包。

## 十、Case 001 实际接入记录（2026-10-30）

### 10.1 WorkBuddy 客户端配置落点

| 位置 | 实际情况 |
| --- | --- |
| `~/.workbuddy-ai/mcp.json` | **不被读取**，日志中无解析记录 |
| `~/.workbuddy-ai/connectors/default/mcp.json` | **未被修改**，`codeagent` 出现 0 次，mtime 仍为 2026-07-08 |
| `~/.workbuddy-ai/connectors/<account>/connector-states.v3.json` | 账户级 store，aes-256-gcm 加密，无法明文核对 |
| `~/.workbuddy-ai/mcp-approvals.json` | 明文可核：`<sha256>::codeagent_reviewer` |

结论：**没有可手写的明文配置路径**，只能经 UI「MCP Server Management」添加并信任。手写配置文件无效，这是排障时最容易走错的一步。

### 10.2 原生调用结果

| 工具 | 结果 |
| --- | --- |
| `codeagent_connection_info` | `bundle_verified=true`、`identity_verified=false`（离线包无实时身份，正确）、Session `20261030-052252-b8f78ce8`、`34e59c4e..8def8a27` |
| `codeagent_get_handoff` | `commit_range` / `completed` / `unknown` / `not_reviewed`、`evidence_complete=true`、`diff_sha256=21bb4e94…`、8 文件、9 命令 |
| `codeagent_get_artifacts` | 调用成功，但结果 **92,518 字符**，超过客户端单次上限并落盘 |

`get_artifacts` 的问题已单列为 [P0-C1 分级获取设计](p0c1-artifact-retrieval.md)。

### 10.3 派生证据包

Case 001 是**返工 Turn**：`turn_started` 携带返工指令、`turn_finished` 是修复报告、`thinking` 含根因。原样提供给审查方等于提前给出答案。

因此该次复核使用派生包：`read.events` 只保留 `turn_accepted`，diff/handoff/命令证据不动，源与派生的 sha256 记在包的 `PROVENANCE.txt`。**这是 independent recheck，不是 blind review**：交付代码注释里仍写着 `Regression (Codex review, …)` 与"首轨为空"的场景。

要获得真正的盲审，必须在返工之前导出，或选择不含指令与报告的 Turn。

## 关联

[P0-A MCP 实验](p0a-mcp-experiment.md)、[P0-C1 分级获取](p0c1-artifact-retrieval.md)、[双 GUI 接入](dual-gui-implementation.md)、[Case 001](collaboration-cases/001-timelinekit.md)、[外部 agent 协作调研](cross-agent-collaboration-research.md)
