# Host 注入 Connection 定义的通道（AgentKit）

> **状态**: 设计探索阶段（设计文档 `design-connection-flattening.md` §12 开放问题 7 的独立片段）
> **受众**: Runtime 开发 + AgentKit 开发
> **版本**: v0.1
> **日期**: 2026-08-03

---

## 1. 问题

扁平化后，连接定义（base_url、api 类型、凭证来源声明）进入 runtime 拥有的层（内建 registry + `~/.codeagent/config.yaml`）。但 **AgentKit 宿主（iOS/macOS）的设置页是连接的实际 owner**：

- iOS 沙箱没有 host 可写的 `~/.codeagent/config.yaml`
- host 设置页管理「连接列表」（用户添加 DeepSeek、配置 gateway……）
- 若 host 无法把连接定义传给 runtime，host 就失去对自己设置页的控制，UI 与 runtime 看到的连接列表会漂移

现状 `Options{ConfigYAML, ModelName, Secrets}` 中，ConfigYAML 是完整配置副本——扁平化的目标正是取消它。需要一条**协议级通道**，让 host 在运行时注入 connection **定义**（不是凭证**值**）。

---

## 2. 现状基础

**注入契约**（`credential-injection-v1.md`，Frozen v1.0）：

- 单向注入，Runtime 不回调 host 索取凭证（gomobile 不能 bridge map、CLI 无 callback、开源 core 不依赖客户端）
- `secretsJSON` 只承载凭证**值**：`{"<encoded_target>": {"type":"bearer","secret":"...","expires_at":...}}`，target 是 `{namespace}/{name}` 编码（`url.PathEscape`，两端编码必须一致）
- `Reconfigure(secretsJSON, modelName)` 在下一个 turn 边界热换模型与凭证（`internal/embed/server.go:505-564`）；结构性变更（provider kind、MCP 列表）仍需重启

**解析链**（`internal/embed/server.go`）：

- `parseSecretsJSON`（:909）— JSON 字符串 → map，兼容旧格式（纯字符串值）与新格式（对象值存 raw JSON）
- `injectSecrets`（:937）— 新格式 target 键 → `StaticResolver`，旧格式 env 名/模型友好名 → 回填 `ModelConfig.APIKey`（back-compat）
- `parseTargetKey`（:991）— 解 `{namespace}/{name}` 编码

---

## 3. 设计决策：定义与值分离，双通道

**连接定义（非机密）与凭证值（机密）走两条通道**：

```
通道 A: connectionsJSON  ← 连接定义（base_url、api 类型、凭证来源声明）— 非机密，可进日志/缓存
通道 B: secretsJSON      ← 凭证值（bearer secret）— 机密，沿用 frozen v1 契约，仅键从
                          {namespace}/{name} 扁平化为 connection id
```

**理由**：

1. **secretsJSON 是 frozen 契约**（v1 已评审冻结），它的 scope 明确是凭证值（`credential-injection-v1.md` §1）。塞入定义会破坏契约边界，且定义进 secrets 通道意味着定义不可审计、不可缓存。
2. **定义与值的生命周期不同**：定义随设置页编辑变化（低频），值随 token 过期刷新（高频，auth_expired）。分开后 Reconfigure 刷新值不必重传定义。
3. **对称于扁平化的概念模型**：Connection.Credential 是「声明」（source），注入的是「值」（secret）——定义通道传声明，值通道传值。

不采用「扩展 secretsJSON 携带定义」（混淆两类数据）、不采用「extension 机制承载」（extension 是代码级 provider 注册，host 设置页是数据级连接管理，两者不同职责——§8.6 的 extension 服务于项目级自定义 provider，host 注入服务于运行时连接管理）。

---

## 4. connectionsJSON Schema

```json
{
  "connections": {
    "<connection_id>": {
      "api": "openai" | "ollama",
      "base_url": "https://...",
      "credential": {
        "source": "jwt" | "keychain" | "env" | "none",
        "ref": "deepseek.key",
        "env": "DEEPSEEK_API_KEY"
      }
    }
  }
}
```

**字段**：

| 字段 | 必需 | 说明 |
|------|:---:|------|
| `api` | 否 | 默认 "openai"（openai-compatible）。与内建 registry 冲突时 host 值覆盖（§8.3 后层覆盖前层） |
| `base_url` | 否 | 内建 registry 已提供则可不传；自定义端点必传 |
| `credential.source` | 否 | 凭证来源声明：`jwt`（gateway）、`keychain`（host 引用 Keychain 条目）、`env`（环境变量）、`none`（本地） |
| `credential.ref` / `env` | 条件 | source 为 keychain 时 ref 指向 host Keychain 条目；source 为 env 时 env 指向环境变量名 |

**三端示例**：

```json
{
  "connections": {
    "gateway": {
      "api": "openai",
      "base_url": "https://agent.xxx.com/api/v1/agent",
      "credential": {"source": "jwt"}
    },
    "deepseek": {
      "credential": {"source": "keychain", "ref": "deepseek.key"}
    },
    "ollama": {
      "base_url": "http://localhost:11434/v1",
      "credential": {"source": "none"}
    }
  }
}
```

**编码**：connection id 复用 `Target.String()` 的 `url.PathEscape` 规则（仅 id 含特殊字符时编码），与 secretsJSON 键一致。

---

## 5. 与 secretsJSON 的协调

- connectionsJSON 的每个连接声明 `credential.source`；secretsJSON 按同一 connection id 提供值。
- **对账规则**（runtime 构建有效连接时）：
  - 连接声明 `source: jwt` 或 `keychain`，secretsJSON 有同 id 值 → 有效，用注入值
  - 连接声明 `source: env` → 走 env 约定（`TOUPPER(id)_API_KEY`）或 `env` 字段指定变量
  - 连接声明 `source: none` → 无需凭证（Ollama）
  - secretsJSON 有值但 connectionsJSON 无此连接 → 值保留（兼容期），或按 §8.3 视为对 registry 内置连接的注入覆盖
- **合并**：注入层（层级 4）覆盖所有静态层——同 id 整条覆盖，不同 id 追加（§8.3 定案规则）。

---

## 6. Reconfigure 语义

```
现状:  Reconfigure(secretsJSON, modelName)
目标:  Reconfigure(connectionsJSON, secretsJSON, modelName)
      — 三个参数均可传 "" 表示「保持当前」
      — 连接增删改 + 凭证刷新 + 模型切换全部热生效（下一 turn 边界）
      — 结构性变更不再需要重启：provider 构建已按连接解耦（§10 步 4 的
        BuildProvider(conn, timeout)），连接图变更不再触碰 MCP 工具图（§11）
```

**约束保留**：MCP 服务器列表仍由 `.mcp.json` 分层管理（§11），不通过 connectionsJSON 变更——host 如需动态 MCP 走既有 `.mcp.json` 注入路径（`Options.MCPJSON`）。

**Options 扩展**：`Options.ConnectionsJSON string`，与 `ConfigYAML`/`Secrets` 并列；`ConfigYAML` 保留为桥接期 back-compat（见 §8）。

---

## 7. 安全约束

- connectionsJSON **只含非机密定义**——base_url、api 类型、凭证来源声明（source/ref/env 名）。**绝不携带 secret 值**。
- secretsJSON 继续遵守 v1 安全约束（metadata 剥离、不落盘、不进入 `/v1/runtime/models` 输出）。
- `ref` 指向 host Keychain 条目名——host 侧解析后经 secretsJSON 传值，runtime 只见值不见 Keychain 结构。

---

## 8. 桥接期与迁移（对齐 flattening §8.7）

| 阶段 | host 行为 | runtime 行为 |
|------|----------|-------------|
| 桥接期 | 仍传 ConfigYAML（models+credentials）+ 旧格式 secretsJSON | 兼容解析：`llm%2Fdeepseek` → connection `deepseek`；ConfigYAML 的 models 读取 + 警告（§8.6 阶段 2）；secretsJSON **三形态 key 双读**（扁平 / `{namespace}/{name}` / 遗留 env 名，flattening §8.7） |
| 切换 | 传 connectionsJSON + 扁平 secretsJSON，ConfigYAML 仅 server/mcp | connectionsJSON 为连接定义唯一注入源（AgentKit 场景唯一事实源，§8.4.1） |
| 删除 | 停传 ConfigYAML | models 读取逻辑删除（§8.6 阶段 3） |

**迁移触发**：此通道形态已定案（flattening §8.4.1 / §12 项 7），AgentKit 的 F4.1/F4.2（secretsJSON 键扁平化、host 注入通道）据此排期。桥接期契约（双 key 读取、schema 前缀匹配、alias 迁移）见 flattening 主文档 §8.7。

---

## 9. 开放问题

1. **连接删除语义**：host 从 connectionsJSON 移除某连接，runtime 如何处理正在使用它的会话？（立即失效 vs 下一 turn 拒绝 vs 允许跑完）
2. **api 类型覆盖的校验**：host 声明 `api: "ollama"` 但 registry 判定为 openai-compatible——以谁为准？（§8.3 倾向后层覆盖，但需明确校验失败时的错误面）
3. **connectionsJSON 与 MCPJSON 的边界**：host 是否可经此通道动态增删 MCP？（§11 倾向否，但需确认宿主现有动态 MCP 需求）
