package settings

import "testing"

func TestCodingToolsLayerCanExplicitlyRestoreFullSet(t *testing.T) {
	on, off := true, false
	base := Settings{Agent: AgentConfig{CodingTools: &on}}
	MergeSettings(&base, Settings{})
	if base.Agent.CodingTools == nil || !*base.Agent.CodingTools {
		t.Fatal("unset overlay lost coding profile")
	}
	MergeSettings(&base, Settings{Agent: AgentConfig{CodingTools: &off}})
	if base.Agent.CodingTools == nil || *base.Agent.CodingTools {
		t.Fatal("explicit false failed to restore full profile")
	}
}
