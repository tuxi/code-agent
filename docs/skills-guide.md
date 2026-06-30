# Skills 使用指南

> code-agent 的渐进式技能系统。Skill 是一个 Markdown 文件，告诉模型**在特定场景下该怎么做**——模型主动查阅，运行时从不强行注入。

## 目录

- [快速开始](#快速开始)
- [Skill 是什么](#skill-是什么)
- [编写 Skill](#编写-skill)
- [Skill 放哪里](#skill-放哪里)
- [安装社区 Skill](#安装社区-skill)
- [Skill 如何被触发](#skill-如何被触发)
- [迭代 Skill](#迭代-skill)
- [调试与诊断](#调试与诊断)
- [进阶：L3 资源层](#进阶l3-资源层)
- [参考：设计文档](#参考设计文档)

---

## 快速开始

```bash
# 1. 脚手架创建一个 skill
codeagent skill init my-validator

# 2. 编辑 SKILL.md，写好 description
vim skills/my-validator/SKILL.md

# 3. 启动 agent，发一个匹配的任务
codeagent
> 帮我验证这次修改的测试覆盖率

# 模型看到 skill 索引 → 自动调用 load_skill("my-validator") → 按指令执行
```

---

## Skill 是什么

Skill 是一个**自主加载的指导文档**。它不是运行时规则，不自动执行——模型看到任务匹配 skill 的 description 后，**自己决定**调用 `load_skill` 加载完整指令。

```
system prompt 只含索引（name + description，极短）
        ↓  模型读索引，判断匹配
模型: load_skill("my-validator")
        ↓  返回完整 SKILL.md body
模型按指令执行
```

这保证了：加 50 个 skill 不会撑大 base prompt，无关 skill 永远不会进入上下文。

---

## 编写 Skill

### 最小可用的 SKILL.md

```markdown
---
name: my-validator
version: "1"
description: 修改 Go 测试文件或运行 go test 后触发。验证测试覆盖率未下降、无竞态条件。
---

# 验证检查清单

每次修改测试相关代码后：

1. `go test -race ./...` 检查竞态条件
2. `go test -cover ./...` 确认覆盖率未下降
3. 如果覆盖率下降，补充测试，不要降低阈值

## Gotchas

- race detector 在 CI 上比本地慢约 3 倍，务必本地先跑
- `-race` 和 `-cover` 不能同时用，需要分开跑
```

### 字段说明

| 字段 | 必填 | 说明 |
|------|:--:|------|
| `name` | **是** | 唯一标识，小写字母 + 连字符，如 `my-validator` |
| `description` | **是** | **唯一的触发条件**。模型据此判断是否加载。要具体到能区分相关/不相关任务，一两句话。 |
| `version` | 否 | 版本号，如 `"1"`。用于遥测对比不同版本的触发率。 |
| `license` | 否 | 许可证，如 `MIT`、`Proprietary`。在索引中展示 `[MIT]`。 |

### description 怎么写 — 80% 的功夫在这里

description 是 skill 的**承重墙**——它决定了 skill 是否被触发。

- ✅ **好：** "修改 Go 测试文件或运行 go test 后触发。验证覆盖率和竞态条件。"
- ❌ **差：** "测试相关的东西"（太模糊，从不触发）
- ❌ **差：** "任何时候修改代码后运行测试并检查覆盖率报告并修复问题"（太宽泛，到处触发）

**原则：** 给信息，不给脚本。Skill 在不同场景复用，过细的步骤会适得其反。写模型需要**知道**的，把判断留给模型。

### Gotchas — 最有价值的部分

成熟的 skill 的价值 ≈ 它的 Gotchas 的价值，而非正文。
**从真实踩坑中积累**，不要预判。每碰到一个边缘情况就追加一条：

```markdown
## Gotchas

- race detector 在 CI 上比本地慢约 3 倍（2026-06-30 发现）
- `-race` 和 `-cover` 不能同时用，必须分开跑（2026-07-03 踩坑）
```

### SKILL.md 写不下了？

保持 SKILL.md 在 500 行以内。超出部分放到 `references/` 目录：

```
skills/my-validator/
├── SKILL.md              # 主指令 + 索引（指向 references/）
├── references/
│   ├── race-detector.md  # 竞态检测详细指南
│   └── ci-config.md      # CI 集成配置
├── scripts/
│   └── coverage-check.sh # 覆盖率检查脚本
└── assets/
    └── .gitkeep
```

在 SKILL.md 中引用：

```markdown
## 参考文档

- 竞态检测详解：references/race-detector.md
- CI 集成配置：references/ci-config.md

## 可用脚本

- coverage-check.sh — 运行完整覆盖率检查，输出 JSON 报告
```

模型会在需要时用 `read_file` 读取参考文档，用 `run_command` 执行脚本。

---

## Skill 放哪里

| 位置 | 作用域 | 路径 |
|------|--------|------|
| **项目 skills** | 仅当前项目 | `<workspace>/skills/<name>/SKILL.md` |
| **全局 skills** | 所有项目 | `~/.codeagent/skills/<name>/SKILL.md` |
| **裸文件**（简单） | 同上 | `skills/<name>.md` |

项目 skill 覆盖同名的全局 skill（启动时 stderr 有警告提示）。

### iOS 路径

| 层 | 路径 |
|----|------|
| 全局/用户级 | `Application Support/skills/` |
| 项目级 | `<Documents>/skills/` |

---

## 安装社区 Skill

```bash
# 从 Git 仓库安装（需要仓库中有 .claude-plugin/marketplace.json）
codeagent plugin install https://github.com/anthropics/skills document-skills

# 查看已安装的插件
codeagent plugin list
# 输出：
#   document-skills  (from anthropic-agent-skills, 4 skills)

# 卸载
codeagent plugin remove document-skills
```

安装原理：clone 仓库到 `~/.codeagent/plugins/`，symlink skill 目录到 `~/.codeagent/skills/<marketplace>/<plugin>/`。可通过 `git pull` 更新。

### 与 Claude Code 互通

code-agent 的 skill 格式与 Claude Code **完全兼容**。同一个 `SKILL.md` 可以同时在两个系统中使用。code-agent 项目自己的 skill 也提供了 `.claude-plugin/marketplace.json`，可以被 Claude Code 安装。

---

## Skill 如何被触发

```
你: "帮我创建一个 PPT"
     ↓
模型在 system prompt 中看到索引（L1）:
  Skills — task-specific playbooks for this project...
  - pptx: 创建/编辑 .pptx 文件。使用 python-pptx 等工具。 [Proprietary]
  - verify-change: 修改代码后验证；失败则修源代码，不改测试。
     ↓
模型判断 "创建 PPT" 匹配 pptx skill → 调用 load_skill("pptx")
     ↓
工具返回（L2 body + L3 资源清单）:
  Loaded skill: pptx (v1) [Proprietary]

  # PPTX Skill
  使用 references/editing.md 编辑已有文件，
  使用 references/pptxgenjs.md 从零创建。

  ---
  Resources available in this skill:
    /Users/.../references/editing.md     (reference)
    /Users/.../references/pptxgenjs.md   (reference)
    /Users/.../scripts/thumbnail.py      (script)
    /Users/.../assets/template.pptx      (asset)
     ↓
模型按指令操作，需要时 read_file 或 run_command 资源
```

### 加载资源文件

模型可以在 `load_skill` 时直接请求某个资源：

```
load_skill(name="pptx", resource="references/editing.md")
→ 返回 editing.md 的完整内容
```

### 忘记 skill 名字？

```
load_skill(name="")
→ 返回所有可用 skill 的列表
```

---

## 迭代 Skill

### 热重载 — 不需要重启

编辑 `SKILL.md` 后保存，下一次 `load_skill` 会自动检测文件变更并重新加载。增删 `references/`、`scripts/` 中的文件后，touch `SKILL.md` 触发重新扫描即可。

```
# 编辑 skill
vim skills/my-validator/SKILL.md

# 新增参考文档
echo "# 新文档" > skills/my-validator/references/new-guide.md
touch skills/my-validator/SKILL.md

# 下次 load_skill("my-validator") 自动生效
```

---

## 调试与诊断

### 为什么我的 skill 没被触发？

1. **description 不够明确？** — 最常见的原因。description 太模糊 → 从不触发；太宽泛 → 到处触发。
2. **`name` 字段缺失？** — 启动时会记入 `Skipped`，skill 不会被加载。
3. **被全局 skill 覆盖？** — 启动时检查 stderr 是否有 `overrides global` 警告。
4. **文件位置不对？** — 确认在 `skills/<name>/SKILL.md` 或 `skills/<name>.md`。

### 启动日志

```
skills: project skill "shared" overrides global skill "shared"
```

### 跑 Eval 验证 skill 行为

```bash
# 需要 config.yaml 中有 API key
CODEAGENT_EVAL=1 go test ./internal/skills/eval/... -v
```

Eval 用例验证 skill 是否正确改变了模型行为（如：加工具不编辑 loop.go、commit 带 Co-Authored-By 尾注）。

### 批量给 .md 文件加 frontmatter

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

---

## 进阶：L3 资源层

### 目录结构完整版

```
skills/<name>/
├── SKILL.md         # 必需。frontmatter + 指令 body
├── references/      # 可选。按需 read_file 的参考文档
│   ├── api.md
│   └── examples.md
├── scripts/         # 可选。可执行脚本
│   ├── check.sh
│   └── thumbnail.py
└── assets/          # 可选。模板、字体、图标等
    ├── template.pptx
    └── logo.png
```

### 资源加载方式

| 资源类型 | 模型如何访问 | 需要确认？ |
|----------|-------------|:--:|
| `SKILL.md` body | `load_skill("name")` | 否 |
| `references/*.md` | `load_skill("name", resource="references/x.md")` 或直接 `read_file` | 否 |
| `scripts/*` | `read_file` 查看源码后 `run_command` 执行 | `run_command` 需确认 |
| `assets/*` | `read_file` | 否 |

**安全：** 资源路径经过穿越防护，`../../../etc/passwd` 会被拒绝。

---

## 参考：设计文档

- [P6 原始设计 PRD](p6-skills.md) — 渐进式披露架构、L1/L2/L3 三级模型
- [P6.1 改进计划](p6.1-skills-improvement-plan.md) — 与 Claude Code skills 的对比分析及改进路线
- [P6.1 改进计划](p6.1-skills-improvement-plan.md) — 与 Claude Code skills 的对比分析及改进路线
