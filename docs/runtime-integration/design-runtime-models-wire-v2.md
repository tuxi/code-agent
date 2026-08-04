# /v1/runtime/models Wire 协议演进（connection + profile 两层）

> **状态**: 设计探索阶段（设计文档 `design-connection-flattening.md` §12 开放问题 1 的独立片段）
> **受众**: Runtime 开发 + AgentKit 开发 + Gateway 开发
> **版本**: v0.1
> **日期**: 2026-08-03

---

## 1. 背景

扁平化后，模型目录的**数据来源**从「host 注入的完整 ModelConfig」变为「内建 registry + 用户全局 + 项目 + 注入」的分层。`/v1/runtime/models` 是 host UI 展示模型列表的唯一来源，必须与 runtime 实际行为同源（消除 pricing/context_window 漂移，§13.2 F4.4）。

**关键现状**：当前 DTO **已经是 connection + profile 两层结构**——`Connections` 分组 + `Models` 描述符（`internal/server/runtime_contract.go:37-60`）。所以本片段不是结构重造，而是**语义演进**：数据来源分层化、Available 真实化、能力数据与展示元数据分离、alias 稳定性。

---

## 2. 现状精确结构

```go
// internal/server/runtime_contract.go
type RuntimeModelCatalog struct {
    Schema              string                   // "runtime-model-catalog/v1"
    Revision            int64                    // 持久化于 server state（BuildRuntimeContract:75）
    DefaultRuntimeAlias string
    Connections         []RuntimeModelConnection
}

type RuntimeModelConnection struct {
    ID, ProviderID, DisplayName string
    BillingSource               string   // 现硬编码 "server_managed"
    Models                      []RuntimeModelDescriptor
}

type RuntimeModelDescriptor struct {
    RuntimeAlias, WireModelID, DisplayName string
    ContextWindow     int      // omitempty
    SupportsTools     bool
    SupportsReasoning bool
    InputModalities   []string
    Available         bool     // 现硬编码 true（buildRuntimeModelCatalog:142）
}
```

**构建路径**：`buildRuntimeModelCatalog(cfg)` 遍历 `cfg.ModelNames()`，字段全部来自 host 注入的 `ModelConfig`（`runtime_contract.go:95-168`）。别名格式 `provider.<b64(connectionID)>.model.<b64(wireModelID)>`，`runtimeAliasComponents`（:192-206）按 `provider.<c>.model.<w>` 四段解析，组件 base64.RawURL 编码 + 规范化校验（:208-218）。

**发布路径**：`BuildRuntimeContract`（:65）→ `RuntimeModels` → `GET /v1/runtime/models`（`mux.go:445`）。

---

## 3. 设计决策

1. **保留两层结构**——与扁平化的 Connection/ModelProfile 完全对齐，wire 不重造。
2. **字段来源分层**——能力数据（ContextWindow/定价/能力）归 registry，展示元数据（DisplayName/supports_*）归 host catalog 注入，冲突时**能力数据优先**（§8.3 定案规则）。
3. **Available 语义真实化**——从硬编码 true 改为真实认证状态，对齐 pi 的「加载但未认证不可用」（`packages/coding-agent/docs/models.md:145`）。
4. **alias 格式保持** `provider.<c>.model.<w>`——它已是扁平形态；迁移只处理重命名模型的旧引用（§6）。

---

## 4. 目标 Schema（v2，增量演进）

保持 v1 字段兼容，新增/改语义字段：

```json
{
  "schema": "runtime-model-catalog/v2",
  "revision": 42,
  "default_runtime_alias": "provider.ZGVlcHNlZWs.model.ZGVlcHNlZWstdjQtcHJv",
  "connections": [
    {
      "id": "deepseek",
      "provider_id": "deepseek",
      "display_name": "DeepSeek",
      "billing_source": "server_managed",
      "credential": {
        "status": "configured",          // 新增：configured | missing | none
        "source": "env"                  // 新增：env | injected | keychain | none（非机密声明）
      },
      "models": [
        {
          "runtime_alias": "provider.ZGVlcHNlZWs.model.ZGVlcHNlZWstdjQtcHJv",
          "wire_model_id": "deepseek-v4-pro",
          "display_name": "DeepSeek V4 Pro",
          "context_window": 128000,
          "supports_tools": true,
          "supports_reasoning": false,
          "input_modalities": ["text"],
          "available": true,             // 语义变化：真实认证状态，非硬编码
          "unavailable_reason": null     // 新增：available=false 时的原因（"no_auth" | "unknown_model"）
        }
      ]
    }
  ]
}
```

**字段级变更清单**：

| 字段 | v1 现状 | v2 目标 | 来源 |
|------|--------|--------|------|
| `connection.credential.status` | 无 | `configured`/`missing`/`none` | 两级凭证解析结果（§6 主文档） |
| `connection.credential.source` | 无 | env/injected/keychain/none | connectionsJSON 声明或 registry 约定（非机密） |
| `descriptor.context_window` | host 手写 | **registry**（能力数据） | 分层合并（§8.3，能力数据优先） |
| `descriptor.display_name` | host 手写 | host catalog 注入（展示元数据） | 分层合并（§8.3） |
| `descriptor.available` | 硬编码 true | **真实认证状态** | 两级凭证解析 |
| `descriptor.unavailable_reason` | 无 | no_auth/unknown_model | 新增 |
| `billing_source` | 硬编码 | 保留（gateway 为 server_managed） | — |

**不在 wire 上的**（§13.2 N1.2）：base_url、credential 值、pricing——目录只发布能力与展示，不发布路由与机密。

---

## 5. Available 的三态语义

| status | 含义 | host UI 行为 |
|--------|------|-------------|
| `available: true` | 连接有凭证（或 source=none）且模型在 registry | 可选 |
| `available: false` + `unavailable_reason: "no_auth"` | 模型已列出但连接无凭证 | 显示但置灰（pi 模式，`/model` 不可选） |
| `available: false` + `unavailable_reason: "unknown_model"` | 连接存在但 wire_model 不在 registry | 显示 + 警告（配置错误） |

**判定时机**：构建 catalog 时对每个 connection 走两级凭证解析的「存在性」检查（只查声明/值是否配置，**不执行命令、不触发网络**——对齐 pi 的 `hasConfiguredAuth`，`model-runtime.ts` 的 `Available` 不跑 shell）。

---

## 6. alias 稳定性与迁移（开放问题 8）

**alias 格式不变**：`provider.<b64(connectionID)>.model.<b64(wireModelID)>` 已是扁平形态，connection id + wire model 直编。扁平化本身不改变格式。

**真正的迁移点是重命名**：

| 场景 | 旧 alias 引用 | 处理 |
|------|--------------|------|
| 友好名 `deepseek-pro` → connection `deepseek` + wire `deepseek-v4-pro` | `provider.<b64(deepseek-pro)>.model...` | 旧 session 里保存的 alias 失效 |
| wire model 改名（上游版本号变更） | 旧 wire 名 | 同上 |

**迁移策略**（三选一，待定）：

- (a) **registry 保留旧名别名**——内建 registry 为每个改名模型保留一个「旧名 → 新名」映射条目，旧 alias 继续可解析，标注 deprecated
- (b) **启动时 alias 迁移表**——`BuildRuntimeContract` 读一张迁移映射，把 catalog revision 关联的旧 alias 重定向到新 alias，session 恢复时按表转换
- (c) **session 恢复时重解析**——恢复逻辑把存不进去的旧 alias 走 `resolveTurnModel` 模糊匹配到新模型（现有 `restoreModelFromSession` 已有 fallback 先例）

**验收信号**：已持久化 session 的模型引用可迁移（旧 alias → 新 alias），或给出明确迁移工具（§13.3）。

---

## 7. Revision 与 UI 刷新

- `Revision` 已持久化（`LoadOrCreateServerState(root, fingerprint)`，`BuildRuntimeContract:71-75`）——**fingerprint 会随 registry 内容变化**，扁平化后 registry 进二进制，fingerprint 需纳入 registry 版本。
- host UI 以 `revision` 为刷新依据：revision 变化 → 重拉目录。这与 auth 状态变化的联动：凭证注入/刷新只影响 `available`，不 bump catalog revision——**available 是随请求计算的瞬态，revision 是目录内容的版本**。需区分两个维度，避免 UI 因 token 刷新而全量重拉。

---

## 8. 兼容性（三端调研验证）

三端调研确认（`impact-agentkit-flattening.md`）：AgentKit 对 schema 是**硬校验**（`RuntimeServerCoordinator`/`RuntimeServerPreflight` 要求 `schema == "runtime-model-catalog/v1"`），且 DTO 字段全部 non-optional。兼容规则：

- **增量字段**：v2 只增字段、改语义（available），不改字段名——旧客户端消费 v1 字段仍可用。
- **schema 协商改为前缀匹配**：运行时在桥接期**同时接受** `runtime-model-catalog/v1` 与 v2（或输出双版本语义）。AgentKit 桥接期接受 v1+v2，`unavailable_reason`/credential status 为 optional；旧 SDK 对 v2 runtime 用 v1 语义（available 恒 true）继续工作。
- **v2 新增字段必须 optional**：AgentKit DTO 全字段 non-optional 严格解码——`unavailable_reason`、`connection.credential.status/source` 若为 required，旧 SDK 对新 runtime 直接 decode 失败。必须以 optional 发布，SDK 升级后再收紧。
- **别名格式不变**：现有 session 与缓存引用不因格式而失效（重命名场景除外，见 §6）。

---

## 9. 开放问题

1. **available 的判定成本**：每个 connection 构建时做凭证存在性检查——对注入型（静态值）零成本，对 env/命令型是否可接受？（倾向：只查声明存在性 + 非空，不执行命令，对齐 pi）
2. **billing_source 语义**：gateway 动态 catalog 的 billing 归 host 还是 runtime？`server_managed` 是否足够，还是需要 host 侧 billing 元数据注入？
3. **能力数据生成化后的 catalog revision 联动**：registry 若后续走生成化（上游数据源），fingerprint/revision 如何与上游版本对齐（§12 开放问题 4 的后置影响）。
