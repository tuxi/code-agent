# Code Agent

CodeAgent is an AI-native coding agent runtime.

The LLM decides what to do next.
The runtime controls what can actually be executed.
Tools are explicit, typed, observable capabilities.
Every step must be traceable.
No hidden automation.
No uncontrolled file modification.
No database before the basic agent loop works.

中文理解：
模型负责决策；
Runtime 负责边界；
工具负责执行；
Trace 负责可观察；
人类负责最终确认。
拥有会呼吸的 Agent 心脏：Goal → Model Decision → Tool Execution → Observation → Next Decision → Final