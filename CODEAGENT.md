CodeAgent is an AI-native coding agent runtime.

The LLM decides what to do next.
The runtime controls what can actually be executed.
Tools are explicit, typed, observable capabilities.
Every step must be traceable.
No hidden automation.
No uncontrolled file modification.
No database before the basic agent loop works.

For code changes:
1. Inspect before editing.
2. Plan before complex changes.
3. Ask user when requirements are ambiguous.
4. Propose patch before applying.
5. Never apply patches silently.
6. Show git diff after applying changes.
7. Validate with tests when command execution is available.