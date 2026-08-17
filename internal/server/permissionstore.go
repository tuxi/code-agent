package server

import (
	"errors"
	"os"

	"code-agent/internal/approve"
	"code-agent/internal/settings"
)

// PermissionStore is the default PermissionService: it reads the effective
// approval tier through the settings merge (user → project shared → local,
// per-workspace file wins) and writes it to the workspace's settings.local.json
// via the canonical atomic writer. It is constructed by the assembler, which
// supplies the home directory (desktop) — the server layer stays a dumb pipe.
//
// EffectiveMode uses settings.Load so a workspace's local file overrides the
// user file; the shared settings.json also participates in the read (a team
// could default a tier), but SetMode never writes there — the tier is a
// machine-local preference like an interactive grant.
type PermissionStore struct {
	home string
}

// NewPermissionStore builds the store for the given home directory (used for
// the user-scope fallback file). An empty home disables the fallback.
func NewPermissionStore(home string) *PermissionStore {
	return &PermissionStore{home: home}
}

// EffectiveMode implements PermissionService.
func (s *PermissionStore) EffectiveMode(root string) (string, error) {
	if root == "" {
		return string(approve.ModeAsk), nil
	}
	merged := settings.Load(root, s.home, os.Stderr)
	return string(approve.ModeFromSettings(merged)), nil
}

// SetMode implements PermissionService. It persists to the workspace's
// settings.local.json; the change takes effect at the next turn boundary (the
// serve builder re-reads the mode from disk per turn).
func (s *PermissionStore) SetMode(root, mode string) error {
	if root == "" {
		return errors.New("workspace path required")
	}
	path := settings.ProjectLocalPath(root)
	if path == "" {
		return errors.New("workspace path required")
	}
	return settings.SetApprovalMode(path, mode)
}
