# 配置统一：config.yaml 并入 settings.json

> **状态**: ✅ 阶段 A/B/C 已落地（2026-08-05，commit 98a8b42）。config.yaml 已移除为配置源；settings.json 为唯一配置层；迁移工具 `codeagent migrate` 可用。
> **受众**: Runtime 开发 + AgentKit 开发 + 架构评审
> **版本**: v1.0（实现基线）
> **日期**: 2026-08-04
> **前置**: design-connection-flattening.md（分层）、docs/p11-project-settings.md（settings 层）

---

## 1. 背景与问题

### 1.1 现状：两套并行配置系统

flattening 引入了 `config.yaml` 分层（用户级 `~/.codeagent/config.yaml` + 项目级 `<cwd>/.codeagent/config.yaml`），与 P11 已落地的 `settings.json` 分层（用户级 + 项目共享 + 项目本地）并存：

```
<root>/.codeagent/config.yaml          ← flattening（infrastructure）
<root>/.codeagent/settings.json        ← P11（behavior，committable）
<root>/.codeagent/settings.local.json  ← P11（behavior，git-ignored）
~/.codeagent/config.yaml               ← flattening（infrastructure）
~/.codeagent/settings.json             ← P11（behavior）
```

**同一目录下两套「用户级/项目级」分层**，各有各的发现机制、加载器、写入方。

### 1.2 重叠字段（已核实）

逐一核对 `Config`（`internal/app/config.go:43-113`）与 `settings.File`（`internal/settings/settings.go:57-63`）：

| Config 字段 | 是否被 settings 重复 | 消费方 |
|---|---|---|
| `Hooks` | **是** | `runner.go:58` 拼接 config.Hooks + settings.Hooks |
| `Permissions` | **是** | `rules.go:86` settings 三层合并 |
| `Agent.VerifyCommand` | **是** | `runner.go:87` settings 覆盖 config |
| Models/Credentials/Provider/Runtime/Server/Web/Currency | 否 | 仅 config |
| MCP / Warnings / StoreFactory / Profile | 内部字段 | 与用户无关 |

**真正重叠的只有 3 个字段**（Hooks/Permissions/VerifyCommand）——已实现「config 兼容 + settings 优先」。合并的实质是把这 3 个从 config 彻底移除，settings 成为唯一承载者，再扩展 settings 承载 infrastructure。

### 1.3 为什么合并

1. **开源心智负担**：一个文件、一套分层规则才可上手。现在要理解两套文件、两套层级、两套发现机制——用户已经「不知道 settings.json 的作用」（验证过）。
2. **settings 已是更成熟的机制**：三层（user/shared/local）、原子写（`Persist`，`write.go:38`）、unknown-key 保留、agent 可写（`SetVerifyCommand`）。config 只有两层，无原子写。
3. **生态对齐**：pi 是 `settings.json`（`~/.pi/agent/settings.json` + `<cwd>/.pi/settings.json`，settings-manager.ts:195-196），Claude Code 也是 `.claude/settings.json`。合并后 `.codeagent/` 与主流 agent 完全对齐。
4. **写入方统一**：agent 已写 settings（grant、verify），不能写 config。合并后单一写入目标。

---

## 2. 目标形态

### 2.1 合并后文件布局

```
~/.codeagent/settings.json            ← 用户级（唯一用户配置）
<root>/.codeagent/settings.json       ← 项目共享（committable）
<root>/.codeagent/settings.local.json ← 项目本地（git-ignored）
```

`config.yaml` 移除。加载优先级（低 → 高）：用户 → 项目共享 → 项目本地。

### 2.2 合并后的 settings.json 结构

```json
{
  "models": {
    "deepseek": {
      "provider": "openai",
      "base_url": "https://api.deepseek.com",
      "model": "deepseek-v4-flash",
      "credential": {"namespace": "llm", "name": "deepseek"},
      "context_window": 128000,
      "input_price_per_million": 0.27
    }
  },
  "credentials": {
    "llm": {"deepseek": {"source": "env", "env": "DEEPSEEK_API_KEY"}}
  },
  "default_model": "deepseek-pro",
  "subagent_model": "deepseek",
  "agent": {
    "max_steps": 68,
    "max_parallel_tools": 8,
    "compact_ratio": 0.75
  },
  "provider": {
    "request_timeout_seconds": 600,
    "max_retries": 5
  },
  "web": {"search": {"provider": "tavily", "tavily_api_key_env": "TAVILY_API_KEY"}},
  "runtime": {"max_concurrent_turns": 5},
  "currency": "$",
  "permissions": {"allow": [], "deny": []},
  "verify": {"command": "auto"},
  "hooks": []
}
```

### 2.3 机密性约束（关键设计决策）

settings.json 是 **committable**（settings.go:13-14 明示「Secrets never live here」）。合并后：

- **凭证值**（API key、access token）**不进 settings.json**——`credentials` 段只放「引用」（`source: env` + env 变量名、`source: injected`、keychain ref）
- **值走 env / keychain / secretsJSON 注入**——与 flattening 已做的「凭证 = ref + resolver」一致
- `server.access_token` 这类机密：只放 `access_token_env`（env 变量名），不放值

### 2.4 兼容性

- **config.yaml 移除**：现有用户 config.yaml 需要迁移（见 §4 阶段 C 迁移工具）
- **YAML → JSON**：全量迁移到 JSON 格式，`Config` 结构改 json tag

---

## 3. 合并的 4 个难点与对策

| 难点 | 说明 | 对策 |
|---|---|---|
| YAML → JSON 迁移 | config.yaml 是 YAML，settings.json 是 JSON | 阶段 C 迁移工具（解析 config.yaml → 写 settings.json）|
| 凭证机密性 | settings committable，不能存 access_token/key 值 | 凭证段只放引用（§2.3）；值走 env/keychain |
| embedded 注入路径 | `Options.ConfigYAML` + `Options.SettingsJSON` 两条 | 统一为 `Options.SettingsJSON` 一条 |
| `Config` 结构 | 大量 json tag 迁移 | 分阶段：先重叠字段移除（§4 阶段 A），再全量 |

---

## 4. 分阶段方案

### 阶段 A：消除双轨（低风险，现在可做）

**移除 config.yaml 的 3 个重叠字段（Hooks/Permissions/VerifyCommand），统一走 settings。**

- [ ] `Config.Hooks` / `Config.Permissions` / `Agent.VerifyCommand` 从 YAML 移除（消费方已接 settings）
- [ ] `runner.go` 不再拼接 `cfg.Hooks`（只走 `set.Hooks`）
- [ ] `rules.go` 不再读 `cfg.Permissions`（只走 settings 三层）
- [ ] `runner.go:87` 移除 legacy `cfg.Agent.VerifyCommand` 回退（只走 `ResolveVerifyFrom`）
- [ ] `config.example.yaml` / 用户配置去掉这 3 段
- [ ] 测试：这 3 个字段从 config 配置到 settings 配置后行为不变

**效果**：双轨消除，settings 成为 behavior 唯一承载者。不动 infrastructure。

### 阶段 B：settings 承载 infrastructure（中风险）

**扩展 `settings.File` 支持 models/credentials/agent/provider/web/runtime/currency 段。**

- [ ] `settings.File` 加 `Models`/`Credentials`/`Agent`/`Provider`/`Web`/`Runtime`/`Currency`/`DefaultModel`/`SubagentModel` 字段（JSON tag）
- [ ] `settings.Load` 合并这些段（复用 `MergeConfigs` 的字段级合并逻辑，从 app 包迁移或复用）
- [ ] `Config` 增加「由 settings 构建」的路径——`settings.Load` 产物 → `Config`，config.yaml 降级为兼容读取（只读不写）
- [ ] embedded 注入：`Options.SettingsJSON` 承载全部，`Options.ConfigYAML` 标记 deprecated
- [ ] 测试：settings 承载的 infrastructure 与 config.yaml 等值

**效果**：settings 成为唯一配置源，config.yaml 只剩兼容读取。

### 阶段 C：移除 config.yaml（高风险）

**迁移工具 + 移除。**

- [ ] 迁移工具：解析现有 `~/.codeagent/config.yaml` + `<root>/.codeagent/config.yaml` → 合并写入 settings.json（三源合一）
- [ ] 凭证值检查：迁移时把 access_token 等机密改写为 `_env` 引用，不落 settings 值
- [ ] 移除 `LoadConfigLayered` / `LoadConfigBytes` 的 config.yaml 路径
- [ ] 更新 README / 模板 / 设计文档
- [ ] 兼容期：检测到旧 config.yaml 时警告「迁移到 settings.json」

**效果**：config.yaml 彻底移除，`.codeagent/` 只有 settings.json 一套。

---

## 5. 与 pi / Claude Code 的对齐

| 维度 | pi | Claude Code | code-agent（目标） |
|---|---|---|---|
| 用户配置 | `~/.pi/agent/settings.json` | `~/.claude/settings.json` | `~/.codeagent/settings.json` |
| 项目配置 | `<cwd>/.pi/settings.json` | `.claude/settings.json` | `<root>/.codeagent/settings.json` |
| 项目本地 | — | `.claude/settings.local.json` | `<root>/.codeagent/settings.local.json` |
| 凭证 | `auth.json`（值） | env/keychain | env/keychain + secretsJSON（引用在 settings） |
| 格式 | JSON | JSON | JSON |

完全对齐。凭证值仍走各自的机密通道，settings 只承载配置与引用。

---

## 6. 风险与决策点

| 项 | 风险 | 决策 |
|---|---|---|
| 阶段 C 迁移破坏现有用户配置 | 高 | 迁移工具 + 兼容期警告；凭证值改写为引用 |
| embedded 注入路径变更 | 中 | 阶段 B 统一到 SettingsJSON，ConfigYAML 兼容期 |
| `MergeConfigs` 逻辑跨包复用 | 中 | settings 包不能 import app（循环依赖）；合并逻辑迁到 settings 或公共包 |
| registry 内建默认的归属 | 中 | registry 保持代码内建（不进 settings），settings 只覆盖 |

---

## 7. 验收标准

- `.codeagent/` 只有 settings.json 一套配置（用户 + 项目共享 + 项目本地）
- config.yaml 彻底移除，无兼容读取
- 凭证值不进 committable 的 settings.json（机密走 env/keychain/secretsJSON）
- embedded 注入走单一 SettingsJSON 通道
- 三端（TUI/codeagentd/embedded）行为与合并前一致
- pi / Claude Code 的 settings.json 可直接理解 code-agent 配置（格式对齐）
