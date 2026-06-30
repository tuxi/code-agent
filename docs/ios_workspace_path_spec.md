# Runtime 改造 Spec：iOS workspace_path 持久化适配

> 状态：Draft · 面向 CodeAgent Go Runtime 团队
> 背景渠道：内嵌 mobile runtime（`MobileStart`），由 iOS host(AgentKit) 进程内启动

## 0. TL;DR

mac 端绝对路径持久化方案在 iOS 上会**重装即失效**。原因：iOS 沙盒绝对路径
（`/var/mobile/Containers/Data/Application/<UUID>/Documents/...`）中的 `<UUID>`
在重装 / 系统迁移 / 从备份恢复时会变，Apple 明确要求不得持久化沙盒绝对路径。

而 runtime 把每个 session 的 `workspace_path` 以**绝对路径**存进了 session DB → 下次
启动这些路径全部指向不存在的旧容器。

**核心修复**：runtime 不再持久化绝对路径。改为持久化「相对 `workspaceDir` 的相对路径」
（项目在 Documents 下的情况），load 时用**本次启动传入的** `workspaceDir` 重新拼接
（re-anchor）。外部目录（不在 `workspaceDir` 下）由 host 负责，runtime 在 attach 时
接受 host 重新传入的新鲜绝对路径并以之为准。

本改动**仅影响 iOS / mobile 形态**，mac server 行为保持不变。

---

## 1. 问题陈述

### 1.1 现状
- `MobileStart(workspaceDir, dataDir, ...)` 每次启动由 host 传入**当前**的沙盒路径：
  - `workspaceDir` = app 的 `Documents/`
  - `dataDir` = app 的 `Application Support/`（session DB 落这里）
- `POST /v1/conversations { workspace_path }` 创建会话，runtime 将 `workspace_path`
  **绝对路径**写入 session 记录。
- `GET /v1/conversations`、conversation detail 返回该绝对 `workspace_path`。

### 1.2 安全 vs 中毒的判定框架
| 来源 | iOS 安全性 | 原因 |
|---|---|---|
| **启动参数**（`workspaceDir` / `dataDir`） | ✅ 安全 | 每次启动 host 用当前沙盒路径重新计算后传入，天然 re-anchor |
| **持久化进 DB 的绝对 `workspace_path`** | ❌ 中毒 | 写入时的 `<UUID>` 被冻结，下次启动对不上 |

> 一句话：凡是「每次启动重新喂进来」的值都安全；凡是 runtime「自己存成绝对路径」的值在
> iOS 上都会烂。修复 = 把后者转换成前者。

### 1.3 路径变化的真实时机（用于评估影响面）
- 重装 app（每次 Debug 部署 / TestFlight 更新都触发）→ **必变**
- 系统大版本升级 / 从备份恢复 / 换机迁移 → **可能变**
- 普通杀进程重启：同一安装内通常不变，但**不可依赖**（Apple 不保证）

`dataDir` 同样会变，但因为它是启动参数、每次现传，所以 DB 文件总能被打开；**唯一被毒化的
是 DB 内容里的绝对 `workspace_path`**。

---

## 2. 目标与非目标

### Goals
1. iOS 上 session 的 workspace 绑定在重装 / 迁移后仍正确解析。
2. mac server 路径行为**零变化**（绝对路径继续可用）。
3. 平滑迁移已存在的（绝对路径）session 记录。
4. 为「外部文件夹（host 持有 security-scoped bookmark）」预留正确的协议形状。

### Non-goals
- 不要求 runtime 理解 / 创建 iOS security-scoped bookmark（这是 host/Foundation 的能力，
  Go 侧做不到）。
- 不改 mac server 的存储格式（除非用统一描述符且对 mac 透明，见 §4.4）。

---

## 3. 设计原则

1. **Workspace 的持久身份不是绝对路径。** 它是一个可移植描述符（portable descriptor）。
2. **绝对路径是「每次启动现算、用完即弃」的临时绑定**，由 re-anchor 或 host 重传得到。
3. **相对锚定优先，host 重传兜底。** 能用相对路径解决的（Documents 下项目）runtime 自己
   搞定；解决不了的（外部目录）靠 host 在 attach 时重传绝对路径。

---

## 4. 数据模型变更

### 4.1 Workspace 描述符（持久化形态）
将 session 记录里的 `workspace_path: string`（绝对）替换为结构化描述符：

```jsonc
// workspace_ref
{
  "root": "workspace" | "external",  // 锚点类型
  "rel":  "MyProject/sub",           // root=="workspace" 时：相对 workspaceDir 的相对路径
  "ext_id": "BKMK-7f3a...",          // root=="external" 时：host 提供的稳定 id（不可信绝对路径）
  "abs_hint": "/var/.../MyProject"   // 可选，仅用于日志/展示，永不作为解析依据
}
```

字段语义：
- `root = "workspace"`：项目位于 `workspaceDir` 之下。**`rel` 是唯一权威身份。**
- `root = "external"`：项目在 `workspaceDir` 之外（外部文件夹）。**`ext_id` 是 host 给的
  稳定标识**，runtime 不自行解析路径，必须等 host 在 attach 时重传绝对路径。
- `abs_hint`：纯展示/排障用，**任何代码路径都不得据此读写文件**。

### 4.2 DB schema
- 新增列（或迁移现有列）：`workspace_root TEXT`、`workspace_rel TEXT`、`workspace_ext_id TEXT`。
  保留 `workspace_path` 仅作为 `abs_hint`/迁移过渡，标注 deprecated。
- 索引：如有按 workspace 聚合的查询，改为按 `(workspace_root, workspace_rel, workspace_ext_id)`。

### 4.3 解析后的运行态（内存，不持久化）
```
type ResolvedWorkspace struct {
    AbsPath string   // 本次启动解析出的真实绝对路径
    Ref     WorkspaceRef
}
```

### 4.4 mac 兼容
mac 上 `workspaceDir` 可能为空 / 任意根。约定：
- 若 `workspace_path` 在 `workspaceDir` 之下 → 存 `root="workspace"` + `rel`（mac 也受益）。
- 否则 → 存 `root="external"`，但 mac 的 `ext_id` 可直接等于绝对路径（mac 路径稳定，
  host 不需 bookmark）。**这样同一套描述符在两端都成立**，差异只在「external 的 abs 是否
  稳定」——mac 稳定、iOS 不稳定。

---

## 5. 相对化（write path）与 re-anchor（read path）

### 5.1 写入：relativize on create
```
func toWorkspaceRef(absPath, workspaceDir string) WorkspaceRef {
    // 规范化，消除 .. / 符号链接 / 大小写差异
    abs := normalize(absPath)
    base := normalize(workspaceDir)

    if base != "" && isUnder(abs, base) {
        return WorkspaceRef{
            Root:    "workspace",
            Rel:     relpath(abs, base),   // e.g. "MyProject/sub"；root 自身 → "."
            AbsHint: abs,
        }
    }
    // 外部目录：runtime 不持有稳定身份，记 hint，等 host 在 attach 重传 / 提供 ext_id
    return WorkspaceRef{
        Root:    "external",
        ExtId:   "",        // 由 createConversation 的 ext_id 入参填充（见 §6.1）
        AbsHint: abs,
    }
}
```

### 5.2 读取：re-anchor on load
```
func (r WorkspaceRef) resolve(currentWorkspaceDir string, hostSuppliedAbs string) (string, error) {
    switch r.Root {
    case "workspace":
        // 用「本次启动」的 workspaceDir 重新拼 —— 自动跨重装/迁移正确
        return safeJoin(currentWorkspaceDir, r.Rel)   // 见 §8 安全 join
    case "external":
        // runtime 无法自行解析；必须由 host 在 attach 时重传新鲜绝对路径
        if hostSuppliedAbs != "" {
            return normalize(hostSuppliedAbs), nil
        }
        return "", ErrExternalNeedsHostRebind   // host 没重传 → 报错，UI 提示重新授权
    }
}
```

要点：
- `root="workspace"` 的会话**完全不需要 host 参与**，runtime 自给自足。
- `currentWorkspaceDir` 永远取**本次** `MobileStart` 传入值，绝不缓存上次的。

---

## 6. API 变更

### 6.1 `POST /v1/conversations`（create）
请求体扩展（向后兼容）：
```jsonc
{
  "workspace_path": "/var/.../Documents/MyProject", // 仍接受；runtime 内部 relativize
  "workspace_ext_id": "BKMK-7f3a..."                // 可选；外部目录时 host 传入稳定 id
}
```
行为：
- runtime 收到绝对 `workspace_path` → 走 §5.1 relativize。
- 若该路径不在 `workspaceDir` 下且带了 `workspace_ext_id` → 存 `root="external"` + `ext_id`。
- 返回的 `ConversationRef.workspace_path` 仍是**本次解析出的绝对路径**（host 可展示，但
  host 侧不得持久化它，见 §7）。

### 6.2 attach / resume：允许重传 workspace_path（兜底 + external 必需）
给现有 attach / 恢复会话的入口增加可选入参：
```jsonc
{
  "conversation_id": "...",
  "workspace_path": "/var/<NEW-UUID>/.../MyProject"  // 可选：host 本次启动重新解析出的绝对路径
}
```
行为：
- 若传入 `workspace_path` → runtime **以传入值为准**刷新该会话的运行态绑定（不改持久 ref 的
  `rel`/`ext_id`，只更新内存 `ResolvedWorkspace.AbsPath`，并可顺手刷新 `abs_hint`）。
- `root="external"` 的会话：**必须**由 host 重传，否则按 §5.2 报 `ErrExternalNeedsHostRebind`。
- `root="workspace"` 的会话：host 可以不传，runtime 用 re-anchor 自给自足；传了也接受（兜底）。

### 6.2bis external rebind：入口形态与时序（host 侧定稿）

§6.2 只说了「attach 可重传 `workspace_path`」，但没定**重传走哪个通道、什么时机**。基于
AgentKit 实际的两阶段连接时序（Phase 1 HTTP → Phase 2 WS），host 侧定稿如下。

#### 决策：HTTP、Phase 1、WS attach 之前。**不走 WS hello / 握手后帧。**

三条理由：
1. **`hello` 是 server→client，载不动 host→server 的 path。** 「hello 消息」选项在当前协议
   方向上不成立——除非新增 client-hello，那是更大的协议改动。
2. **握手后帧（类 `register_tools`）会引入「首 turn 竞态」。** rebind 帧在 hello 之后才到，
   而用户首条输入也走 WS；runtime 必须为 external 会话**阻塞首 turn 直到 rebind 帧到达**，
   否则 `Load` 可能在绑定刷新前就跑。有状态 gating，易错。
3. **HTTP-in-Phase-1 天然无竞态、且与 create 对称。** 流还没开、任何 turn 的 `Load` 还没跑，
   绑定就已刷新好；而 `createConversation` 本就是 HTTP POST 带 `workspace_path`。

#### Wire 形状
```
// Phase 1: host 先取 detail
GET /v1/conversations/{id}
→ { ..., "workspace_ref": {"root":"external","ext_id":"BKMK-7f3a"},
        "needs_rebind": true }

// needs_rebind=true → host 用 ext_id 查 bookmark → 解析出本次启动的绝对路径
//                     → startAccessingSecurityScopedResource → 重传
POST /v1/conversations/{id}/rebind
     { "workspace_path": "/var/mobile/.../Documents/MyProject" }
→ 200  // runtime 刷新内存 ResolvedWorkspace.AbsPath，校验路径存在

// Phase 2: 之后才开 WS stream，此时 Load 已能正确解析
WS  /v1/conversations/{id}/stream
```

#### 契约点（runtime 落实）
- **`needs_rebind` 语义**：`true` 当且仅当 `root=="external"` 且本次进程启动尚未 rebind。
  `root=="workspace"` **永远 false**（runtime 自锚定，host 零参与）。
- **rebind 幂等**；host 对 external 会话可无条件调用（即便 `needs_rebind=false`）做防御。
- **rebind 校验路径存在**，不存在返回错误 → host 提示「workspace 失联，请重新授权/重选」。
- **顺序保证由 host 负责**：rebind 返回 200 后才开 WS（host 侧保证）。
- rebind **只刷新内存绑定**，不改持久 ref 的 `rel`/`ext_id`（与 §6.2、§5.2 一致）。

#### 兜底选项（仅当 runtime 坚持单通道）
若必须收在 WS 上，**用 stream URL 的 query param**（`…/stream?workspace_path=…`），
**不要用握手后帧**。query param 在 upgrade 时即可得 → 同样无竞态，host 侧
`connectionValidatorRequest` 正好能塞。代价：绝对路径进 URL（loopback 无真实日志，影响小）。
优先级仍低于 HTTP-rebind。

### 6.3 `GET /v1/conversations` / detail
- 返回的 `workspace_path` = **本次启动 re-anchor 后的绝对路径**（对 `external` 未重传者可返回
  空或带 `needs_rebind: true` 标记，供 UI 提示）。
- 建议同时返回结构化 `workspace_ref`（`root`/`rel`/`ext_id`），便于 host 做展示与 bookmark 映射。

---

## 7. Host（AgentKit）侧契约（写给 runtime 团队对齐，不在本仓改）

- host **不得**持久化服务端返回的绝对 `workspace_path`（它是本次启动的临时值）。
- host 对 `root="workspace"` 会话：无需任何动作，runtime 自洽。
- host 对 `root="external"` 会话：
  1. 自己保存 `ext_id → security-scoped bookmark`。
  2. 每次 attach 前解析 bookmark → 拿到本次启动的真实绝对路径 → 通过 §6.2 重传。
  3. 全程持有 security scope 到会话结束。

---

## 8. 边界与健壮性

- **safeJoin / 越界防护**：`re-anchor` 的 `safeJoin(base, rel)` 必须拒绝 `rel` 通过 `..`
  逃出 `base`（防止旧数据 / 构造路径越权）。规范化后校验 `isUnder(result, base)`。
- **符号链接 / `/private` 前缀**：iOS Documents 解析可能带 `/private` 前缀差异，
  `normalize` 用 `filepath.EvalSymlinks` + `filepath.Clean`，relativize 与 resolve 用**同一套
  normalize**，避免 `isUnder` 判定抖动。
- **rel 为根**：项目即 `workspaceDir` 本身时 `rel="."`，`safeJoin(base, ".") == base`。
- **大小写**：iOS 文件系统大小写不敏感（默认），mac 可能敏感；relpath 比较保持原样，不强制
  lowercase，仅 Clean。
- **`workspaceDir` 为空（mac）**：跳过相对化，全部按 `external`/绝对处理，等价旧行为。
- **沙盒标志**：现有 `sandboxed=true` 若对工具调用做了 `workspaceDir` 限定，需确认 `external`
  路径是否被允许（与第一轮「runtime confinement」问题联动）；若 external 被 confine 拦截，则
  iOS 实际只支持 `root="workspace"`，host 侧据此关闭外部目录入口。

---

## 9. 迁移（已存在的绝对路径 session）

一次性迁移（启动时执行，幂等）：
```
func migrateLegacyWorkspacePaths(db, currentWorkspaceDir string) {
    for each row where workspace_root IS NULL and workspace_path != "" {
        abs := row.workspace_path   // 旧绝对路径，含旧 <UUID>

        // 尝试「按尾部子路径」抢救：旧路径里 Documents 之后的部分通常仍有效
        if rel, ok := relAfterMarker(abs, "/Documents/"); ok {
            // 用尾部相对段在「当前」workspaceDir 下验证是否存在
            if exists(safeJoin(currentWorkspaceDir, rel)) {
                row.workspace_root = "workspace"
                row.workspace_rel  = rel
                continue
            }
        }
        // 抢救不了 → 标记为 external + needs_rebind，交给 host 重新授权/重选
        row.workspace_root  = "external"
        row.workspace_ext_id = ""        // 空 → UI 提示「workspace 失联，请重新选择」
        row.abs_hint = abs
    }
}
```
要点：
- 迁移**只读尾部相对段**来抢救，不信旧绝对前缀。
- 抢救失败不删数据，降级为 `needs_rebind`，UI 引导用户重新指定。
- mac 上迁移可把 `workspace_path` 原样塞进 `ext_id`（绝对即稳定身份），保持旧行为。

---

## 10. 平台 gating

- 相对化 / re-anchor / 迁移逻辑对两端都跑（统一描述符），差异仅在数据：
  - iOS：`root="workspace"` 占绝大多数，`external` 需 host 重传。
  - mac：`external.ext_id` == 绝对路径，host 无需重传，等价旧行为。
- 不引入 `#if ios` 分叉的存储格式；用「描述符 + external.abs 是否稳定」吸收平台差异。

---

## 11. 测试计划

1. **重装模拟**：创建 `root="workspace"` 会话 → 换一个 `workspaceDir`（模拟新 `<UUID>`）
   重启 runtime → list/attach 得到的绝对路径应指向新 `workspaceDir` 下同一相对路径，可读写。
2. **external 重传**：`root="external"` 会话，不重传 → `ErrExternalNeedsHostRebind`；
   重传新绝对路径 → 正常 attach。
3. **越界防护**：构造 `rel="../../etc"` → `safeJoin` 拒绝。
4. **迁移**：注入旧绝对路径行 → 迁移后尾部段在当前 `workspaceDir` 命中则 `workspace`，
   否则 `external + needs_rebind`，且不丢行。
5. **mac 回归**：`workspaceDir` 为空 / 任意根，绝对路径会话行为与改造前一致。
6. **normalize 一致性**：带 `/private` 前缀、符号链接、尾部 `/` 的路径，relativize→resolve 往返
   得到等价绝对路径。

---

## 12. 交付清单（runtime 侧）

- [ ] DB schema 增 `workspace_root` / `workspace_rel` / `workspace_ext_id`，`workspace_path` 降级 hint。
- [ ] create：绝对 `workspace_path` → relativize（§5.1）；接受可选 `workspace_ext_id`。
- [ ] load/list/detail：re-anchor（§5.2）用本次 `workspaceDir`；返回结构化 `workspace_ref`。
- [ ] `POST /v1/conversations/{id}/rebind`（§6.2bis）：HTTP、Phase 1、WS 前；接受
      `workspace_path` 覆盖运行态绑定，校验存在性，幂等；external 未 rebind 则 Load 报错。
- [ ] detail/list 返回 `workspace_ref` + `needs_rebind`（external 且本次未 rebind → true）。
- [ ] `safeJoin` + 统一 `normalize`（EvalSymlinks+Clean）+ 越界校验。
- [ ] 启动时一次性迁移（§9），幂等。
- [ ] mac 回归：external.ext_id=绝对路径，行为不变。
- [ ] 单测覆盖 §11 全部用例。
```
