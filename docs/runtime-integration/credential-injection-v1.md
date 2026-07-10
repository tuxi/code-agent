# Credential Injection Contract v1

> **Status**: Frozen v1.0 — reviewed & approved by Go Runtime team (2026-07-09).
>
> **Extracted from**:
> - [`runtime-gateway-integration-v1.md`](./runtime-gateway-integration-v1.md) §6 (宿主注入契约)
> - [`design-credential-gateway.md`](./design-credential-gateway.md) §6.3 (AgentKit 注入实现)
> - [`agentkit-credential-account-design-v1.md`](../design/agentkit-credential-account-design-v1.md) §2-6 (Swift 侧数据模型与注入路径)
>
> **Role**: 本文件是 Host (AgentKit / CLI / CI) → Runtime 的单向 credential 注入协议。
> 它不定义 credential 如何存储、如何刷新、如何登录 —— 这些属于 Host 层。

---

## 1. Scope

本契约定义：

| 定义 | 不定义 |
|------|--------|
| ✅ Host 如何把 credential 传给 Runtime | ❌ Token 刷新逻辑 |
| ✅ `secretsJSON` 的精确 schema | ❌ 登录/注册/OAuth 流程 |
| ✅ Key 编码规则 | ❌ Keychain / 文件存储格式 |
| ✅ `Reconfigure` 的调用语义 | ❌ `credential.Resolver` 接口（见 `design-credential-gateway.md`） |
| ✅ `auth_expired` 事件格式 | ❌ HTTP 401 的重试策略（见 `runtime-gateway-integration-v1.md` §4） |
| ✅ 安全约束（metadata 剥离） | ❌ Account / Subscription 管理 |

---

## 2. Injection Flow

```
┌─────────────────────────────────────────────────────┐
│  Host (AgentKit / CLI / CI)                          │
│                                                     │
│  CredentialStore / Env / File                       │
│       │                                             │
│       ▼                                             │
│  CredentialMap.toSecretsJSON()                      │
│       │                                             │
│       │ 单向注入（不回调）                              │
│       ▼                                             │
│  Runtime.Start(secretsJSON)                         │
│  Runtime.Reconfigure(secretsJSON)                   │
└─────────────────────┬───────────────────────────────┘
                      │
        ┌─────────────▼───────────────────────────────┐
        │  Code-Agent Runtime                         │
        │                                             │
        │  injectSecrets(cfg, secretsJSON)             │
        │       │                                     │
        │       ▼                                     │
        │  StaticResolver (Go)                        │
        │       │                                     │
        │       ▼                                     │
        │  credential.Resolver.Resolve(ctx, target)   │
        └─────────────────────────────────────────────┘
```

**核心约束**：注入是单向的。Runtime 不回调 Host 索取 credential。Host 通过 `Reconfigure` 推送新 credential。

如果让 Runtime 通过 gomobile callback 向 AgentKit 要 credential：
- Runtime 生命周期依赖 UI 线程（iOS 后台 UI 可能没在跑）
- CLI 无法独立运行（没有 callback 注册）
- 开源 core 变成客户端插件

---

## 3. `secretsJSON` Schema

### 3.1 格式

```json
{
  "<encoded_target_key>": {
    "type": "bearer",
    "secret": "<credential_value>",
    "expires_at": 1750432000
  }
}
```

### 3.2 顶层 Key

顶层 key 是 credential target 的稳定编码字符串。

格式：`{namespace}/{name}`

其中 namespace 和 name 各自经 `url.PathEscape` 编码。

**Swift 侧实现**：
```swift
// CredentialTarget.id
public var id: String {
    "\(namespace.addingPercentEncoding(withAllowedCharacters: .urlPathAllowed) ?? namespace)/\(name.addingPercentEncoding(withAllowedCharacters: .urlPathAllowed) ?? name)"
}
```

**Go 侧实现**：
```go
// Target.String()
func (t Target) String() string {
    return url.PathEscape(t.Namespace) + "/" + url.PathEscape(t.Name)
}
```

**两端必须保持完全一致的编码逻辑。**

### 3.3 示例

```json
{
  "gateway%2Fdefault": {
    "type": "bearer",
    "secret": "eyJhbGciOiJSUzI1NiIs...",
    "expires_at": 1750432000
  },
  "llm%2Fdeepseek": {
    "type": "bearer",
    "secret": "sk-abc123..."
  },
  "mcp%2Fgithub": {
    "type": "bearer",
    "secret": "gho_xyz789...",
    "expires_at": 1750086400
  }
}
```

> 注意：`llm/deepseek` 和 `mcp/github` 中的 `/` 无需编码（只编码 namespace 和 name 内部可能含有的特殊字符，如 `github.enterprise.com%2Forg%2Fproject`）。

### 3.4 字段说明

| 字段 | 类型 | 必需 | 说明 |
|------|------|:---:|------|
| `type` | string | ✅ | `"bearer"` — `Authorization: Bearer <secret>`（JWT / API key / OAuth token 统一走此类型）。`"none"` — 显式清空此 target 的 credential。注意：`"secret"` 是 Go 内部的 `CredentialType`（预留给 AWS SigV4、mTLS 等非 Bearer 机制），**不在 `secretsJSON` 中使用** |
| `secret` | string | ✅ | credential 机密值。不可展示、不可日志输出 |
| `expires_at` | number | ❌ | Unix 秒时间戳（int64，UTC）。`null` 或缺失 = 永不过期（如静态 API key） |

**为什么没有 `metadata` 字段？**
Host 侧的 `Credential` 对象包含 `metadata`（如 `refresh_token`），但在注入 Runtime 时**必须剥离**。Runtime 不需要也不应该知道 `refresh_token`。

**Host 侧注入前剥离 metadata：**
```swift
// Swift
public func strippedForInjection() -> Credential {
    Credential(kind: kind, secret: secret, expiresAt: expiresAt, metadata: [:])
}
```

```go
// Go — credential.StaticResolver 只读 Credential 结构体，不含 Metadata
// Metadata 在构造 StaticResolver 之前已由 Host 剥离
```

### 3.5 版本共存

旧格式（环境变量 map）与新格式（credential map）**永久共存**，无废弃计划：

**旧格式**（继续支持）：
```json
{"DEEPSEEK_API_KEY": "sk-xxx"}
```

**新格式**（推荐）：
```json
{"llm%2Fdeepseek": {"type": "bearer", "secret": "sk-xxx"}}
```

Runtime 的 `injectSecrets` 通过以下规则自动区分：
- key 包含 `/` 或 `%2F` → 新格式（credential target）
- key 为大写 `_` 分隔 → 旧格式（env var name）

---

## 4. `Reconfigure` Semantics

### 4.1 Signature

```go
func (s *Server) Reconfigure(secretsJSON string, modelName string) error
```

Host 侧调用：
```swift
// AgentKit — Swift
let secrets = credentialStore.all().toSecretsJSON()
try server.reconfigure(secrets, modelName: "")
```

### 4.2 Preconditions

- Runtime 必须先经 `Start()` 初始化
- `secretsJSON` 格式必须符合 §3 的 schema
- `modelName` 为空字符串时保留当前模型

### 4.3 Idempotency

相同的 `secretsJSON` 多次调用为 no-op。Runtime 内部比较 credential map 是否实质变更，无变更则跳过。

### 4.4 Concurrency

`Reconfigure` 可被多个线程/goroutine 并发调用。实现保证 **last successful update wins**。
中途被新的 `Reconfigure` 覆盖时，被覆盖的更新无副作用。

### 4.5 Effect Timing

- 调用 `Reconfigure` 时若有 turn 正在执行 → defer 到下一 step 边界生效
- 调用 `Reconfigure` 时若 session 为 `paused` → 立即生效
- 不换端口、不重建 server、不中断现有 WebSocket 连接

### 4.6 Atomicity

Reconfigure 采用 **copy-on-stack** 模式，三阶段原子性：

1. `secretsJSON` parse 失败 → 返回 error，`h.cfg` 不变
2. model select 失败 → 返回 error，旧配置不变
3. `BuildProvider` 失败 → 返回 error，旧 provider 不变

**任一阶段失败，旧 credential 完整保留。** 这是 `embed/server.go:198-220` 已验证的行为。

### 4.7 空 `secretsJSON` 语义

`"{}"` = **无变更（no-op）**。空 map 不做任何事。

清空所有 credential（如用户登出）应**显式**发送：
```json
{"gateway%2Fdefault": {"type": "none"}}
```

### 4.8 触发场景

| 触发事件 | Host 行为 |
|----------|----------|
| 用户登录 | `AccountManager.login()` → 写入 Keychain → `Reconfigure(secretsJSON)` |
| Token 刷新 | `AccountManager.gatewayCredential()` lazy refresh → 写入 Keychain → `Reconfigure(secretsJSON)` |
| 用户切换 BYOK key | `CredentialSettingsStore.saveBYOKKey()` → 写入 Keychain → `Reconfigure(secretsJSON)` |
| 用户登出 | `AccountManager.logout()` → 删除 Keychain → `Reconfigure("{}")` |

---

## 5. `auth_expired` 事件

### 5.1 事件格式

当 Runtime 因 Gateway 返回 HTTP 401 导致 turn 失败时，通过 WebSocket 事件流通知 Host：

```json
{
  "kind": "turn_failed",
  "session_id": "sess_abc",
  "turn_id": "turn_42",
  "at": "2026-07-09T10:30:00.000Z",
  "error": {
    "code": "auth_expired",
    "message": "Gateway returned 401 — access token may be expired"
  }
}
```

### 5.2 Host 响应流程

```
Host 收到 turn_failed (code: auth_expired)
    │
    ├─ ① AccountManager.gatewayCredential()
    │     → 检测 token 过期 → 触发 lazy refresh
    │     → POST /api/v1/auth/refresh
    │     → 获取新 access_token
    │     → Keychain.set(newCredential)
    │
    ├─ ② Runtime.Reconfigure(newSecretsJSON)
    │
    └─ ③ UI 提示用户「Token 已刷新，请重试」
       或自动重发上一条消息（由 Host 的 UX 策略决定）
```

### 5.3 Server 模式（HTTP API）

当 Runtime 以 server 模式运行（`codeagent serve`），`auth_expired` 通过 HTTP 响应返回：

```json
{
  "error": {
    "code": "auth_expired",
    "status": 401
  }
}
```

### 5.4 CLI 模式（stderr）

```
Error: Gateway authentication failed (HTTP 401).
Your access token may have expired. Run:
  codeagent login
```

---

## 6. Security Constraints

### 6.1 Runtime 不可接触的数据

| 数据 | 位置 | Runtime 可见？ |
|------|------|:---:|
| `secret` (token/key) | `secretsJSON` | ✅ 仅用于构造 HTTP Authorization header |
| `expires_at` | `secretsJSON` | ✅ 仅 `CachedResolver` 用于缓存失效判断 |
| `refresh_token` | Host Keychain/文件 | ❌ Runtime 永不接收 |
| `user_id` / `email` | Host Keychain/内存 | ❌ Runtime 不感知 |
| `subscription_tier` | Host 内存 + Gateway JWT claims | ❌ Runtime 不解析 JWT |

### 6.2 日志安全

- `Credential.secret` 不得出现在任何日志输出中
- Runtime 的 `ResolvedCredential.Source` 字段（`"agentkit:gateway"`）可安全日志，不包含机密
- 调试模式下若需输出 `secretsJSON`，必须遮蔽 `secret` 字段

### 6.3 传输安全

- iOS：`secretsJSON` 通过 `MobileStart()` 的 C string 参数传入，同进程内传输
- macOS：`Authorization: Bearer <jwt>` 通过 `http://localhost` 发送，同机内传输；未来远端部署需 HTTPS
- CLI：环境变量 `DEEPSEEK_API_KEY` 在进程环境中；文件 `~/.codeagent/credentials` 权限应为 `0600`

---

## 7. Contract Versioning

本契约遵循 [agent-wire-v1.md](../protocols/agent-wire-v1.md) §5 的版本规则：

- 同一 major 内**只增不改**：可新增可选字段、新增 target namespace
- Host 必须忽略 Runtime 返回的未知字段
- Runtime 必须忽略 Host 传入的未知字段
- `secretsJSON` 的顶层 key 格式变更属于 breaking change → 升 major

---

## 8. Review Resolutions (Go Runtime, 2026-07-09)

所有 Draft 阶段标注的 `[REVIEW]` 点已 resolve，摘要如下。详细讨论见 Go Runtime review。

| # | 疑点 | 决议 |
|---|------|------|
| 1 | `type` 字段取值范围 | `"bearer"` 和 `"none"` 在 secretsJSON 中使用。`"secret"` 是 Go 内部 `CredentialType`（预留给 AWS SigV4、mTLS 等非 Bearer 机制），不在 secretsJSON 注入通道中使用 |
| 2 | 旧格式废弃时间线 | **无废弃计划，永久共存**。`injectSecrets` 的检测逻辑维护成本为零，CLI 用户不应被迫改变配置格式 |
| 3 | `expires_at` 的时区语义 | `int64`，Unix 秒，UTC。JSON number 经 `ParseInt(s, 10, 64)` 解析。秒级精度对 token 过期已足够 |
| 4 | Reconfigure 错误处理 | copy-on-stack 模式保证原子性：parse 失败/model select 失败/BuildProvider 失败 → 返回 error，旧状态完整保留 |
| 5 | 空 `secretsJSON` 语义 | `"{}"` = no-op。清空 credential 需显式 `{"gateway%2Fdefault": {"type": "none"}}` |

---

## Appendix A: Quick Reference for Host Implementors

### AgentKit (Swift)

```swift
// 构造 secretsJSON
let map = try await credentialStore.all()
let secretsJSON = map.toSecretsJSON()
// → {"gateway%2Fdefault":{"type":"bearer","secret":"eyJ...","expires_at":1750432000}}

// 启动时注入
try runtime.launch(with: credentialStore)

// Token 刷新后热切换
let newSecrets = try await credentialStore.all().toSecretsJSON()
try runtime.reconfigure(secretsJSON: newSecrets, modelName: "")
```

### CLI (Go — future)

```go
chain := &credential.ChainResolver{
    Resolvers: []credential.Resolver{
        credential.EnvResolver{},                          // DEEPSEEK_API_KEY
        credential.NewFileResolver("~/.codeagent/credentials"),
    },
}
```

### CI (env vars)

```bash
export DEEPSEEK_API_KEY=sk-xxx
codeagent run
# EnvResolver 自动读取
```
