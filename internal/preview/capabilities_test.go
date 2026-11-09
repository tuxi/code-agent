package preview

import (
	"reflect"
	"testing"
)

func TestReconcileAdvertisedUsesRuntimeIntersection(t *testing.T) {
	got, mismatch := ReconcileAdvertised([]string{"streaming", CapabilityInteractive, CapabilityReplay, "preview_unknown_v1"}, RuntimeFlags{})
	if !mismatch {
		t.Fatal("stale preview capabilities were not reported as a mismatch")
	}
	want := []string{"streaming", "preview_unknown_v1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reconciled capabilities = %v, want %v", got, want)
	}

	got, mismatch = ReconcileAdvertised([]string{"streaming"}, FullRuntimeFlags())
	if mismatch {
		t.Fatal("valid runtime preview flags reported a mismatch")
	}
	want = []string{"streaming", CapabilityContract, CapabilityInteractive, CapabilityStatic, CapabilityReplay}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("full preview capabilities = %v, want %v", got, want)
	}
}

func TestInteractiveAdvertisementRequiresStatic(t *testing.T) {
	invalidRuntime := []string{CapabilityContract, CapabilityInteractive}
	staticClient := []string{CapabilityContract, CapabilityStatic}
	got := Negotiate(invalidRuntime, staticClient, false)
	if !got.InvalidRuntime || got.Effective.Interactive || got.LiveMode != ModeDegradedInvalidRuntime {
		t.Fatalf("invalid runtime negotiation = %+v", got)
	}
}
