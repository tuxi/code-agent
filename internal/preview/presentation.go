package preview

import "time"

type PresentationState string

const (
	StateLoading          PresentationState = "loading"
	StateInteractiveReady PresentationState = "interactiveReady"
	StateStaticFallback   PresentationState = "staticFallback"
	StateUnavailable      PresentationState = "unavailable"
	StateRenderFailed     PresentationState = "renderFailed"
	StateExpired          PresentationState = "expired"
)

type PresentationInput struct {
	Artifact          Artifact
	Capabilities      NegotiatedCapabilities
	Historical        bool
	Now               time.Time
	ViewerFailure     *Error
	SecurityViolation bool
}

func DerivePresentation(in PresentationInput) PresentationState {
	artifact := in.Artifact
	fallback := hasFallback(artifact)
	if artifact.Expired(in.Now) {
		if fallback {
			return StateExpired
		}
		return StateUnavailable
	}
	if in.SecurityViolation {
		if fallback {
			return StateStaticFallback
		}
		return StateUnavailable
	}
	if in.ViewerFailure != nil {
		switch in.ViewerFailure.Code {
		case CodeViewerTooLarge, CodeViewerMIMEMismatch, CodeDigestMismatch, CodeViewerFetchFailed, CodeInvalidArtifact:
			if fallback {
				return StateStaticFallback
			}
			return StateRenderFailed
		case CodeSecurityViolation:
			if fallback {
				return StateStaticFallback
			}
			return StateUnavailable
		}
	}
	switch artifact.Status {
	case StatusPending:
		return StateLoading
	case StatusFailed:
		if fallback {
			return StateStaticFallback
		}
		return StateUnavailable
	case StatusUnavailable:
		if fallback {
			return StateStaticFallback
		}
		return StateUnavailable
	case StatusReady:
		switch artifact.Kind {
		case KindStaticImage:
			return StateStaticFallback
		case KindInteractiveHTML:
			if artifact.Viewer == nil || artifact.Fallback == nil {
				return StateUnavailable
			}
			if in.Historical && !in.Capabilities.Effective.Replay {
				if fallback {
					return StateStaticFallback
				}
				return StateUnavailable
			}
			if in.Capabilities.Effective.Interactive {
				return StateInteractiveReady
			}
			if in.Capabilities.Effective.Static && fallback {
				return StateStaticFallback
			}
			return StateUnavailable
		}
	}
	return StateUnavailable
}

func hasFallback(artifact Artifact) bool {
	if artifact.Fallback == nil {
		return false
	}
	asset, ok := artifact.FindAsset(artifact.Fallback.AssetID)
	return ok && asset.Role == RoleFallback
}
