package settings

import "testing"

func TestBackgroundJobNotificationsMergeExplicitFalse(t *testing.T) {
	enabled, disabled := true, false
	base := Settings{Agent: AgentConfig{BackgroundJobNotifications: &enabled}}
	MergeSettings(&base, Settings{})
	if base.Agent.BackgroundJobNotifications == nil || !*base.Agent.BackgroundJobNotifications {
		t.Fatal("missing overlay changed base")
	}
	MergeSettings(&base, Settings{Agent: AgentConfig{BackgroundJobNotifications: &disabled}})
	if base.Agent.BackgroundJobNotifications == nil || *base.Agent.BackgroundJobNotifications {
		t.Fatal("explicit false did not override")
	}
	if (Settings{}).Agent.BackgroundJobNotifications != nil {
		t.Fatal("feature enabled by default")
	}
}
