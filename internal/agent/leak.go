package agent

import "strings"

// LooksLikeToolCallLeak reports whether s is a model's tool-call markup leaked as
// plain text rather than a real answer. It happens when a provider is asked to
// answer with no tools but the model still "wants" to call one — deepseek emits
// its native DSML markup into the content field (see finalAnswerAfterLimit, the
// only no-tools call in the loop).
//
// The two markers are chosen to be near-zero false positive: "DSML" is deepseek's
// internal marker name, and "<｜" uses fullwidth pipes (U+FF5C) — both essentially
// never appear in normal prose. Deliberately NOT matching the bare word
// "tool_calls", which is common in any discussion of this codebase.
func LooksLikeToolCallLeak(s string) bool {
	return strings.Contains(s, "DSML") || strings.Contains(s, "<｜")
}
