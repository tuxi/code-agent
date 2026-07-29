// Package buildinfo is the single source of truth for the Runtime version and
// Agent Wire compatibility advertised over HTTP and WebSocket.
package buildinfo

const (
	Product           = "codeagent"
	AgentWireMajor    = 1
	AgentWireRevision = "1.2"
)

// Version is replaced at build time with:
//
//	-ldflags "-X code-agent/internal/buildinfo.Version=<semver>"
//
// Development builds deliberately report "dev" rather than a model name or an
// invented release version.
var Version = "dev"

func ServerName() string { return Product + "/" + Version }
