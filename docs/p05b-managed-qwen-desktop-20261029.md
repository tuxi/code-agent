# P0.5-B 真实受管桌面实验：准入阻塞报告

日期：2026-10-29。结论：原生 MCP、Task 创建/关联、计划回报及 Talkify 父任务展示通过；受管 Run 在模型调用前被拒绝。本次未进入执行、审查、返工或 Finalize，不构成真实受管闭环验收。

## 授权与执行边界

用户明确授权本次使用 `opencode-go/qwen3.8-flash`，经 `https://opencode.ai/inference/go/openai/v1` 及其模型上游；只修改原隔离仓库的 `managed_smoke/labels.py`、`managed_smoke/test_labels.py`。仅允许外发任务必要上下文、原 GUI Session 历史与 Runtime 必需规则/工具定义；不发送密钥、环境变量值、.env、隐私或无关源码。

最多 3 轮执行（含首轮）、10 分钟；观测到 12 次请求、20 万输入或 1.6 万输出 Token 即取消。没有请求前硬配额，在途调用可能超限；使用现有 OpenCode Go 账户，不购买、不充值、不切模型。费用按实际账户规则记录，不承诺美元硬上限。审批保留给用户；持续失败即停止。

没有修改 Agent Loop、生成参数、Provider 配置或权限，没有另外启动 Daemon，没有推送或部署。

## 实际身份与 Task

- 原生 MCP：当前对话直接调用 `codeagent_*`，不是实验驱动模拟。模型目录 configured/available=true、verified=false；本地可用性不等于 API 请求成功。
- Endpoint：`https://127.0.0.1:55288`。TLS 使用系统 CA，未关闭验证。
- Server：`srv_31ff12dc-6485-453d-a565-53da1fe86f03`。
- Store：`cstore_16a5c66010fa0840a975cb9ff13173e8`。
- Session：`20261027-165016-b3490cad`。
- Workspace：`/private/tmp/codeagent-gui-workspace-__aaks0i/repo`。
- HEAD：`36b06b1aef49efbe10c572310281e5c842cfa88f`。
- 本次 Task：`ctask_5e59806b-fb58-4691-81e4-c274a1653950`，目标以 `MANAGED_QWEN_GUI_20261029` 开头。
- Planner：`part_6a9562fa-7f95-4a13-a845-e2aebb3e970f`；Reviewer：`part_f5067d4b-3189-4c89-bca7-90295b44e1fa`，均为当前 Codex 对话的 reported Participation。
- Executor：`part_42e50f8a-6a32-41be-a502-b3a2ce647c86`，关联原本地 Session，observation_level=managed。
- 计划：`artifact_5832915e-9413-401c-8b8c-1cb1a4d16227`，版本 1，hash `9c585618c92feadda6f1d099df07d581e4ecfd4604c3f1cd1d4895f9130f54a6`。

计划要求：标准库 Python 的 Unicode 标签归一化，严格输入类型、NFC、Unicode 空白折叠、casefold、再次 NFC、稳定去重、输入不变；由 unittest 覆盖 Unicode 与类型边界。最后运行明确的前台 unittest 命令和 `git diff --check`。

原有两个明确禁止执行的 metadata-only Task 未启动、未修改。本次另建 Task，验证了原 Session 可跨 Task 关联。

## 时间与结果

以下为北京时间：

| 时间 | 事实 |
| --- | --- |
| 16:06:43 | 原生 MCP 创建本次 Task |
| 16:07:39 | 持久化计划回报完成 |
| 16:08:22 | 首次原生 MCP `codeagent_start_task_run`，HTTP 409 |
| 随后 | 使用完全相同 request_id 和参数仅诊断重试一次，仍 HTTP 409，停止启动尝试 |
| 16:10:51 | 原生 Snapshot 确认无 Run、无 Executor Turn |
| 16:13:46 | 结束只读定位，隔离工作区仍无 managed_smoke |

从 Task 创建到只读定位结束约 7 分 3 秒。模型阶段没有准入，10 分钟模型执行计时未实际开始。

| 项目 | 实际值 |
| --- | --- |
| 启动请求 | 2 次，同 request_id：`managed-qwen-20261029-run-1` |
| 新 ManagedRun / 执行 Turn | 0 / 0 |
| 本实验 Provider 模型请求 | 0 |
| 输入 / 输出 Token | 0 / 0 |
| 本实验模型调用费用 | US$0（没有发起 Provider 请求） |
| 代码修改 / Diff | 0；managed_smoke 未创建 |
| 新增测试执行 | 未进入该阶段 |
| 审查返工 / PASS / Finalize | 均未发生 |

最终 Task 为 waiting / planning / not_reviewed，revision=5、snapshot_task_seq=12、managed_run 缺省，只有计划 reported Attempt。两次失败未创建部分 Run/租约/执行 Attempt；没有改 request_id 绕过冲突。

## 准入前检查

同一 Runtime 的 capabilities 包含 managed workflow、review gate、executor model。目标 Session 为 done，无 active Turn，工作区匹配且未归档。

已验证 Store 的只读 SQL 聚合结果：目标工作区 claim=0、execution lease=0；目标 Session pending Turn input=0，job_started/task_started bracket=0；本 Task run count=0。

但全 Store 查询发现：2,665 条 job_started 和 32 条 task_started，其事件分区没有同 ID 的 sessions 行，共 2,697 条。这里统计的是事件行，不是 2,697 个已确认存活或未退出的 OS 进程。

## 根因与证据

`internal/session/sqlite/managed_recovery.go` 的 `rejectUnprovenWorkspaceProcesses` 对全库执行以下存在性判断：

```sql
SELECT EXISTS(
  SELECT 1 FROM session_events e
  LEFT JOIN sessions s ON s.id=e.session_id
  WHERE s.id IS NULL AND e.kind IN ('job_started','task_started')
)
```

任意命中即返回 `collab.ErrRecoveryRequired`，没有先区分正常子流分区与实际丢失的父会话，也未证明其与本次工作区有关。

`internal/runtime/jobs_sink.go` 明确规定并实现：Job 事件写入 job_id 独立分区，起止 bracket 另复制到所属父 Session。正常 Job 子流没有同 ID 的 sessions 行是合法持久化形态。因此上述判断可把合法子流误判为 orphan，并阻塞同 Store 中无关工作区的受管准入。

有限只读样本验证：从被查询命中的 job_started 中取 12 条，检查其附近事件行的 Kind、payload.SessionID、TurnID、CallID；**12/12 都有匹配且所属 sessions 行存在的父会话副本**。这证明至少样本中的记录为正常双分区 bracket，不代表已逐一证明全部 2,697 条安全，也不能据此推断任何后台进程已停止。

一次试图全量关联的只读诊断过慢，已仅终止本轮诊断 Python 进程；随后改为有 5 秒进度上限、最多 12 条样本的查询，约 0.24 秒完成。未停止 Daemon 或用户其他任务，未修改数据库。

## 错误透传问题

`writeTaskError` 未单独映射 `ErrRecoveryRequired`，该错误的 Unwrap 指向 ErrConflict，最终返回普通 `task_conflict`。原生 MCP 又只报告 HTTP 状态，无法看见恢复原因。

相同请求的诊断响应：HTTP 409，应用 envelope code=40900、msg=Conflict、data.code=task_conflict；trace_id=`09c0b3d1-78d1-4d61-8b8b-685a654522d1`。

因此客户端仅看到“冲突”，无法分辨幂等 payload 冲突、工作区占用与恢复保护拒绝。这次不能归因于 Qwen 编码能力、输出预算或模型 API。

## Talkify 实际核对

本次 Task 在真实协作任务列表和父 Timeline 出现，显示目标、三个角色、计划版本/正文和 5 条持久事件。启动前与被拒后均显示 waiting/planning、0 轮执行、not_reviewed；没有伪造 Executor 工具轨迹，也没有新增审批。

失败启动未生成 Run，父 Timeline 因此没有执行或恢复事件；具体准入错误必须通过命令错误返回清晰展示，不能依赖尚未创建的 Run。

## 下一批建议

1. 修复子流/父 Session/工作区归属判定：正常且可证明归属的子流不能仅因没有同 ID sessions 行而全库阻塞。目标工作区的未知副作用、无法关联的记录仍须保守拒绝；不能简单删除查询或把 job_finished 当作进程死亡证明。
2. 完整透传结构化准入原因：HTTP 保留 recovery_required 等边界错误；MCP 解析经过白名单约束的安全错误 code，而不是仅返回 HTTP 409。不得把私密服务端原始错误直接回显。
3. 离线新增真实双分区 Job 历史回归：正常他工作区 Job 子流与现存父会话、本工作区未知进程、真正失去父会话的记录、同请求重试均覆盖。
4. 修复部署并离线验证后，再申请下一次真实实验授权。保留本次 Task/计划与诊断证据，不清库、不改租约、不换独立 Daemon 绕过保护。

本次实验已因持续准入失败停止；没有自动重启或继续消耗本次模型授权。报告只记录实际发生的行为，不以此前离线测试替代本次真实执行验收。
