package runtime

import (
	"fmt"
	"path/filepath"

	"code-agent/internal/preview"
)

// OpenPreviewService opens the immutable preview store beside the project's
// session database. The store is local to this workspace state directory and is
// shared by the daemon, embedded runtime, CLI, and TUI.
func OpenPreviewService(root string) (*preview.Service, error) {
	path, err := storePath(root)
	if err != nil {
		return nil, fmt.Errorf("resolve preview store path: %w", err)
	}
	store, err := preview.NewFSStore(filepath.Join(filepath.Dir(path), "previews"))
	if err != nil {
		return nil, err
	}
	return preview.NewService(store), nil
}
