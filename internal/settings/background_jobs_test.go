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
		t.Fatal("default must not overwrite an unset layer")
	}
}

func TestBackgroundJobNotificationsDefaultAndOverrides(t *testing.T) {
	enabled, disabled := true, false
	for _, tc := range []struct {
		name            string
		global, project *bool
		want            bool
	}{
		{"default", nil, nil, true},
		{"global off", &disabled, nil, false},
		{"global on", &enabled, nil, true},
		{"project off", &enabled, &disabled, false},
		{"project on", &disabled, &enabled, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := Settings{Agent: AgentConfig{BackgroundJobNotifications: tc.global}}
			MergeSettings(&base, Settings{Agent: AgentConfig{BackgroundJobNotifications: tc.project}})
			if got := base.Agent.BackgroundJobNotificationsEnabled(); got != tc.want {
				t.Fatalf("enabled = %v, want %v", got, tc.want)
			}
		})
	}
}
