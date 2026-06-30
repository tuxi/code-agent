# Runtime 需求 Spec：GitHub 仓库 clone 进 workspace（iOS / go-git）

> 状态：Draft · 面向 CodeAgent Go Runtime 团队
> 依赖：runtime 的 **go-git 回填**（团队列的能力缺口 A.#1）。本 spec 是该 batch 的一个 API 面。
> 范围（v1）：**仅公开仓库**，无鉴权。私有仓库 / PAT 留待后续 spec。

## 0. TL;DR

希望在 iOS 上「输入一个 GitHub 仓库 → 它出现在工作区里可被 agent 读写」。

iOS 沙盒禁 fork/exec → host **不能** shell 调 `git`，也不宜再塞一套 host 端 libgit2（会与
go-git 重复）。所以 **clone 必须由 runtime 内的 go-git 完成**：host 发一个 clone 请求，runtime
把仓库 clone 进 `workspaceDir`（= iOS 沙盒 Documents）下的一个子目录，返回该项目路径；该目录
天然是 `root="workspace"`，复用既有的 relativize/re-anchor、项目列表、copy-in 选中流程。

## 1. 为什么是 runtime 做

| 路径 | 可行性 | 取舍 |
|---|---|---|
| host shell `git` | ❌ | iOS 沙盒禁 fork/exec |
| host libgit2/SwiftGit2 | ⚠️ | 现在能做，但 vendore 重依赖，且与未来 go-git **重复两套 git 实现** |
| **runtime go-git** | ✅（待 batch） | 单一 git 实现；clone 落在 runtime 已持有的 workspaceDir 内，权限/路径天然正确 |
| host 下载 zipball 解压 | ✅（现在） | 仅快照、无 `.git`、不能 pull/push —— 已评估，**本次不选**，选 go-git 真 clone |

## 2. API

### 2.1 `POST /v1/repos/clone`
请求体：
```jsonc
{
  "url":  "https://github.com/owner/repo",  // 也可接受 "owner/repo" 简写，由 runtime 规范化
  "ref":  "main",                            // 可选：分支/tag；缺省 = 仓库默认分支
  "name": "repo",                            // 可选：目标目录名；缺省取 repo 名
  "depth": 1                                  // 可选：shallow 深度；见 §4
}
```
行为：
- clone 目标 = `workspaceDir/<name>`（即 Documents 下）。**必须落在 workspaceDir 内**，
  拒绝任何越界路径（`name` 含 `/`、`..`、绝对路径 → 报错）。
- 目标名冲突 → **自动加序号**（`repo`、`repo-2`…），**绝不覆盖**已有目录（与 host copy-in 的
  `uniqueDestination` 同语义）。
- 返回：
```jsonc
{
  "workspace_path": "/var/.../Documents/repo",        // 本次解析的绝对路径（host 仅展示）
  "workspace_ref":  { "root": "workspace", "rel": "repo" }  // 持久身份，re-anchor 友好
}
```
> `workspace_ref` 与 workspace-path spec 一致：持久身份是 `rel`（相对 workspaceDir），
> 绝对 `workspace_path` 是本次启动的临时值，host 不持久化它。

### 2.2 同步 vs 异步 / 进度
clone 可能慢（大仓 / 慢网）。两种形态，runtime 定，但请明确告诉 host：
- **同步**：`POST` 阻塞到 clone 完成再返回。简单；host 显示不确定进度转圈。需设**合理超时**
  （建议 ≥ 120s，可配）。
- **异步 job + 进度**：`POST` 立即返回 `{ job_id }`，进度通过 …（候选：复用现有 WS 事件流推
  `clone_progress` 帧 / 新增 `GET /v1/repos/clone/{job_id}` 轮询 / SSE）。go-git 支持 sideband
  进度回调，可映射成百分比或 "Receiving objects…" 文本。
- **建议**：v1 先**同步 + 合理超时**（host 转圈即可）；进度流作为增强。**入口形态请和 host 对齐**
  （就像 rebind 那次：放 WS 帧还是 HTTP 轮询，需要约定）。

## 3. 错误语义
结构化错误，host 据此提示：
- 无效 URL / 非 GitHub host → `invalid_url`
- 仓库不存在 / 私有（公开 API 返回 404/401）→ `repo_not_found`（v1 仅公开，私有等同不存在）
- 网络失败 / 超时 → `network_error`（可重试）
- 目标写入失败（空间不足等）→ `io_error`
- `ref` 不存在 → `ref_not_found`

## 4. 边界 / 安全
- **仅公开仓库**：v1 不带任何凭证、不读任何 token。私有仓库后续 spec（PAT 走 host Keychain，
  下载/clone 时由 host 注入 `Authorization`，或 runtime 从 secrets 读——届时定）。
- **路径约束**：clone 结果必须在 `workspaceDir` 内；`name` 规范化 + 越界拒绝。
- **shallow 默认**：建议默认 `depth=1`（go-git 支持）——省空间/时间，host 拿到的是可用工作副本
  （仍含 `.git`，可后续 fetch/unshallow）。对「分析仓库」场景足够；需要完整历史时传 `depth=0`/省略。
- **体积上限（可选）**：可设最大 clone 体积/对象数保护，超限报错，避免塞满 Documents。

## 5. Host 侧（AgentKit）后续，待本 API ready 再接
- 「Import from GitHub」菜单项（与 New Project / Import Folder 并列）→ 输入 repo URL（+可选 ref）。
- 调 `POST /v1/repos/clone` → 成功后 `ProjectsStore.reload()` + 选为当前草稿的 workspace。
- 复用现有 copy-in 的命名/选中 UX；clone 期间显示进度/转圈。
- **在该 API ready 前，host 不实现网络调用**（不对空接口编码，避免接口未定就先写死）。

## 6. 交付清单（runtime 侧）
- [ ] go-git clone（public、shallow 默认）落 `workspaceDir/<name>`，越界拒绝、冲突加序号。
- [ ] `POST /v1/repos/clone`：入参 `url`/`ref`/`name`/`depth`；返回 `workspace_path` + `workspace_ref`。
- [ ] 同步 + 超时（或异步 job + 进度；入口形态与 host 对齐）。
- [ ] 结构化错误（§3）。
- [ ] 仅公开仓库，不读任何凭证。
- [ ] 单测：公开仓库 clone 成功 / 不存在仓库 / 冲突加序号 / 越界 name 拒绝 / ref 不存在。
