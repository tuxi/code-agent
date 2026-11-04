# Manual V1：显式重连修复

日期：2026-11-04。离线候选，尚未加载到当前桌面 MCP。无数据库迁移、无 Daemon 更新、无模型调用；不改变 Agent Loop、模型参数或 Managed 准入。

## 根因与最小修改

ManualService 的首次有效探测固定授权代次。普通读取拒绝跟随替换授权，但原先只有 `bind_session(replace_binding_id=...)` 能发布新代次；尚无 Session 的连接缺少显式重连入口。因此 Talkify 重启并重新授权后，用户被迫重载整个 MCP 进程。

扩展现有 `codeagent_connection_info` 的可选 `reconnect` 参数，不新增工具。旧 `{}` 和 `{binding_id}` 调用保持兼容；旧 executor/reviewer 接口不变。示例中的三个值必须来自原来核实的连接，不可从错误信息推测或换成新目标：

```json
{
  "reconnect": {
    "server_id": "原 Runtime ID",
    "store_id": "原 Store ID",
    "workspace_path": "/原项目规范化工作区"
  }
}
```

仅在用户明确恢复本机授权后调用。Bridge 必须已存在原身份锚点；没有锚点时先普通查询，不以 reconnect 选择另一个服务。重连不接受 Token、端口、任意 grant 路径或模型参数，也不更新授权文件。

验证工作区/Git 文件身份、原身份锚点、私有授权有效性和本机目标身份，再通过现有 HTTP 核对实际 Runtime/Store。最后重新检查授权代次、工作区和取消状态，全部通过才更新服务锚点。授权代次在验证中再次变化或请求取消，不发布新锚点。

成功返回 `reconnected=true`、`execution_bound=false`。若旧 Session 绑定仍使用旧授权，返回 `bindings_require_revalidation=true`；它们继续拒绝访问，需调用原 `bind_session` 并提供 `replace_binding_id`，得到新句柄后才能读取原 Turn。

重连只更新进程内服务锚点。它不创建 Session、不提交或重发 Turn、不批准/恢复审批、不改基线，不删除任何未知提交或创建记录。Session 替换沿用现有未确认提交合并逻辑。工作区 HEAD 可以因交付改变，但旧 Session 的基线不会随之更新。

重连与 create/bind/send/cancel 的准入操作串行；已有读取保留原 Client 和原 binding_id。普通查询没有 reconnect 时仍拒绝采用新授权。工具目录保持 11 项，不依赖动态 tools/list。

## 离线验证范围

- 同一个 MCP SDK 连接：普通查询 → 替换授权 → 结构化 grant_replaced → 显式 reconnect → 普通查询恢复；工具目录不变，重复 reconnect 无副作用。
- 不同调用身份、manifest 身份、工作区、缺失参数、过期/缺失授权、目录替换：拒绝且无 Runtime 请求。
- Runtime 实际身份不符、验证期间再次更换授权、请求取消：失败不改变锚点。
- 未确认 Send：重连后旧绑定仍不可用；显式重新绑定后仍保留待审批与 admission_unknown 阻断，无重复输入。
- 新端口、新实例但原 Runtime/Store：允许显式重连；create_unknown 记录仍阻止再次创建。
- Bridge/CLI 完整离线 race 回归及 vet/build；跨版本 opt-in 测试未提供额外二进制时明确 skip，不视为本轮已重跑。

实际计数、候选源码与二进制 hash 见 `build/manual-v1d-reconnect-nf8_odrv/candidate.json`；限定补丁为同目录 `reconnect.diff`。候选由冻结的 C3 源码、V1-d 测试替身修正和本次 5 个 Go 文件构建，不包含其他工作区 WIP。

独立快照中 `go test -race ./internal/mcpbridge ./cmd/codeagent-mcp -count=1 -timeout=180s`：84 项顶层通过、9 项明确跳过、0 失败；两包均通过。新增 5 项重连顶层测试及其子用例合计 17 个通过记录。跳过项为环境 opt-in/子进程 helper，包含未在本轮重新提供四个二进制的跨版本测试；没有将其计为通过。相同快照 `go vet` 和候选 CLI 构建成功。未运行全仓库测试。

## 加载与桌面验收

项目配置迁移需用户明确批准；构建不代表加载。保留现有 command 的二进制和配置私有备份，仅将本项目 command 切换到候选路径，其他设置、required=false、manifest 路径和测试工作区保持不变。不覆盖正在运行的 Bridge，不更新 Talkify/Daemon。

首次升级 schema 需要客户端重载 MCP 一次；若客户端没有单项重载入口，重启 Codex 是加载新版的替代步骤。之后 Talkify 重新授权应在同一 MCP 连接中通过上述参数恢复，无需每次重启 Codex。这个后续桌面场景仍须实际验收，不以离线测试替代。

回滚仅恢复备份的项目 command 并重载 MCP，保持 required=false；不恢复过期凭据、不删除 Store 或任务历史、不重放输入。旧版不支持 reconnect 参数，恢复原来的显式绑定/重载路径。
