package preview

import (
	"testing"
	"time"
)

func presentationArtifact(status Status, fallback bool) Artifact {
	artifact := Artifact{Status: status, Kind: KindInteractiveHTML}
	if fallback {
		artifact.Assets = []Asset{{AssetID: "asset_fallback", Role: RoleFallback, MIMEType: "image/png", SHA256: "sha256:" + "0000000000000000000000000000000000000000000000000000000000000000"}}
		artifact.Fallback = &Fallback{AssetID: "asset_fallback", Text: "fallback"}
	}
	return artifact
}

func TestPresentationPrecedence(t *testing.T) {
	interactive := Negotiate(FullRuntimeFlags().List(), FullRuntimeFlags().List(), false)
	cases := []struct {
		name  string
		input PresentationInput
		want  PresentationState
	}{
		{"failed fallback", PresentationInput{Artifact: presentationArtifact(StatusFailed, true), Capabilities: interactive, Now: time.Now()}, StateStaticFallback},
		{"failed no fallback", PresentationInput{Artifact: presentationArtifact(StatusFailed, false), Capabilities: interactive, Now: time.Now()}, StateUnavailable},
		{"security no fallback", PresentationInput{Artifact: presentationArtifact(StatusReady, false), Capabilities: interactive, Now: time.Now(), SecurityViolation: true}, StateUnavailable},
		{"fetch failure no fallback", PresentationInput{Artifact: presentationArtifact(StatusReady, false), Capabilities: interactive, Now: time.Now(), ViewerFailure: &Error{Code: CodeViewerFetchFailed}}, StateRenderFailed},
		{"interactive corruption", PresentationInput{Artifact: presentationArtifact(StatusReady, false), Capabilities: interactive, Now: time.Now()}, StateUnavailable},
	}
	expired := presentationArtifact(StatusReady, true)
	expires := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	expired.ExpiresAt = &expires
	cases = append(cases, struct {
		name  string
		input PresentationInput
		want  PresentationState
	}{"expired fallback", PresentationInput{Artifact: expired, Capabilities: interactive, Now: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)}, StateExpired})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DerivePresentation(tc.input); got != tc.want {
				t.Fatalf("presentation = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestInvalidRuntimeInteractiveAdvertisementDegradesToStaticFallback(t *testing.T) {
	runtimeCaps := []string{CapabilityContract, CapabilityInteractive}
	clientCaps := []string{CapabilityContract, CapabilityStatic}
	negotiated := Negotiate(runtimeCaps, clientCaps, false)
	if !negotiated.InvalidRuntime || negotiated.Effective.Interactive || !negotiated.Effective.Static {
		t.Fatalf("negotiated capabilities = %+v", negotiated)
	}
	if negotiated.LiveMode != ModeDegradedInvalidRuntime {
		t.Fatalf("live mode = %s", negotiated.LiveMode)
	}
	artifact := presentationArtifact(StatusReady, true)
	artifact.Viewer = &Viewer{URL: "/viewer", MIMEType: ViewerMIMEType, SizeBytes: 1, SHA256: "sha256:" + "0000000000000000000000000000000000000000000000000000000000000000"}
	got := DerivePresentation(PresentationInput{
		Artifact: artifact, Capabilities: negotiated, Now: time.Now(),
	})
	if got != StateStaticFallback {
		t.Fatalf("presentation = %s, want %s", got, StateStaticFallback)
	}
}

func TestRuntimeWithoutContractCannotDegradeToStatic(t *testing.T) {
	negotiated := Negotiate(
		[]string{CapabilityInteractive},
		[]string{CapabilityContract, CapabilityStatic},
		false,
	)
	if negotiated.Effective.Contract || negotiated.Effective.Static || negotiated.Effective.Interactive {
		t.Fatalf("effective capabilities = %+v", negotiated.Effective)
	}
	if negotiated.LiveMode != ModeUnavailable {
		t.Fatalf("live mode = %s, want %s", negotiated.LiveMode, ModeUnavailable)
	}
	artifact := presentationArtifact(StatusReady, true)
	artifact.Viewer = &Viewer{URL: "/viewer", MIMEType: ViewerMIMEType, SizeBytes: 1, SHA256: "sha256:" + "0000000000000000000000000000000000000000000000000000000000000000"}
	got := DerivePresentation(PresentationInput{
		Artifact: artifact, Capabilities: negotiated, Now: time.Now(),
	})
	if got != StateUnavailable {
		t.Fatalf("presentation = %s, want %s", got, StateUnavailable)
	}
}
