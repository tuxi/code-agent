# P0-C1：Reviewer 证据的分级获取（最小设计）

2026-10-30。状态：**设计，未实现**。只扩展 `codeagent_get_artifacts` 的入参与返回，不新增工具、不新增调度器、不新增 Agent 类型、不修改 TimelineKit。

## 问题

实测数据：Case 001 的 `codeagent_get_artifacts` 返回 **92,518 字符**，超过 WorkBuddy 单次工具结果上限，被落盘，客户端无法一次消费。

组成：`diff` 71,657 B（8 个文件，含 3 个二进制段）+ `command_results` 9 条（含 stdout）+ `model_calls` 15 条。而 `get_handoff` 已经把同样的 9 条命令以紧凑形式（命令 + 退出码 + 证据引用）给过一次。

两个具体缺陷：

1. 没有粒度：要么全拿，要么拿不到。
2. 重复：命令证据在 handoff 与 artifacts 里各出现一次，后者还带完整 stdout。

## 设计

### 1. 入参扩展（同一个工具，不新增工具）

```go
type ArtifactInput struct {
    SessionID string `json:"session_id"`
    TurnID    string `json:"turn_id"`
    Diff      string `json:"diff,omitempty"`      // summary | stat | full，默认按 profile
    Path      string `json:"path,omitempty"`      // 只取单个文件的 patch
    Offset    int    `json:"offset,omitempty"`    // patch 的字节偏移
    Limit     int    `json:"limit,omitempty"`     // 单次最多返回字节，服务端再夹一层硬上限
    Commands  *bool  `json:"include_commands,omitempty"`
    ExpectSHA string `json:"expect_diff_sha256,omitempty"` // 版本守卫
}
```

`Path` 与 `Offset` 组合即按文件分页；不传 `Path` 时按 `Diff` 模式返回整体摘要。

### 2. 默认值按 profile 区分（这是兼容的关键）

| profile | `diff` 默认 | `include_commands` 默认 | 理由 |
| --- | --- | --- | --- |
| executor | `full` | `true` | 保持既有调用行为完全不变 |
| reviewer | `summary` | `false` | 新面没有历史调用方，直接给可用的小结果 |

`full` 仍然可用，只是不再是 reviewer 的默认。这样 executor 侧零改动，reviewer 侧默认不再产生 92 KB。

### 3. 返回结构

`summary` 模式不返回 `diff` 正文，改为：

```json
{
  "baseline_commit": "34e59c4e…",
  "delivery_commit": "8def8a27…",
  "diff_sha256": "21bb4e94…",
  "evidence_source": "commit_range",
  "evidence_complete": true,
  "files": [
    {"path": "Sources/…/ThumbnailProvider.swift", "status": "modified",
     "additions": 96, "deletions": 14, "binary": false,
     "patch_bytes": 12781, "patch_sha256": "…"}
  ],
  "total_additions": 0, "total_deletions": 0,
  "command_count": 9,
  "diff_truncated": false,
  "note": "per-file patches available via path/offset"
}
```

单文件模式返回该文件的 `patch` 分片：

```json
{"path": "…", "patch": "…", "offset": 0, "next_offset": 16384,
 "patch_bytes": 12781, "patch_sha256": "…", "complete": false}
```

`next_offset=0` 表示该文件已取完。二进制文件恒为 `binary=true, patch_bytes=0, patch=""`，不内联。

### 4. 体积约束

- 单次响应硬上限 32 KiB（可调常量），超出即切片，不报错。
- `Limit` 只能调小，不能突破硬上限。
- summary 模式目标 < 8 KiB；按 Case 001 实测应约 1.5 KiB。

### 5. 版本守卫

请求可带 `expect_diff_sha256`。服务端比对当前绑定的 `diff_sha256`，不一致直接返回 `evidence_mismatch`，不做部分返回。

作用：防止调用方拿 A 版本的文件列表去取 B 版本的 patch。因为每个分片响应都带 `patch_sha256` 与顶层 `diff_sha256`，逐文件拼接后可与 `diff_sha256` 对账。

`evidence_source`、`baseline_commit`、`delivery_commit`、`head_moved`、`worktree_dirty` 在所有模式下都返回，不允许因为"摘要"就丢掉绑定信息。

### 6. 命令证据去重

- reviewer 默认 `include_commands=false`，命令走 `get_handoff`（已有 9 条紧凑记录）。
- 显式 `include_commands=true` 时只返回 handoff 里没有的部分：按 `call_id` 关联的 `stdout`/`stderr`，单条超 4 KiB 截断并标 `output_truncated`。
- 不改变 `handoff.commands` 的结构与语义。

### 7. 不改变的东西

- 证据包 schema 与校验规则：`bundle.go` 不动，历史 bundle 继续可读。
- 历史 bundle 内容不重写，`PROVENANCE.txt` 的派生关系不变。
- 工具数量：reviewer 仍是 7 个。
- 写边界：`requireWritable()` 不变。

## 回归用例（用 92 KB 实测场景）

| 用例 | 断言 |
| --- | --- |
| `TestArtifactsSummaryFitsBudget` | 真实 Case 001 证据的 summary 响应 < 8 KiB |
| `TestArtifactsPerFilePagination` | 逐文件按 `next_offset` 取完，拼接后 `patch_sha256` 与 summary 一致，累计内容 sha256 == `diff_sha256` |
| `TestArtifactsChunkCap` | 单次响应 ≤ 32 KiB，`Limit` 调大也不能突破 |
| `TestArtifactsEvidenceMismatch` | `expect_diff_sha256` 不符时返回 `evidence_mismatch`，不返回任何 patch |
| `TestArtifactsBinaryNotInlined` | 3 个 PNG 返回 `binary=true` 且 `patch` 为空 |
| `TestArtifactsCommandsDeduped` | reviewer 默认不含命令正文；`include_commands=true` 时只返回 stdout，且带 `call_id` |
| `TestArtifactsExecutorDefaultUnchanged` | executor profile 默认仍是 `diff=full` + commands，输出与改动前逐字节一致 |
| `TestBundleSchemaUnchanged` | 旧 bundle 仍能打开，`OpenBundle` 校验规则未变 |

第 7 条是硬约束：executor 输出必须逐字节相同，否则不算兼容。

## 明确不做

- 不新增 `codeagent_get_diff` 之类的工具。
- 不做服务端缓存、不做增量订阅、不做调度。
- 不引入新的 Agent 类型或角色。
- 不修改 TimelineKit、不重跑它的任务。
- 不接入豆包，等本设计落地后再评估。

## 影响面

预计改动集中在 `internal/mcpbridge/artifacts.go`（摘要与切片）、`mcp.go`/`reviewer_tools.go`（入参 schema 与 profile 默认）、`bundle.go`（仅把已存 diff 按文件切分，不改 schema），加测试。无 Runtime、无 Session、无 Flux 改动。
