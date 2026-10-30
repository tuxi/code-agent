# Case 002 · WorkBuddy 直接派发的可行性评估

2026-10-30。状态：**评估结论，未实现、未授权**。未修改 Runtime、未派发 Turn、未调用任何模型。

## 结论

**当前不可行**，卡点不在工具，而在凭据模型。

WorkBuddy 要派发，就必须持有 Bridge 能接受的凭证；而 Bridge 只接受**owner 级** access token，没有 per-client 作用域。给 WorkBuddy 这个 token，等于把生产 Daemon 的完整权限交出去——与"不得暴露 owner 凭据"直接冲突。

在不改凭据模型的前提下，只有两条替代路径，且都各有硬伤（见第 3 节）。

## 1. 现状核查

| 项 | 事实 | 出处 |
| --- | --- | --- |
| WorkBuddy 现有连接器 | `codeagent_reviewer`，7 个只读工具，无派发能力 | 本轮已验收 |
| Reviewer 面是否有 send | 没有；方法层 12 条写路径返回 `ErrObserveOnly` | `reviewer_tools.go`、`client.go` |
| Executor 面需要什么 | `--connection-manifest`（含 `access_token`）或 `--runtime-url` + token 环境变量 | `cmd/codeagent-mcp/main.go` |
| manifest 的权限级别 | owner 级；12 小时；绑 `server_id` / `store_id` / `daemon_pid` | `connection.go` |
| 服务端认证 | 单一 access token（≥32 字节），无 per-client 作用域、无能力声明 | `internal/server/auth.go` |
| 会话存储 | 按 workspace 分库：`~/.codeagent/projects/<name>-<hash>/sessions.db` | 生产 Daemon 实际打开的文件 |
| 数据根 | 由 `$HOME` 推导（`~/.codeagent/...`），Daemon 没有 `--data-dir` 参数 | `cmd/codeagentd/main.go` |

推论：**"换一个端口再起一个 Daemon"不等于数据隔离**——同一个 `$HOME` 下，任何 Daemon 都能打开全部 project store。真正的隔离要换 `$HOME`。

## 2. 为什么 Reviewer 面帮不上忙

Reviewer 面是**离线证据包**模式：读一个冻结文件，没有 daemon 连接、没有 token、没有网络。它无法派发，也不应该能派发——这正是 P0-C 的设计目标。

要在 WorkBuddy 里派发，必须再加一个 executor 连接器，而那个连接器需要 manifest。

## 3. 三条路径与代价

### A. 受限凭据（推荐，但需要改服务端）

新增一种**能力受限的 token**：绑定 workspace、可选绑定 session、只允许 `send` / `read` / `handoff`，带过期时间。Bridge 侧再加一个 `--profile dispatcher`，只注册派发与读取工具，其余在方法层拒绝（复用 `requireWritable` 的反向逻辑）。

- 满足：WorkBuddy 能派发、不能越权、owner 凭据不外泄。
- 代价：改服务端认证中间件、manifest schema、Bridge profile、Chater 授权 UI。是本次评估里唯一"正确"的方案，但属于架构扩展。
- 关键点：即使 stdio 子进程能读到 manifest 文件，读到的也只是受限 token，爆炸半径限制在单个 workspace。

### B. 隔离 Daemon（不改代码，但会丢掉 Talkify）

用 `HOME=/tmp/case002/home` 起第二个 Daemon，独立 store、独立 token、独立端口；给它手工构造 manifest 交给 WorkBuddy。

- 满足：不暴露生产凭据。
- 硬伤 1：**Talkify 观察的是生产 Daemon**，看不到这个隔离实例。审批只能在派发它的客户端里做，与"用户在 Talkify 观察和处理审批"矛盾。
- 硬伤 2：隔离 `$HOME` 里没有 `secrets.json`，要么复制 provider 凭据过去（又一次凭据扩散），要么另配。
- 硬伤 3：manifest 需要手工构造（格式已知，但正常由 UI 生成），等于绕过产品的授权路径。

### C. 人工中转（零凭据扩散）

WorkBuddy 只生成派发请求（任务文本 + request_id + validation_commands）写入文件，由 Owner 或 Codex 执行 `send`。

- 满足：完全不动凭据模型。
- 硬伤：不是"WorkBuddy 直接调用"，只是把编排权交给了 WorkBuddy。

## 4. 最小必要条件（若选 A）

按依赖顺序，缺一不可：

1. **服务端**：认证层接受第二种 token，带 claims（`workspace_id`、`session_id?`、`actions`、`expires_at`），并在每个请求上校验。
2. **签发**：Owner 能生成这种 token，且不经过第三方客户端。最短路径是 Chater 增加"授权委派（受限）"动作。
3. **manifest**：增加 `scope` 字段；`loadConnection` 校验其存在与内容。
4. **Bridge**：新增 `--profile dispatcher`，只注册 `create_session` / `send_to_session` / `read_session` / `wait_for_updates` / `get_handoff` / `connection_info`，其余方法层拒绝。
5. **审计**：派发记录要能归属到"WorkBuddy 的受限授权"，而不是笼统的 owner 操作。

工作量落在 1、2 两项，属于服务端与宿主 App 的改动，不是 Bridge 内部能解决的。

## 5. 权限风险

| 风险 | 现状 | 若走 A |
| --- | --- | --- |
| owner token 落到第三方进程可读位置 | 是（stdio 子进程可读 manifest） | 否，读到的只是受限 token |
| 越权读其他 workspace 的会话 | 可能 | 不可能（claims 限定） |
| 越权写、取消、审批 | 可能 | 不可能（actions 限定） |
| 审批被客户端代答 | Bridge 不放行审批 | 不变 |
| 重复派发 | 无跨进程租约 | 不变，仍靠 request_id 幂等 |

## 6. 建议

1. **本轮不做 A**。它是服务端改动，与"不扩大架构"冲突，且当前没有任何一次成功的 WorkBuddy 派发作为需求证据。
2. **先用 C 跑通一次**：WorkBuddy 出派发请求，Owner 执行 send，验证"派发后结束轮次 → 独立执行 → 一次 Handoff 恢复"这条链路本身是否成立。这不需要任何凭据改动。
3. 如果 C 证明链路有价值，再评估 A 的投入。届时 A 的收益是"省掉人工中转"，成本是服务端 + 宿主 App 的授权改造。

换句话说：**先证明流程有价值，再为流程改权限模型**，而不是反过来。

## 7. 未验证项

- `opencode-go/deepseek-v4.1-flash` 是否在模型目录中（需 `codeagent_list_models`，executor 面才有）。
- 隔离 Daemon 方案在 macOS 上换 `$HOME` 后能否正常加载 provider 凭据（未实测）。
- Chater 是否有可复用的"受限授权"入口（未查 UI 源码）。
