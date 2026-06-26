package prompt

// PlanningPrompts holds prompt fragments related to plan mode. These are injected
// as ephemeral user messages during the planning phase — they never live in the
// persistent system prompt, so the system prompt stays small and the planning
// guidance only appears when the model is actually in plan mode.
//
// The main system prompt (system.go) contains only a brief 2-line nudge to use
// enter_plan_mode for complex tasks. The detailed guidance lives here.
const PlanningPrompts = ""
