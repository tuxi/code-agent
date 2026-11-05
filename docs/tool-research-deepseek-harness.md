# 工具实现调研（一）：DeepSeek Harness

调研时间：2026-11-05。检出位置：`/Users/xiaoyuan/Documents/work/git/deepseek-harness`；HEAD：`fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159`。结论限于本地检出版本，不代表其他版本或运行部署。

本轮只读源码、配置、测试和已提交快照。未安装依赖、启动应用、调用真实模型、运行测试或修改参考仓库。服务端设计草案保持候选状态，等三家调研完成后再定。

## 1. 主要结论

它不是一个已证实通过“每步理解用户意图，再自动搜索工具”工作的实现。已核对的核心、标准预设和调用链体现的是：插件组合确定基础能力，作用域限制确定继承能力，步骤开始时运行条件确定本步可用能力，适配器再按供应商能力编码工具定义与变化。

最值得借鉴的是四件事：定义与执行共用步骤工具集、完整记录定义变化、统一结果状态与输出验证、为重复调用提供可观察的提示。

不能从“支持动态工具变化”推导出“自动缩减无关工具”；也不能从“API 里只有一个工具”推导出“提示词里只有一个工具的说明”。

## 2. 从注册到执行的实际链路

```text
配置加载插件／预设
    → tools.register(definition, { when? })
    → 当前 Agent 的继承、覆盖和 allow/deny 限制
    → preStep 捕获模型选择，tools.resolve(...)
    → 生成 ResolvedToolSet
    → systemPrompt.assemble 使用同一工具集
    → executor.describe 生成模型表示
    → request/header 记录完整定义，必要时记录增删消息
    → 供应商适配器生成请求
    → 本批调用持有同一 ResolvedToolSet
    → 统一 pipeline：校验、策略、执行、结果验证和记录
```

源码：

- [注册与作用域解析](/Users/xiaoyuan/Documents/work/git/deepseek-harness/packages/core/tools/src/index.ts:113)。
- [每步解析工具集](/Users/xiaoyuan/Documents/work/git/deepseek-harness/packages/core/agent-loop/src/agent.ts:283)。
- [工具集的执行资格与生命周期](/Users/xiaoyuan/Documents/work/git/deepseek-harness/packages/core/tools/src/index.ts:251)。
- [模型请求与工具变更记录](/Users/xiaoyuan/Documents/work/git/deepseek-harness/packages/core/agent-loop/src/agent.ts:656)。

`when` 可以异步执行，输入包含 Agent、捕获的模型选择和取消信号。已看到的业务用途包括网页提供方是否就绪、Agent Team 是否启用、计划模式状态。未发现这些调用点通过额外 LLM 对当前任务作自然语言分类。

## 3. 工具数量主要靠组合配置控制

Web 的 minimal 预设明确关闭 read、write、edit，仅保留图片读取和 shell 等能力；standard 预设单独组合文件、搜索、命令、技能、计划和子代理能力，并显式关闭 apply_patch。它允许不同预设选择不同工具组合，而不是一个全局目录无差别给所有 Agent。

本轮直接读取已提交快照，得到以下特定场景的数据：

| 快照场景 | 初始模型工具数 | 说明 |
| --- | ---: | --- |
| web/minimal-preset | 3 | bash、read_image、working_directory |
| sdk/bash-tool | 26 | 文件、命令、搜索、目标、子代理等 |
| web/cordis-tool-round | 34 | 含运行时扩展与普通编码能力 |
| session/ptc-script-retained | 1 | 唯一原生入口 run_code |

这些是测试场景的已提交期望，不是所有产品部署的默认数量，也不是本轮实测模型表现。

- [minimal 预设](/Users/xiaoyuan/Documents/work/git/deepseek-harness/packages/bundle/web-app/presets/minimal.patch.yml:5)。
- [standard 预设](/Users/xiaoyuan/Documents/work/git/deepseek-harness/packages/bundle/web-app/presets/standard.patch.yml:5)。
- [minimal 模型定义快照](/Users/xiaoyuan/Documents/work/git/deepseek-harness/snapshots/web/minimal-preset/tool-schemas.expected.json)。

另一个重要取舍：standard 的计划模式说明明确要求保持工具目录稳定，以避免请求缓存变化。它没有为了进入计划阶段就把所有修改工具定义移出请求。不能把“按步骤解析”理解成“每步都应该更换目录”。

## 4. 动态变化与供应商兼容

本步先捕获模型选择和工具集，重试复用该选择与提示词。工具条件失败会报告激活错误，不把失败静默当作“无需此工具”，也不会提交一个部分成功的集合。

同一步新注册的工具不自动进入原工具集。执行前还会复查注册身份和当前作用域限制，原定义被同名替换时，未开始的旧调用不能悄悄执行新实现。步骤结束后工具集失效。

工具定义变化记录为 request/header；名称增删再记录 tool-addition/tool-removal，并关联完整定义。适配器支持三类路径：

- 不支持历史工具更新：发送当前完整工具集合，移除历史更新块和延迟标记。
- addition-only：用历史中的添加激活工具；删除等情况需要重建声明基线。
- in-history：可以按历史顺序表示添加与删除。

这里的“当前完整集合”是作用域和条件过滤后的集合，不是把所有插件重新放开。`defer_loading` 支持也不等于实现了工具搜索或相关性排序。

- [供应商中立投影](/Users/xiaoyuan/Documents/work/git/deepseek-harness/packages/llm/llm/src/content.ts:439)。
- [DeepSeek 更新模式声明](/Users/xiaoyuan/Documents/work/git/deepseek-harness/packages/llm/llm-deepseek/src/types.ts:36)。
- [工具更新与重试测试](/Users/xiaoyuan/Documents/work/git/deepseek-harness/packages/core/agent-loop/tests/tool-updates.spec.ts:54)。
- [步骤资格、同名替换与作用域测试](/Users/xiaoyuan/Documents/work/git/deepseek-harness/packages/core/tools/tests/activation.spec.ts:43)。

## 5. Native 与 PTC 必须分别计量

NativeExecutor 将本步所有有效工具直接映射成 schema，没有在 describe 内再做意图检索或数量预算。

PTCExecutor 只向 API 暴露 run_code，但同时生成 tools:sdk 提示词片段，包含当前有效能力的 TypeScript／Python SDK 声明及输出类型。嵌套调用仍经统一执行 pipeline 和本步工具资格检查。

因此 PTC 可以减少直接函数入口、合并多次调用或处理流程，但其收益不能只用 tools 数组长度衡量。还要计入系统提示里的 SDK 说明、程序输出、额外代码生成与执行成本。对弱模型也不能预设程序化调用一定更可靠。

- [Native describe](/Users/xiaoyuan/Documents/work/git/deepseek-harness/packages/core/tool-executor-native/src/index.ts:15)。
- [PTC 唯一入口与 SDK 提示词生成](/Users/xiaoyuan/Documents/work/git/deepseek-harness/packages/core/tool-executor-ptc/src/index.ts:36)。

## 6. 编辑契约与结果处理

常规 edit 使用 file_path、old_string、new_string，三者全部必填；replace_all 可选。执行前还拒绝空 old_string 和 old_string == new_string。底层采用精确文本匹配，归一化换行但不忽略一般缩进差异，默认要求唯一匹配。

这说明减少编辑错误不必以模糊匹配为前提。与 CodeAgent 当前 `new` 非必填相比，它的常规 edit 能在缺替换内容时直接拒绝，不会把缺字段自然转成空替换。

同时不能把它的全部工具视为同一种契约：另一个可选 str_replace_editor 接口中，str_replace 的 new_str 省略即删除。该行为是独立兼容接口，不应机械复制到我们的 edit_file。

参数 DSL 会为必填字段生成 schema，并在 defineTool.execute 前验证；但参数根对象默认开放，没有全局自动 additionalProperties:false。它不是“所有未知字段都严格拒绝”的范例。我们仍需独立确定是否对内置工具关闭未知字段。

成功执行返回 canonical JSON value，统一 pipeline 验证输出 schema，再生成模型内容和 UI 元数据；错误具有明确的 isError 和错误身份。最终结果快照冻结后用于观察与日志，避免依靠错误文案猜测成功与失败。后处理替换的成功值也要经过输出验证。

本地文件修改采用目标级锁和原子写入；可选观察策略可提供已读版本检查并拒绝过期修改。但基础配置中 fs-observation-policy 明确 disabled:true，不能将其作为默认保护已生效的证据。

- [常规编辑参数与 no-op 检查](/Users/xiaoyuan/Documents/work/git/deepseek-harness/packages/fs/tool-fs/src/edit.ts:47)。
- [精确替换实现](/Users/xiaoyuan/Documents/work/git/deepseek-harness/packages/fs/fs-local/src/fsio.ts:862)。
- [参数根对象与错误类型](/Users/xiaoyuan/Documents/work/git/deepseek-harness/packages/core/tools/src/schema.ts:450)。
- [成功值和输出投影验证](/Users/xiaoyuan/Documents/work/git/deepseek-harness/packages/core/tools/src/pipeline.ts:537)。
- [原子修改与可选版本检查](/Users/xiaoyuan/Documents/work/git/deepseek-harness/packages/fs/fs-local/src/index.ts:250)。
- [基础组合中关闭观察策略](/Users/xiaoyuan/Documents/work/git/deepseek-harness/packages/bundle/base/cordis.patch.yml:292)。

## 7. 重复调用恢复

repeat-tool-reminder 是独立插件，不在 loop 中按具体业务工具写重试逻辑。它按 Agent 统计连续相同调用，对 JSON 对象键排序后比较完整参数，默认在 3、5、8 次时注入提醒；参数展示截断，但检测使用完整值。

它不阻断、不修改参数、不自动换方案；被拒绝的调用也可以计数，用户新消息会重置链。两个不同错误交替出现不会被这种“连续同一调用”检测完整覆盖，因此不是通用的所有无进展循环检测器。

- [提醒插件实现](/Users/xiaoyuan/Documents/work/git/deepseek-harness/packages/guard/repeat-tool-reminder/src/index.ts:180)。
- [基础组合启用与阈值](/Users/xiaoyuan/Documents/work/git/deepseek-harness/packages/bundle/base/cordis.patch.yml:456)。

## 8. 对我们候选设计的启示

| 候选方向 | 本轮证据 | 暂定判断 |
| --- | --- | --- |
| 工具数越少越好 | 不同预设为 3、26、34 个；计划模式考虑缓存稳定 | 需要按任务配置与成本权衡，不追求单一最小数字 |
| 每步先分类再选工具 | 条件解析存在；未找到核心标准链路的意图分类器 | 不引入额外分类 LLM 的判断仍合理 |
| 模型主动搜索加载 | 已检查核心与标准预设没有通用 tool_search 入口 | 本家不足以证明我们的搜索方案最优，继续看后两家 |
| 请求级工具快照 | 定义、执行、重试共用步骤集合；执行前复查身份 | 强烈值得借鉴 |
| 动态 schema 与缓存 | 有历史更新协议和不支持时的明确投影路径 | 需要供应商能力声明，不应假设一种协议通吃 |
| 弱模型编辑可靠性 | new_string 必填，no-op 拒绝，输出验证 | 先修契约和结果反馈，不能仅靠减少工具 |
| 单一代理执行器 | PTC 单入口但仍生成 SDK 提示词 | 不作为默认减负方案，需单独评测 |
| 失败恢复 | 独立可配置重复提醒，不自动重试 | 符合模型决定下一步、运行时提供事实的方向 |

待其余两家完成后，再比较发现入口、工具集生命周期、检索命中、缓存策略、跨供应商适配与真实评测结果。本轮不调整服务端设计、不移植代码，也不据此承诺错误率下降幅度。
