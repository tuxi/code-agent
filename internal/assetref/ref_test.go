package assets

import "testing"

func TestWorkspaceURIPercentEncodesPathSegments(t *testing.T) {
	got := WorkspaceURI("agentkit-local", "Sources/你好 world/A#B?.swift", &Range{StartLine: 7})
	want := "workspace://agentkit-local/Sources/%E4%BD%A0%E5%A5%BD%20world/A%23B%3F.swift#L7"
	if got != want {
		t.Fatalf("WorkspaceURI = %q, want %q", got, want)
	}
}

func TestStableIDIsDeterministic(t *testing.T) {
	a := StableID("turn_1", "call_2", 1, "file", "a.go", "7")
	b := StableID("turn_1", "call_2", 1, "file", "a.go", "7")
	if a != b {
		t.Fatalf("StableID not deterministic: %q vs %q", a, b)
	}
}
