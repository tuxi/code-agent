# Case 001 · TimelineKit

2026-10-30，Asia/Shanghai。状态：**Delivered（本地交付）**。

## 任务与交付

VideoEditorDemo iOS 的真实视频/图片缩略图展示，以及项目列表窄屏、宽屏布局适配。Code-Agent 实现，Codex 规划与独立审查，用户在 Talkify 处理审批并完成人工 UI 验收。采用 Manual Collaboration，没有受管 ReviewGate/Finalize 验收。

| 项目 | 记录 |
| --- | --- |
| 项目 | TimelineKit |
| 执行模型 | `opencode-go/qwen3.8-flash`；实际路由/响应已在原 Talkify 会话观察 |
| 执行 Session | `20261030-052252-b8f78ce8` |
| 返工 Turn | `turn_18e33a6ab0de1858_45` |
| 原基线 | `34e59c4e5bec71e3a33ff6ec6c8f36e35de1cdad` |
| 交付提交 | `8def8a27da4b5b496d4172ff44e22f1d21c4e2ad` |
| 分支与合并 | `codex/videoeditor-thumbnails-layout` → `main`，`--ff-only`，无冲突 |
| 交付范围 | 3 个实现文件、2 个测试文件、3 张验证截图 |
| 远程操作 | 未推送；未删除任务 worktree |

本轮只读 Git 核验：提交的直接父节点为原基线，main 与任务分支均指向交付提交，任务 worktree 干净；原 checkout 中既有未跟踪 shared scheme 仍保留。没有修改交付代码或重新运行该项目任务。

## 独立审查的实际价值

Codex 发现真实项目可有多个 `isMainTrack` 轨道，首个为空、后续轨道有视觉内容；原封面解析只取首个主轨，导致有媒体却显示无缩略图。审查反馈发送回原 Session，Code-Agent 定向修复并补充多主轨回归测试，没有修改 fixture 来掩盖缺陷。

这证明一次独立审查确实发现并推动修复了真实边界问题；没有与 Codex 单独执行作同任务对照，不能据此宣称协作整体质量或效率更优。

## 最终验证来源

原 Codex 对话「配置 Code-Agent 手工协作」，交付对话 Turn `01a18c72-a674-7402-82f0-cb2b1a6a721a`：

- 无管道 `swift test`：真实命令记录 completed / exit 0；对话交付报告为 0 失败。
- 无管道 iOS Simulator `xcodebuild ... build`：真实命令记录 completed / exit 0；对话报告 BUILD SUCCEEDED。
- 最终 Diff 审查：对话报告无未解决问题，清理一处尾随空格。
- UI：用户亲自运行并确认通过。本记录没有重新独立验证全部设备视觉矩阵。
- commit 与 `git merge --ff-only`：原对话命令记录均 completed / exit 0；本轮另以只读 Git 对照提交及分支。

早先 Handoff 的 completed / unknown / not_reviewed 是当时真实状态，保留不改写。本 Case 的 Delivered 是后续人工协作交付结论，不代表回填了 Daemon Task Accepted，也不伪造不可变 ReviewBundle。

## 效率数据与局限

本案例主要使用旧协作流程：接入、临时 stdio、重复状态读取及 Codex 补验证带来额外工作，审查阶段曾触及 Codex 账户额度限制。它不是新版异步零轮询实验。

| 指标 | 记录 |
| --- | --- |
| 是否有真实审查返工 | 有，针对多主轨封面缺陷 |
| 新版桌面只读恢复 | 原生 connection_info 1 次、Handoff 1 次、send/wait 0；详见异步验收记录 |
| 执行期主动轮询精确总数 | unknown；已观察到旧流程轮询，不满足零轮询目标 |
| Codex 总参与次数/有效审查比例 | unknown；未完成统一口径统计 |
| 人工审批精确次数/完整端到端耗时 | unknown |
| Code-Agent 模型费用、完整 Token 消耗 | unknown |
| Codex 账户实际消耗及节省比例 | unknown；不估算 |

交付后工作区 HEAD 已前进。旧 Session 的原基线不能静默改成新 HEAD；新任务应准备新工作区基线和新 Session。重载旧实验 Bridge 因 HEAD 不符而拒绝时，不清库、不 reset、不改写历史来绕过。

关联：[Collaboration UX P0](../collaboration-ux-p0.md)、[异步桌面验收](../collaboration-async-desktop-acceptance.md)、[下一次验收模板](next-async-acceptance.md)。
