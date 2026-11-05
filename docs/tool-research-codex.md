# 工具实现调研（三）：Codex 与三家对照

调研时间：2026-11-05。本地位置：`/Users/xiaoyuan/Documents/work/git/codex`；HEAD：`86ac0916597244a2e67a197822a5483924660ff1`。只读检查源码与测试；未安装依赖、运行真实模型、执行 Rust 测试或修改参考仓库。测试部分是阅读断言所得，不代表本机执行通过。结论仅对应该检出版本。

## 1. 主要结论

Codex 同时支持直接暴露、延迟发现和 Code Mode 等工具呈现方式，具体组合受模型元数据、配置、权限策略和功能开关控制。并不是每一步额外调用一个分类模型，再按任务阶段自动挑选工具。

最值得借鉴的原生发现链路是：模型调用 tool_search → 客户端本地 BM25 搜索允许发现的工具 → 将所选完整定义放进专门的 tool_search_output 历史项 → 下一次模型请求利用该结果调用工具。对应测试明确要求：不把搜索结果追加进普通 tools 数组。

这依赖支持该协议的模型／接口。它不能原封不动移植到任意 OpenRouter 模型，或用普通文本返回工具说明就假定具有同样效果。

## 2. 暴露范围与能力适配

注册目录与模型本次看到的定义分离；直接、延迟、Code Mode、隐藏等 exposure 可组合或裁剪。MCP 工具在模型支持搜索且策略允许时可以从直接暴露改为延迟发现，否则移除延迟路径，保留其余允许的暴露方式。其他配置仍可能让工具隐藏或只走 Code Mode，不能概括成任何不支持搜索的模型都会收到全部工具。

tool_search 只有在模型声明 supports_search_tool、策略允许且确有可搜索的延迟工具时才加入。工具目录、策略与请求呈现由运行时构建，不通过对自然语言用户需求做权限分类。

模型信息还决定终端类型、补丁能力与工具说明覆盖。未知模型的兜底元数据不声明搜索能力，apply_patch_tool_type 也为空。这比“所有模型发送同一套工具，再多写几条提示词”更谨慎，但元数据本身仍需真实兼容性验证。

- [MCP 暴露方式调整](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/core/src/tools/spec_plan.rs:297)。
- [有条件注册搜索入口](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/core/src/tools/spec_plan.rs:425)。
- [仅直接可见定义进入普通请求工具列表](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/core/src/tools/spec_plan.rs:660)。
- [未知模型能力兜底](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/models-manager/src/model_info.rs:106)。
- [补丁工具受模型能力控制](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/core/src/tools/spec_plan.rs:1458)。

## 3. 搜索是本地检索，不是额外模型

tool_search 输入只有必填 query 和可选 limit；schema 关闭额外字段。执行时拒绝空查询和零 limit，默认返回上限为 8 个搜索条目。条目可以包含命名空间下的多个工具，不能直接把 8 理解成严格的独立函数数量或 token 预算。

索引覆盖名称、拆分下划线后的名称词、描述、输入 schema 的字段名和描述，以及命名空间信息。BM25 使用本地 corpus 和 Language::English；源码中的 Embedder 是该词法检索库的表示构建器，不能据名字误读成调用远程 embedding 服务。中文查询召回效果仍需评测。

目录索引有缓存，目录变化时重建；共享定义以 Arc 保存，只对选中项物化返回定义。原生搜索结果会整理命名空间并带 defer_loading。部分 Code Mode 路径返回适合嵌套调用的名字／描述，不能与原生结果表示混为一谈。

- [搜索入口契约](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/core/src/tools/handlers/tool_search_spec.rs:16)。
- [缓存与目录身份](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/core/src/tools/handlers/tool_search.rs:65)。
- [本地英文 BM25 索引](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/core/src/tools/handlers/tool_search.rs:147)。
- [输入检查与结果分支](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/core/src/tools/handlers/tool_search.rs:208)。
- [共享定义、检索文字与延迟结果规范化](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/tools/src/tool_search.rs:1)。

搜索入口可以提示可用来源，也可以省略来源列表。源码允许很大的来源描述上限，这说明“工具数量少”本身并不证明提示词足够短；我们应测量完整请求、来源摘要与历史中的定义体积，不能照搬其阈值。

## 4. “发现”和“加载”在原生协议中的含义

ToolSearchOutput 实现专用响应历史类型，包含 call_id、completed 状态、client 执行标记和完整定义。集成测试检查初始请求不暴露相关 app 工具；搜索后的第二、第三次请求依靠历史中的定义，普通 tools 列表仍没有这些 app 工具。

因此，这条路径不需要以“本地已加载集合 → 每轮重写 tools 数组”实现。运行时的可调用判断也没有要求先逐一记录某个工具已被搜索：当搜索入口可见，目录中符合延迟暴露条件的工具便可满足该可调用判断。具体执行仍经过注册表与既有策略链路；搜索不是新增权限。

历史整理代码需要维护搜索调用／结果对应关系，压缩可能裁剪结果。不要据此声称搜索定义永久保留，或者直接推导成我们需要实现一套 TTL／LRU／显式卸载系统。

- [专用历史结果类型](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/core/src/tools/context.rs:262)。
- [明确禁止搜索后普通工具注入的测试断言](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/core/tests/suite/search_tool.rs:748)。
- [本步可调用工具判断](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/core/src/tools/router.rs:199)。
- [历史对应关系整理](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/core/src/context_manager/normalize.rs)。
- [远程压缩裁剪搜索结果的测试](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/core/tests/suite/compact_remote_trimming.rs:89)。

## 5. 编辑工具：不同契约，不能只复制宽松匹配

本版本 apply_patch 是带 Lark grammar 的 freeform 工具，明确要求输入补丁原文而非 JSON。采用 Begin Patch／Update File 等专用语法；它不是普通 old/new 精确替换，也不是 CodeAgent 当前 JSON 包装的标准 unified diff。

定位先依照补丁上下文和递增行游标，再尝试精确、忽略行尾空白、忽略行首尾空白、归一化部分 Unicode 排版字符。找不到预期行时返回具体上下文。与本次所读另外两家相比，Codex 的该匹配器对行首缩进更宽松。

它是从当前游标向后寻找匹配序列，不提供全文件唯一匹配保证。不能把 trim fallback 单独移植到字符串替换工具中，以为只会减少失败而不会误改。需要连同上下文约束、重复片段用例、换行保留、变更展示和验证一起评估。补丁执行也不能被概括成整个多文件修改的原子事务：处理代码会在失败时保留已提交 delta，供后续记录。

解析／验证失败走 RespondToModel，给模型修正机会；权限与沙箱检查仍存在。相比给弱模型同时提供多种编辑语法，更适合先选择一种经过验证的简单契约。

- [freeform 与 grammar 契约](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/core/src/tools/handlers/apply_patch_spec.rs:9)。
- [补丁语法](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/core/assets/tools/apply_patch.lark)。
- [匹配阶梯及其测试](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/apply-patch/src/seek_sequence.rs:1)。
- [上下文、游标与换行保留](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/apply-patch/src/file_update.rs:60)。
- [验证错误与权限检查](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/core/src/tools/handlers/apply_patch.rs:478)。
- [失败时保留已提交变更信息](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/core/src/tools/handlers/apply_patch.rs:614)。

## 6. 参数、结果与命令

已检查的普通参数解析路径用 serde_json 反序列化为具体 Rust 类型，解析失败返回 failed to parse function arguments 加原因。这里没有出现 OpenCode 那样的通用结构修复插件；这只是所读链路的事实，不等于证明整个仓库没有任何专用兼容处理。

不要把类型反序列化等同于所有工具都拒绝额外字段。例如终端参数没有统一 deny_unknown_fields，工作目录还通过另一个参数结构单独提取。我们应修正本项目自己的 schema／执行契约，不能宣称主流项目全都使用同一种最严格校验。

注册表的指标与完成状态依据 ToolOutput.success_for_logging，而不只依据 Rust Result 是否为 Ok。但普通 FunctionToolOutput 在 success 未提供时默认 true，因此仍依赖具体处理器正确标记业务失败；不能声称它自动解决了所有“文字失败、状态成功”的问题。

终端区分 yield_time_ms 与 timeout_ms：等待本次返回的时间不等于命令生命周期超时，运行中的会话另有 session_id 和 write_stdin 入口。这能避免把“工具等待结束”与“命令执行失败”混为一谈；本轮未验证进程跨服务重启恢复，不能拿这部分推断后台持久化方案。

- [可恢复的类型解析错误](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/core/src/tools/handlers/mod.rs:92)。
- [终端参数与等待时间](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/core/src/tools/handlers/unified_exec.rs:63)。
- [成功状态参与日志与生命周期](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/core/src/tools/registry.rs:920)。
- [普通结果缺省成功的实际边界](/Users/xiaoyuan/Documents/work/git/codex/codex-rs/core/src/tools/context.rs:340)。

## 7. 三家共同点与分歧

| 维度 | DeepSeek Harness | OpenCode | Codex |
| --- | --- | --- | --- |
| 范围控制 | preset、scope、条件解析 | 原生核心、权限、模型 hook | 模型能力、策略、exposure |
| 扩展发现 | 所读标准链路未见通用搜索 | Code Mode 内本地搜索签名 | 原生 BM25 + 专用搜索结果；另有 Code Mode |
| 搜索后定义 | 不适用 | 不追加原生 tools；inventory 已有执行实现 | 原生结果进入历史，不追加普通 tools |
| 所有模型统一机制 | 否 | 否 | 否 |
| 编辑契约 | 常规必填 old/new | 按模型切换 edit/write 与 patch | 能力控制的 freeform patch |
| 定位容错 | 常规精确、换行归一 | 排版字符、行尾空白 | 上下文序列、首尾空白、排版字符 |
| 参数恢复 | 明确校验错误 | 有限结构修复再校验 | 所读通用路径类型解析并返回错误 |
| 额外任务分类模型 | 所读链路无此必要 | 所读链路无此必要 | 所读链路无此必要 |

共同方向是小而明确的能力入口、受控范围、模型适配与可恢复错误；不是共同采用一种加载状态机。Code Mode、原生延迟发现、普通 function calling 各有协议和模型前提，不能只比较最外层工具个数。

## 8. 对 CodeAgent 的收敛建议

此前服务端发现设计继续保留为候选稿，本报告不修改运行逻辑。三家证据足以支持以下实施顺序，但不足以承诺具体错误率下降幅度：

1. **先修契约和观测。** 必填编辑内容、缺参拒绝、未知字段诊断、说明一致性、拒绝／业务失败状态统一。读取显示与可复制原文分开；编辑失败返回有限且权限允许的原文提示。不要通过自动丢弃错字段掩盖内容缺失。
2. **再缩减编码常用集合。** 提供显式 coding profile，直接保留检查、单一编辑、命令及必需控制能力；压缩重复工具说明，扩展能力另行发现。先避免常见编辑和命令每次都多走一轮搜索。确切常驻清单由当前能力覆盖与重放结果决定，不复制别人的固定数量。
3. **扩展发现按协议适配。** 目录与执行实现分离。已验证支持原生搜索的接口可以走专用协议；通用 function calling 若采用发现，应在下一次请求明确提供选中工具的 schema。普通文本不是原生装载协议。第一版不引入完整 Code Mode 解释器、额外分类模型或显式 tool_release／TTL／LRU。
4. **以固定任务、固定实际模型比较。** 分开统计参数校验、名称／路径、编辑定位、权限拒绝、超时、进程失败和传输故障；记录请求工具体积、首次成功率、同因重试、任务成功率、调用轮数与成本。免费路由须记录实际模型，避免用一个变化路由名称作模型能力结论。

通用发现适配仍需要明确的请求可见集合和最少状态；不应为了“无状态”放弃本项目现有 advertised 执行边界。是否跨 Turn 保留选中工具、如何恢复，应在基本路径评测后单独决定，不先建设完整缓存生命周期系统。

9.3% 的混合失败／拒绝率不能直接与主流 Agent 排名比较：本轮源码研究没有提供同任务、同模型的对照数据。用户看到更弱模型接近必失败，足以优先排查供应商工具协议、schema 兼容性及缺参分布；不能直接归因为工具多，也不能据此断言全部来自模型。最终应选择使任务完成更可靠且总成本更低的最小改动。

相关报告：[DeepSeek Harness](/Users/xiaoyuan/Documents/work/git/code-agent/docs/tool-research-deepseek-harness.md)、[OpenCode](/Users/xiaoyuan/Documents/work/git/code-agent/docs/tool-research-opencode.md)、[先前服务端设计候选稿](/Users/xiaoyuan/Documents/work/git/code-agent/docs/server-tool-discovery-design.md)。
