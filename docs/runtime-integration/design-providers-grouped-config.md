# Providers 配置模型：服务商分组 + 模型数组 + HTTP 管理

> **状态**: 设计定案（待评审）
> **受众**: Runtime 开发 + AgentKit 开发 + chater 开发 + 架构评审
> **版本**: v1.0
> **日期**: 2026-08-05
> **前置**: design-provider-id-model.md（provider=服务商 id）、design-config-settings-merge.md（settings.json 单一配置源）、design-connection-flattening.md（connection 扁平化）

---

## 1. 问题

### 1.1 加模型的重复成本

当前 settings.json 的 `models` 是**扁平 map**（friendly name → 完整配置）。给 qwen 加一个模型要写：

```json
"qwen3.7-max": {
  "base_url": "https://dashscope.aliyuncs.com/compatible-mode/v1",  // 重复
  "provider": "openai",                                             // 重复
  "credential": {"namespace": "llm", "name": "qwen"},               // 重复
  "context_window": 128000,                                          // 重复
  "model": "qwen3.7-max",                                            // 唯一差异
  "input_price_per_million": 2.0,                                    // 重复
  "output_price_per_million": 8.0                                    // 重复
}
```

服务商公共属性在每个模型条目里全量重复。registry 只救了内置默认模型，**用户追加的模型还是全量重复**。

### 1.2 模型身份跨服务商冲突

`deepseek-v4-flash` 在 DeepSeek 官方、阿里 DashScope、OpenRouter 上**都存在**。扁平 friendly name 是唯一 key，同名模型跨服务商直接冲突——用户无法同时配置「官方 deepseek-v4-flash」和「openrouter 的 deepseek-v4-flash」。

### 1.3 客户端配置各存各的

chater/AgentKit 的 `ProviderConnectionRegistry`（UserDefaults 本地存储）与 runtime 的 settings.json **是两份数据**——客户端改服务商 = 本地改 + 重启注入，HTTP 面完全只读，无配置 CRUD。

---

## 2. 目标

1. **服务商分组 + 模型数组**是一等配置概念——加模型只写 id（对齐 pi 的 models.json）
2. **模型身份 = (provider_id, model_id) 二元组**——同名模型跨服务商可共存（alias 已编码 `provider.<b64>.model.<b64>`）
3. **Runtime HTTP 配置管理**——客户端通过 HTTP 查询/修改服务商，不再各存各的（对齐 AgentKit 的 `ProviderConnection` 概念：一个连接 = 一个服务商实例 + 模型数组）

---

## 3. 服务商分组（磁盘格式）

### 3.1 settings.json 新增 `providers` 段

```json
{
  "providers": {
    "dashscope": {
      "base_url": "https://dashscope.aliyuncs.com/compatible-mode/v1",
      "api": "openai",
      "credential": {"namespace": "llm", "name": "dashscope"},
      "headers": {},
      "models": [
        {"id": "qwen3-coder-plus"},
        {"id": "qwen3.7-max", "context_window": 256000},
        {"id": "qwen3.8-max", "input_price_per_million": 2.0}
      ]
    },
    "openrouter": {
      "base_url": "https://openrouter.ai/api/v1",
      "api": "openai",
      "credential": {"namespace": "llm", "name": "openrouter"},
      "models": [
        {"id": "deepseek/deepseek-chat"},
        {"id": "anthropic/claude-sonnet-4"}
      ]
    }
  }
}
```

**结构**：

| 字段 | 说明 |
|---|---|
| `providers.<id>.base_url` | 服务商端点（已知服务商可省略，registry 补） |
| `providers.<id>.api` | api 类型（openai/responses/ollama；省略由 registry 推） |
| `providers.<id>.credential` | 凭证引用（省略由服务商 id 推导 llm/<id>） |
| `providers.<id>.headers` | 附加 header（可选）——**只存 env 引用**（如 `{"Authorization": "Bearer ${OPENROUTER_API_KEY}"}`），值走 env 展开，**不存字面量 secret**（§4.3） |
| `providers.<id>.models[]` | 模型数组，**每项只写差异字段** |
| `providers.<id>.models[].id` | wire model id（必填） |
| `providers.<id>.models[].context_window` / `*_price_per_million` / `supports_tools` 等 | 差异覆盖（可选） |

**向后兼容**：扁平 `models` map 保留（存量不动）。`providers` 是补充写法，`FromSettings` 展开时 `providers` 优先、扁平 models 兜底。

**冲突校验（§3.1）**：扁平 `models` 的 key **禁止含 `/`**（含 `/` 报错——避免与 `providers` 展开 key `<provider>/<model>` 碰撞）。一个模型不能同时出现在扁平段和 providers 段：`providers.<id>.models[].id` 与扁平 key 相同、且该扁平模型经 registry 推断的 provider 也是 `<id>` 时报错（明确冲突）；provider 不同（如扁平 `deepseek-v4-flash` 推断 provider=deepseek、providers.openrouter.models[].id=deepseek-v4-flash）**不冲突**——这正是跨服务商同名共存的目标。

### 3.2 展开规则（FromSettings）

`providers` → 扁平 ModelConfig map，模型 key 用 `connection-scoped`：

```
模型 key = providers.<id> 的展开别名
```

- 服务商级字段继承到每个模型：base_url / api / credential / headers
- 模型级差异字段覆盖
- 已知服务商（registry 命中）省略的字段由 registry 补（复用 `applyRegistryDefaults` 的 ProviderType 推导）
- `Catalog.ConnectionID = <provider_id>`、`Catalog.ProviderID = <provider_id>`、`Catalog.DisplayName = <model id>`——`/v1/runtime/models` 天然按服务商分组

### 3.3 模型 key 与跨服务商消歧

展开后扁平 map 的 key 需要唯一，且**不能含 `/`**（扁平 key 禁止 `/`，§3.1）。OpenRouter 的 model id 本身含 `/`（如 `deepseek/deepseek-chat`），直接拼 `<provider>/<model>` 会变三层歧义。因此 key 用**编码形式**，复用 alias 的 base64url 规则：

```
key = provider.<b64url(provider_id)> + "." + model.<b64url(model_id)>
```

这正好是 `/v1/runtime/models` 的 runtime alias 编码（provider.<b64>.model.<b64>），天然唯一、无斜杠、跨服务商同名可共存：

```
providers.dashscope.models[0].id = "qwen3-coder-plus"
  → key = "provider.ZGFzaHNjb3Bl.model.cXdlbjMtY29kZXItcGx1cw"
  → Catalog.ConnectionID = "dashscope", Catalog.ProviderID = "dashscope"

providers.openrouter.models[0].id = "deepseek/deepseek-chat"
  → key = "provider.b3BlbnJvdXRlcg.model.ZGVlcHNlZWsvZGVlcHNlZWstY2hhdA"   // 无斜杠，安全
```

**SelectModel 解析**：`--model` / `subagent_model` / TUI 选择接受三种形式（精确 map 查找，不 split）：
1. 完整 alias key（`provider.<b64>.model.<b64>`）
2. `<provider_id>/<model_id>`（用户可读形式，`SelectModel` 先按 provider 过滤再匹配 model id——OpenRouter 的 model id 含 `/` 时用「最左段是已知 provider」消歧）
3. 裸 friendly name（仅当唯一，兼容存量）

**default_model / subagent_model** 引用同样的 key 形式。存量无斜杠 friendly name 继续工作（形式 3）。

---

## 4. HTTP 配置管理端点

### 4.1 端点设计

| 方法 | 路径 | 语义 |
|---|---|---|
| GET | `/v1/providers` | 列出现有服务商分组（含模型数组；不含凭证值） |
| GET | `/v1/providers/{id}` | 单个服务商详情 |
| PUT | `/v1/providers/{id}` | 创建/更新服务商（upsert，模型数组整体替换） |
| DELETE | `/v1/providers/{id}` | 删除服务商 |

**请求/响应形状**（与 settings.json 的 providers 段一致，JSON）：

```json
// PUT /v1/providers/dashscope
{
  "base_url": "https://dashscope.aliyuncs.com/compatible-mode/v1",
  "api": "openai",
  "credential": {"namespace": "llm", "name": "dashscope"},
  "models": [{"id": "qwen3-coder-plus"}, {"id": "qwen3.7-max"}]
}
```

### 4.2 落盘与生效（含一致性）

- **落盘**：写 `<root>/.codeagent/settings.json` 的 providers 段（复用 `settings.Persist` 原子写 + unknown-key 保留）
- **生效**：写盘后调 `Reconfigure`（运行期热生效）——模型/凭证变更走现有连接注入路径；结构性变更（api 类型变）需重启（与现有 constraints 一致，design-connection-injection-channel.md §6）
- **凭证值**：providers 段只存引用（credential ref）与 env 引用（headers 的 `${VAR}`），值走 env/keychain/secretsJSON——**HTTP 不传、不存 secret 值**（对齐 settings.json committable 原则）

**写一致性与失败处理**（评审修订）：
- **单写入者约束**：`settings.Persist` 的 `writeMu` 是**进程内**锁。`/v1/providers` 跑在 server 进程（codeagentd/embed），而 CLI agent 进程也会写 settings（permission grant/verify）——两个进程 read-modify-write 并发会丢更新。**约束**：provider 配置写操作只允许在 server 进程发生（HTTP handler 持有写路径）；CLI 进程的 settings 写（grant/verify）通过**文件锁**（`flock` 或同款）与 HTTP 写互斥。具体：settings.Persist 升级为进程间文件锁 + read-modify-write。
- **落盘→生效失败回滚**：先 Persist 后 Reconfigure，若 Reconfigure 失败（如 build provider 出错），**回滚磁盘**（把 providers 段恢复到写前状态，或保留旧配置并返回错误给客户端 + 标记「配置已存但未生效，需重启」）。客户端收到明确状态，不出现「磁盘新、运行态旧」静默不一致。
- **modelName 语义**：provider 变更时 `Reconfigure` 的 modelName 传**当前默认模型 key**（不重置用户选择）；仅当默认模型所属 provider 被删除时才回退到剩余第一个。

### 4.3 鉴权（评审修订）

- **写端点（PUT/DELETE）要求 Bearer token 鉴权**（复用现有 server auth）
- **GET 也必须鉴权**（评审修订：providers 段含 headers 的 env 引用，虽非 secret 值但泄露结构；且 GET 响应**剥离 headers 与 credential 细节**，只返回 base_url/api/models/凭证状态）——与 §9「凭证值永不进 HTTP 响应」自洽
- embedded 场景：token 是宿主注入的内存 Bearer（现有机制）

### 4.4 与现有端点的关系

| 端点 | 现状 | 关系 |
|---|---|---|
| GET `/v1/runtime/models` | 只读模型目录（已分组） | **读目录**——客户端展示 |
| GET/PUT/DELETE `/v1/providers` | 新增 | **写配置**——客户端管理服务商 |
| 进程内 `Reconfigure` | 注入 secrets/connections | 落盘后触发生效 |

**闭环**：客户端 HTTP 写服务商 → 落盘 → 生效 → `/v1/runtime/models` 反映新分组。客户端不再需要本地 `ProviderConnectionRegistry` 作为唯一事实源（runtime settings.json 成为单一事实源）。

**embedded 双源澄清**（评审修订）：embedded（iOS）无磁盘 `<root>/.codeagent/settings.json`，配置走 host 注入（`Options.SettingsJSON`）。**HTTP /v1/providers 写路径仅适用于有磁盘的 desktop/server 部署**；embedded 场景保留「host 本地 registry + `buildConnectionsJSON` 注入」为唯一路径（HTTP 面只读目录）。「单一事实源」的承诺在两种部署各自成立：desktop = settings.json 文件，embedded = host registry。二者通过同一 providers 结构对齐（§5 映射），但写入通道不同。

---

## 5. 与 AgentKit ProviderConnection 的对齐

AgentKit/chater 的 `ProviderConnection`（ProviderConnection.swift:74-149）：

```
id（稳定连接 ID）+ providerID（模板类目）+ transport + authentication
+ baseURL + models: [ProviderModel] + isEnabled
```

**映射**（AgentKit 通过 HTTP 管理时）：

| ProviderConnection | settings providers 段                                               |
|---|--------------------------------------------------------------------|
| `id` | `providers.<id>` 的 key                                             |
| `baseURL` | `base_url`                                                         |
| `transport` | `api`（openai → openai）                                                |
| `authentication` | `credential`（gateway_account → gateway/default，api_key → llm/<id>） |
| `models` | `models[]`                                                         |
| `isEnabled` | 存在 = 启用；删除 = 禁用                                                    |

**关键转变**：客户端从「本地 registry 为唯一事实源 + 重启注入」改为「runtime settings.json 为单一事实源 + HTTP 管理」。`ProviderConnectionRegistry` 可降级为「HTTP 结果的本地缓存」（或直接移除，改为每次 HTTP 查询）。

**兼容期**：`buildConnectionsJSON`（运行期注入形状）保留——它是 embedded 启动时的一次性注入，与 HTTP 管理互补（HTTP 管理的是磁盘配置，注入的是运行态连接）。

---

## 6. 迁移与兼容

| 项 | 处理 |
|---|---|
| 存量扁平 `models` | 保留，正常读取（向后兼容） |
| 迁移工具 | `codeagent migrate` 可选把扁平 models 收敛为 providers 分组（不强制） |
| 已知服务商 | registry 继续补 base_url/env/api（providers 省略字段时） |
| `/v1/runtime/models` | 输出不变（已分组），只是数据源从扁平 models + catalog 变为 providers 展开 |
| TUI `/use` | friendly name 变为 alias key / `<provider>/<model>` 形式，picker 展示分组 |
| `--model` 参数 | 接受三形式：alias key、`<provider>/<model>`（最左段已知 provider 消歧）、裸 friendly name（唯一时） |

---

## 7. 实施顺序

### ① settings providers 段（磁盘格式 + 展开）

- `internal/settings/settings.go`：`File` 加 `Providers map[string]ProviderConfig`；`ProviderConfig{BaseURL, API, Credential, Headers, Models []ProviderModel}`；`ProviderModel{ID, ContextWindow, 价格, SupportsTools...}`
- `settings.Load` 合并 providers（后层 wins per provider id，models 数组整体替换）
- `app.FromSettings` 展开 providers → 扁平 ModelConfig（**key = alias 编码 `provider.<b64>.model.<b64>`**，§3.3；Catalog.ConnectionID/ProviderID 填充）
- 校验：扁平 key 禁止含 `/`；扁平 models 与 providers 同名且同 provider 冲突报错（§3.1）；provider 模型 id 含 `/` 由 alias 编码消歧
- 测试：展开、跨服务商同名消歧、OpenRouter 含 `/` model id、已知服务商省略字段、扁平+providers 冲突校验

### ② HTTP /v1/providers 端点

- `internal/server`：mux 加 GET/PUT/DELETE `/v1/providers[/{id}]`
- handler 读写 settings.json 的 providers 段（`settings.Persist` 原子写 + **进程间文件锁**，§4.2）+ 落盘后触发生效（Reconfigure，失败回滚磁盘，§4.2）
- 鉴权：**GET/PUT/DELETE 均需 Bearer token**；GET 响应剥离 headers/credential 细节（§4.3）
- PUT 校验：base_url（未知服务商必填）、模型数组非空、**credential ref 存在性**（指向不存在的 credentials 条目 → 400，或允许懒解析但标记）、已知服务商合法性
- DELETE 校验：被 default_model/subagent_model 引用 → 拒绝或回退（§4.2 modelName 语义）；删除后运行中会话行为明确（现有 turn 跑完，新 turn 拒绝该 provider）
- 测试：CRUD、凭证值不进 HTTP 响应、未知服务商校验、冲突校验、credential ref 校验、DELETE 悬空引用、Reconfigure 失败回滚

### ③ AgentKit/chater 改用 HTTP

- AgentKit：`ProviderConnectionRegistry` 从本地事实源改为 HTTP 查询/管理 runtime（或降级为缓存）
- chater：设置页从「本地 UserDefaults」改为「HTTP 调 runtime」
- 消除双份数据；`/v1/runtime/models` 成为展示目录

---

## 8. 风险与决策点

| 项 | 风险 | 决策 |
|---|---|---|
| 模型 key 语义变化 | 中 | friendly name → alias key（provider.<b64>.model.<b64>）；SelectModel 三形式解析（§3.3），TUI/CLI 需验证 |
| 扁平 models 与 providers 双写 | 中 | 扁平 key 禁 `/`；同名同 provider 冲突报错；存量扁平保留（§3.1） |
| HTTP 写配置的安全 | 高 | GET/PUT/DELETE 均 Bearer 鉴权；providers 段只存引用/env 引用不存值；GET 剥离 headers/credential（§4.3） |
| 跨进程写冲突 | 高 | settings.Persist 升级进程间文件锁；provider 写仅 server 进程（§4.2） |
| 落盘→生效失败 | 中 | Reconfigure 失败回滚磁盘 + 明确错误（§4.2） |
| credential ref 悬空 | 中 | PUT 校验 ref 存在；懒解析标记（§7 ②） |
| DELETE 悬空引用 | 中 | 被 default_model/subagent_model 引用 → 拒绝或回退；运行中会话明确（§7 ②） |
| 热生效 vs 重启 | 中 | 模型/凭证变更走 Reconfigure；api 类型变更需重启（文档明示） |
| chater 本地 registry 迁移 | 高 | desktop 兼容期保留为缓存，逐步切 HTTP；**embedded 保留注入路径（无磁盘，§4.4 澄清）** |
| 跨服务商同名模型 | 已解决 | (provider_id, model_id) 二元组 + providers 分组 |
| **范围边界** | 低 | providers 段只覆盖 chat-completions/responses/ollama 类服务商；**MCP 仍在独立 `.mcp.json`，gateway web_search 仍在 web 段**——本设计不合并它们（保持现有边界） |

### 8.1 Open Questions 裁决（stage ③ 调研后定案）

调研产出：`requirements-agentkit-provider-http.md`、`requirements-chater-provider-http.md`。两端的共同 open questions 在此裁决：

**OQ1 — enable/disable 语义：`isEnabled` 标志 vs presence/absence**

现状 `DELETE /v1/providers/{id}` 删除即丢配置（无「禁用保留」语义），与 chater 的 `setEnabled` 冲突。

**裁决：providers 段加 `enabled` 字段，默认 true；禁用 = `enabled: false`（保留配置），删除 = DELETE（移除配置）。**

```json
"providers": {
  "dashscope": {
    "enabled": false,
    "base_url": "...",
    "models": [{"id": "qwen3-coder-plus"}]
  }
}
```

- `settings.FromSettings` 展开时跳过 `enabled: false` 的服务商（模型不出现在 `/v1/runtime/models` 的可用列表，但配置保留）
- HTTP 端点：`PUT` 接受 `enabled`；新增 `PATCH /v1/providers/{id}` 切换 `enabled`（或 PUT 全量带 enabled）；`GET` 返回 `enabled` 字段
- 映射 chater `setEnabled` → `PATCH enabled`；DELETE 仅用于真正移除
- **影响**：`DELETE` 的 `ErrProviderInUse`（被 default_model 引用）保持拒绝；禁用不涉及引用检查（配置保留，default 仍可指向但运行时跳过 → SelectModel 报「未配置」）

**OQ2 — 「已存未生效」：`requires_restart` 标记**

现状：reconfigure=nil（codeagentd）时变更只落盘、重启生效；`reconfigure` 存在但 api 类型变时需重启——客户端不知道何时生效。

**裁决：写端点响应统一带 `applied` 字段：`true`（已热生效）/ `false`（已落盘，需重启）。**

```json
// PUT /v1/providers/dashscope → 200
{ "id": "dashscope", "applied": false }
```

- ProviderStore 判定：`reconfigure == nil` 或本次变更含 api 类型变化 → `applied: false`；否则 `true`
- chater 收到 `applied: false` → UI 显示「已保存，重启后生效」；`true` → 直接刷新模型列表
- `GET /v1/providers` 响应不含 applied（只读）

**OQ3（记录，暂不改）— codeagentd 读写路径不一致**

`settings.Load(root, home)` 读用户级 `~/.codeagent/settings.json`，但 `NewProviderStore(filepath.Join(root, ".codeagent", "settings.json"))` 写项目级 `<cwd>/.codeagent/settings.json` —— 读写在两层，PUT 落盘后运行时加载不到。**已记录为待办（用户 2026-08-06 指示先记录不改）**，实施 ③ 前必须修复：ProviderStore 的 settings 路径应与 settings.Load 的写入层对齐（用户级 + 项目级合并写）。

---

## 9. 验收

- 加 qwen 模型只写 `{"id": "qwen3.7-max"}` 一行（继承服务商公共配置）
- `deepseek-v4-flash` 可同时存在于 dashscope 和 openrouter，不冲突（alias 编码 key）
- OpenRouter 的 `deepseek/deepseek-chat` 模型 id 含 `/` 正确解析（alias 消歧）
- 客户端 HTTP GET /v1/providers 看到分组（响应剥离 headers/credential）；PUT 添加服务商后 /v1/runtime/models 反映新分组
- 凭证值永不进 HTTP 响应 / providers 段（含 headers 只存 env 引用）
- 存量扁平 models settings.json 正常运行（向后兼容）
- 与 AgentKit ProviderConnection 概念对齐（连接 = 服务商 + 模型数组）
- **并发写**：CLI grant 与 HTTP PUT 并发不丢更新（文件锁验证）
- **失败回滚**：Reconfigure 失败时磁盘回滚或明确「未生效需重启」标记
- **DELETE 悬空**：删除被引用的 provider 拒绝或回退默认模型，不静默崩
