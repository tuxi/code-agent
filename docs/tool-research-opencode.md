# 工具实现调研（二）：OpenCode

调研时间：2026-11-05。本地位置：`/Users/xiaoyuan/Documents/work/git/anomalyco/opencode`；HEAD：`fa2843f27058651d6ea78eccac30aefdd64e89ee`。只读检出源码、配置和测试；未安装依赖、运行应用或真实模型、执行测试、修改参考仓库。结论对应本版本，不以早期 OpenCode 实现或网上介绍替代当前证据。

本轮新增调研报告，不调整既有服务端设计草案。三家完成后再选择方案。

## 1. 结论

本版本采用混合方式：核心读、搜、编辑、命令等能力保留原生函数工具；其他能力默认可进入 Code Mode，用一个 execute 入口调用。Code Mode 初始提示词只展示有预算的目录摘要，未展开的能力通过 search 查询调用路径和类型签名。

这里的搜索是按需获取定义信息，不是将搜索结果逐个加入下一轮原生 tools 数组的“加载”。被搜索的工具已在当前 Code Mode inventory 中；搜索不创建一份持久的已加载工具集合，也不需要对每个搜索结果做加载、卸载或 TTL 管理。可调用性由 inventory 和执行权限决定，提示词要求模型先发现再使用未知工具。

最值得比较的方向是：稳定的少量原生工具、扩展目录按需发现、按模型选择编辑契约、对确定的参数结构错误作有限修复。不能因此认定 Code Mode 对所有模型都更可靠，或照搬它的运行时与插件系统。

## 2. 注册、快照与请求

Tool.Service 是 Location 作用域的完整目录。内置、插件与 MCP 通过 transform 注册，生成 schema 和实现。snapshot 先按 Agent／Session 权限隐藏整项禁止的能力，再分成 direct 与 Code Mode 两组。

```text
注册完整工具对象
    → 按权限得到当前可用目录
    → codemode:false：原生工具定义
    → 其他工具：Code Mode inventory + execute 入口
    → 会话 context hook 按模型等因素进一步调整定义
    → 为本次请求捕获定义与执行器
    → 供应商 schema 投影
    → 执行前 hook、参数解码、工具自身权限检查、输出验证
```

执行资格以本次请求保留的定义为准，hook 不能凭空增加没有注册来源的工具。工具快照捕获执行器；后续目录 reload／dispose 影响后续快照，不能假设所有主流项目都采用“旧快照立即撤销”的相同策略。工具自身仍负责执行时的权限判断。

- [完整目录与 snapshot](/Users/xiaoyuan/Documents/work/git/anomalyco/opencode/packages/core/src/tool.ts:251)。
- [请求 hook 后的定义选择与来源对应](/Users/xiaoyuan/Documents/work/git/anomalyco/opencode/packages/core/src/session/model-request.ts:270)。
- [本目录的架构约定](/Users/xiaoyuan/Documents/work/git/anomalyco/opencode/packages/core/src/tool/AGENTS.md)。

核心 read、edit、write、shell、glob、grep、skill、subagent、question、todo、websearch、webfetch 在已检查的插件中都显式 codemode:false。不能把本版本描述成“所有事情都必须写 JavaScript”。禁止 execute 时，快照逻辑会把原本 Code Mode 内的工具直接暴露出来，以保持它们可达；这也意味着改变执行模式可能显著增加定义体积。

## 3. 大目录怎么缩减

CodeModeCatalog.summarize 使用 2000 的估算 token 预算，成本近似为字符数除以 4；条目使用签名和最多 120 字符的一行描述。保留命名空间概览与 pinned 工具，再在各命名空间间轮流选择条目，优先较短条目。

这不是本步任务相关性的 LLM 筛选，也不是精确 tokenizer 的硬上限。命名空间与 pinned 项可超预算，源码还有大量命名空间需进一步限制的 TODO。应测量实际提示词，不直接把 2000 当成本项目可复用的安全阈值。

目录完整时不要求搜索；目录不完整时提示模型调用 search，禁止猜工具名。目录变动通过 Instructions 生成完整替换或较短差异提示，兼顾历史理解和请求缓存。

- [目录预算与采样](/Users/xiaoyuan/Documents/work/git/anomalyco/opencode/packages/core/src/codemode/catalog.ts:49)。
- [完整／部分目录及变化提示](/Users/xiaoyuan/Documents/work/git/anomalyco/opencode/packages/core/src/codemode/instructions.ts:9)。
- [预算、pinned 和部分目录的测试](/Users/xiaoyuan/Documents/work/git/anomalyco/opencode/packages/core/test/codemode/catalog.test.ts:33)。

## 4. 搜索实际做什么

模型通过 execute 中的全局 search 调用检索，例如：

```js
return search({ query: "list issues", namespace: "github", limit: 3 })
```

实际命名空间必须从目录取得，示例不代表默认安装 GitHub。结果包括准确 path、description、signature，以及 remaining／next 分页信息。取得签名后再编写实际调用，不经通用的字符串 execute_tool(name,args) 转发。

搜索用本地索引，不调用另一个模型、不使用向量数据库。路径精确匹配优先，其次按路径词、路径子串、描述与输入字段文字打分。默认返回 10 条，支持 limit、offset、namespace。只有选中的结果才需要渲染类型签名，目录及索引按 inventory 缓存。

源码的词法分割主要支持 a-z0-9；纯中文查询在该分词路径中不会产生有效词。中文检索效果需要独立验证，不能因其支持字符串 query 就推断具备跨语言语义检索。

运行时还兼容模型误写的 tools.search，前提是没有真实工具占用该路径。这是明确的别名容错，不是模糊猜测任意工具名。

- [搜索输入、输出与默认分页](/Users/xiaoyuan/Documents/work/git/anomalyco/opencode/packages/codemode/src/tool-runtime.ts:92)。
- [分词与确定性排序](/Users/xiaoyuan/Documents/work/git/anomalyco/opencode/packages/codemode/src/tool-runtime.ts:203)。
- [实际检索执行](/Users/xiaoyuan/Documents/work/git/anomalyco/opencode/packages/codemode/src/tool-runtime.ts:243)。
- [Code Mode 的入口与调用规则](/Users/xiaoyuan/Documents/work/git/anomalyco/opencode/packages/core/src/codemode/tool.ts:85)。

## 5. 模型对应不同编辑工具

patch 插件在 context／compaction／generate hook 中检查模型 ID。当前规则为：ID 包含 gpt-，且不含 oss 或 gpt-4，则移除 edit 和 write，保留 patch；其他模型移除 patch。

这是显式、简单的模型适配，不额外运行模型判断阶段。它说明“所有模型收到相同工具集合”不是必要原则，但字符串规则也不是未来所有模型的能力证明。我们的实现应依据实际模型／协议验证，而不是无条件复制名字匹配。

本版本工具叫 patch，输入字段为 patchText；不应沿用旧资料将其称为 apply_patch。它使用带 Begin Patch 等标记的补丁格式，而不是 CodeAgent 当前 JSON 包装的标准 unified diff。

- [模型选择编辑工具的 hook](/Users/xiaoyuan/Documents/work/git/anomalyco/opencode/packages/core/src/tool/plugin/patch.ts:290)。
- [补丁契约](/Users/xiaoyuan/Documents/work/git/anomalyco/opencode/packages/core/src/tool/plugin/patch.ts:20)。

## 6. 参数修复：有做，但有边界

ToolInputRepairPlugin 出现在内部 pre 插件清单。它在执行前基于 schema 尝试确定性的结构修复，随后仍走原 schema 校验：

- JSON 字符串对象／数组解析为对应容器。
- 数字字符串、明确的 true／false 字符串转成声明类型。
- 仅对确定不接受该占位值的可选字段，移除 null 或空对象占位。
- 在明确关闭额外字段且没有复杂组合语义时移除多余字段。
- 在 schema 明确允许的结构下处理数组、tuple、字典与局部引用。
- bash 名称不存在而 shell 存在时使用明确别名，结果告诉模型真实执行名称。

它不生成缺少的 path、oldString 或 newString，不推测编辑内容，不自动纠正任意字段名称，也不放宽权限。含歧义的 union／组合会保持原样，让后续验证决定是否接受。闭合 schema 的额外字段删除是该项目的策略取舍；我们未必需要照搬。若采用修复，需保留原始与实际执行参数的诊断关系，避免降低表面错误率却掩盖语义变化。

- [默认内部插件清单](/Users/xiaoyuan/Documents/work/git/anomalyco/opencode/packages/core/src/plugin/internal.ts:244)。
- [修复插件与明确别名](/Users/xiaoyuan/Documents/work/git/anomalyco/opencode/packages/core/src/plugin/tool-input-repair.ts:20)。
- [边界与歧义测试](/Users/xiaoyuan/Documents/work/git/anomalyco/opencode/packages/core/test/plugin/tool-input-repair.test.ts:51)。
- [修复后仍验证 schema 的集成测试](/Users/xiaoyuan/Documents/work/git/anomalyco/opencode/packages/core/test/plugin/tool-input-repair-integration.test.ts:19)。

## 7. 编辑匹配与失败反馈

edit 的 path、oldString、newString 全部必填，replaceAll 可选。相同旧新文本、空 oldString、无匹配、多处匹配和替换后未变化都作为明确失败返回，不伪装为修改成功。

匹配顺序是：保持文件换行信息的精确匹配、排版引号／横线／空格变体、逐行尾部空白。一般行首缩进依旧需要匹配，并没有广泛忽略缩进。与旧版介绍中的多重激进 fallback 不同，应以当前代码为准。

失败时可以返回“新文本已在文件中”的提示，或最多 12 行、每行最多 200 字符的相近原文，减少下一次调用盲猜。这些提示只有在当前读取权限允许时才显示；近似定位用来诊断，不直接决定修改位置。读取视图和编辑匹配对 CRLF／LF 的处理一致，并尽量保留未修改区域和混合换行。

- [编辑输入](/Users/xiaoyuan/Documents/work/git/anomalyco/opencode/packages/core/src/tool/plugin/edit.ts:24)。
- [有限容错与换行映射](/Users/xiaoyuan/Documents/work/git/anomalyco/opencode/packages/core/src/tool/plugin/edit.ts:168)。
- [失败提示](/Users/xiaoyuan/Documents/work/git/anomalyco/opencode/packages/core/src/tool/plugin/edit.ts:199)。
- [失败路径与权限控制](/Users/xiaoyuan/Documents/work/git/anomalyco/opencode/packages/core/src/tool/plugin/edit.ts:280)。

## 8. 结果、schema 与长命令

注册的同一工具对象提供模型 schema、执行实现和可选 output schema。运行时解码输入；失败返回具体字段问题与再次调用提示。成功值存在 output 声明时也会验证，但不把没有 output 声明的第三方工具等同于受同样强度的结构验证。

schema 在发给模型前还会做兼容投影：内联可展开的局部引用、简化安全可合并的 allOf，确保对象根，并针对 Gemini／Kimi 等协议或模型族选择处理规则。这些兼容工作与“模型能力弱”是不同问题，不能简单归到后者。

shell 也有默认两分钟前台超时，同时提供 workdir、timeout、background 等参数。后台完成通过通知继续处理，并明确要求模型不要反复 sleep／读取输出轮询。可借鉴其调用契约与任务反馈，但本轮没有验证重启后的进程／任务恢复行为。

在已检查的当前执行与插件链路中，没有找到早期 processor 风格的活动 doom_loop 检测实现；搜索到的 doom_loop 名称在旧配置转换代码中。不能拿旧版行为当成本版本证据。

- [输入解码与可操作错误](/Users/xiaoyuan/Documents/work/git/anomalyco/opencode/packages/core/src/tool/runtime.ts:64)。
- [schema 转换](/Users/xiaoyuan/Documents/work/git/anomalyco/opencode/packages/core/src/tool/runtime.ts:175)。
- [供应商兼容投影](/Users/xiaoyuan/Documents/work/git/anomalyco/opencode/packages/ai/src/protocols/utils/tool-schema.ts:86)。
- [命令与后台通知契约](/Users/xiaoyuan/Documents/work/git/anomalyco/opencode/packages/core/src/tool/plugin/shell.ts:21)。

## 9. 两家对照与设计影响

| 维度 | DeepSeek Harness 本地版本 | OpenCode 本地版本 |
| --- | --- | --- |
| 核心工具控制 | 预设、作用域、条件解析 | 原生核心工具、权限过滤、模型 hook |
| 扩展工具发现 | 已检查标准链路未见通用搜索入口 | Code Mode 部分目录 + 本地 search |
| 程序化调用 | PTC 可将全部有效工具 SDK 放入提示词 | 核心原生调用与扩展 Code Mode 混合；目录有摘要预算 |
| 搜索结果加载状态 | 不构成本轮已验证机制 | 查询定义信息，不维护逐结果加载／卸载状态 |
| 编辑必填 | file_path／old_string／new_string | path／oldString／newString |
| 匹配 | 常规精确匹配，归一化换行 | 精确优先，排版字符与行尾空白有限容错 |
| 模型适配 | 条件可读取本步模型选择 | 明确切换 patch 与 edit/write，并做 schema 适配 |
| 参数恢复 | 校验后返回明确错误 | 确定性结构修复，再校验 |
| 结果表达 | canonical value、isError 与输出验证 | Tool.Error／ToolFailure 与声明输出验证 |

对我们最直接的启示是先修好必填字段、错误状态、说明一致性和失败诊断，再按真实压力选择工具暴露策略。扩展能力按需发现有了更具体的参考，但不应直接搬入完整 Code Mode 解释器。

此前草案中的逐 Turn 加载状态、显式卸载、数量上限与持久化恢复，应继续视为待论证的方案成本，不是必须建设的基础设施。更简单的“常驻核心 + 有预算的扩展目录 + 发现入口”值得与原草案对照；是否采用原生工具装载或程序化入口，等第三家和固定模型评测后决定。
