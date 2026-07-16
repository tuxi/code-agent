package sandbox

import "testing"

func TestHasDangerousAssignment(t *testing.T) {
	tests := []struct {
		command       string
		wantVar       string
		wantDangerous bool
	}{
		{"PATH=/tmp go build", "PATH", true},
		{"LD_PRELOAD=/evil.so ./app", "LD_PRELOAD", true},
		{"DYLD_INSERT_LIBRARIES=evil.dylib go build", "DYLD_INSERT_LIBRARIES", true},
		{"BASH_ENV=/tmp/evil.sh bash script.sh", "BASH_ENV", true},
		{"NPM_CONFIG_REGISTRY=http://evil npm install", "NPM_CONFIG_REGISTRY", true},
		// Safe assignments
		{"GOOS=linux go build", "", false},
		{"GOARCH=arm64 go build", "", false},
		{"go build", "", false},
		// Not an assignment
		{"git status", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			v, dangerous := hasDangerousAssignment(tt.command)
			if dangerous != tt.wantDangerous {
				t.Errorf("hasDangerousAssignment(%q) = (%q, %v), want (%q, %v)",
					tt.command, v, dangerous, tt.wantVar, tt.wantDangerous)
			}
			if dangerous && v != tt.wantVar {
				t.Errorf("var name = %q, want %q", v, tt.wantVar)
			}
		})
	}
}

func TestDangerousVarEscalates(t *testing.T) {
	policy := DefaultPolicy()

	tests := []struct {
		command string
		want    Decision
		desc    string
	}{
		// Dangerous vars escalate Allow → Confirm
		{"PATH=/tmp go build", Confirm, "PATH override"},
		{"LD_PRELOAD=/x go test", Confirm, "LD_PRELOAD"},
		{"BASH_ENV=/x bash script.sh", Confirm, "BASH_ENV"},
		// Already Confirm stays Confirm
		{"PATH=/tmp git push", Confirm, "PATH + git push"},
		// Safe vars stay Allow
		{"GOOS=linux go build", Allow, "GOOS safe"},
		{"go build", Allow, "no var"},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			got := policy.Classify(tt.command)
			if got.Decision != tt.want {
				t.Errorf("Classify(%q).Decision = %v, want %v (reason: %s)",
					tt.command, got.Decision, tt.want, got.Reason)
			}
		})
	}
}
