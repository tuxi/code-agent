# Skills 接入指南：从裸 Markdown 到可加载的 Agent 技能

> 面向 iOS/macOS 端侧 agent 的技能文件编写与部署。

## 1. 双层架构

code-agent 加载两层 skill：

| 层 | 路径（macOS） | 路径（iOS） | 作用 |
|----|--------------|------------|------|
| 全局/用户级 | `~/.codeagent/skills/` | `Application Support/skills/` | 跨 workspace 共享，预装+用户导入 |
| 项目级 | `<workspace>/skills/` | `<Documents>/skills/` | 当前 workspace 专属 |

加载顺序：全局先 → 项目后。项目级同名 skill **覆盖**全局的。

## 2. 文件格式

两种布局均可，同一目录共存：

### 目录式

```
skills/
└── review-change/
    └── SKILL.md
```

### 平铺式（推荐：简单、可拖放）

```
skills/
├── review-change.md
├── news-pulse.md
└── earnings-review.md
```

### 文件内容

无论哪种布局，`.md` 文件必须以 `---` frontmatter 开头：

```markdown
---
name: my-skill
description: 这个技能做什么、什么时候用（一句话，50 字以内）
---

# 正文标题

具体步骤...
```

字段约定：

| 字段 | 必填 | 说明 |
|------|------|------|
| `name` | 否* | skill 唯一标识；缺则用文件名（去 `.md`） |
| `description` | **是** | L1 索引中展示的一句话，模型据此判断是否加载 |
| `version` | 否 | 版本号，如 `"1"` |

> \* 虽然有文件名 fallback，但建议总是写 `name`——文件名可能变，frontmatter 是 canonical identity。

## 3. 常见问题

### "我的 .md 文件放进去没反应"

**诊断**：启动后看 `os.Stderr` 日志：

```
[registry] skills: 18 loaded, 0 skipped
```

如果有 skipped：

```
[registry]   skipped "my-skill": frontmatter missing required field 'description'
```

| 错误信息 | 原因 | 修复 |
|---------|------|------|
| `SKILL.md must start with '---'` | 文件直接 `# 标题` 开头，没用 frontmatter | 加 `---` YAML 头 |
| `missing required field 'description'` | frontmatter 里没有 `description:` | 补上 |
| `invalid frontmatter YAML` | `---` 之间的内容不是合法 YAML | 检查缩进、引号 |
| `duplicate skill name` | 两个文件的 `name` 相同 | 改名或合并 |

### 静默跳过（不记 Skipped）

以下情况**不报错不记日志**：

- 目录里没有 `SKILL.md`（但又不含 `.md` 文件）→ 不是 skill，跳过
- 文件不以 `.md` 结尾 → 忽略
- Skills 目录不存在 → 返回空注册表，正常行为

这会导致「文件放下了但不知道为什么不加载」。脚本批量注入 frontmatter 可避免此类问题。

## 4. 批量修复脚本（无 frontmatter → 自动化注入）

对一批直接 `# 标题` 开头的 `.md`，自动补最小 frontmatter：

```bash
cd /path/to/skills
for f in *.md; do
  if head -1 "$f" | grep -q '^---$'; then
    echo "skip (has frontmatter): $f"
    continue
  fi
  name="${f%.md}"
  desc=$(head -5 "$f" | grep '^# ' | head -1 | sed 's/^# *//' | tr -d '\n')
  [ -z "$desc" ] && desc="$name"
  tmp="${f}.tmp"
  { echo "---"; echo "name: ${name}"; echo "description: ${desc}"; echo "---"; echo ""; cat "$f"; } > "$tmp"
  mv "$tmp" "$f"
  echo "fixed: $f"
done
```

## 5. 部署流程（iOS）

1. `./scripts/build-ios.sh` — 把 `skills/review-change` 和 `skills/verify-change` 打进 `build/skills/`
2. Swift 侧 `AgentRuntime.start()` 首次启动时从 `Bundle.module` 拷贝到 `Application Support/skills/`（**注意不是 `Documents/skills/`**）
3. 用户通过 Files / AirDrop / 下载 往 `Application Support/skills/` 或 `<workspace>/skills/` 里加 `.md` → 重启 → `load_skill` 即可用

## 6. 目录约定速查

| 你想做什么 | 放这里 | 什么时候生效 |
|-----------|--------|-------------|
| 所有项目都能用的 skill | `Application Support/skills/`（iOS）或 `~/.codeagent/skills/`（macOS） | 重启 App |
| 只在当前打开的项目用 | `<workspace>/skills/` | 重启 App |
| 测试 skill 格式是否合法 | 跑 `go test ./internal/skills/...` | 即时 |

## 7. 设计参考

符合 Claude Code 的 `~/.claude/skills/`（全局）和 `.claude/skills/`（项目）双层模式。差异点：Claude Code 的 skill 可以有附加资源文件（`*.py`、`*.txt` 等），code-agent v1 只加载 `SKILL.md` 或 `.md` 正文——附加文件需以后扩展。
