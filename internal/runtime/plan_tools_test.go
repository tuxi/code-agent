package runtime

import "testing"

func TestPlanModeIncludesIsolatedTaskButSubagentCannotRecurse(t *testing.T) {
	if !containsToolName(PlanModeToolNames, "task") {
		t.Fatal("plan mode must expose task for independent discovery and critique")
	}
	if containsToolName(ReadOnlyToolNames, "task") {
		t.Fatal("subagent toolset must not contain task recursively")
	}
}

func containsToolName(names []string, target string) bool {
	for _, name := range names {
		if name == target {
			return true
		}
	}
	return false
}
