//go:build !darwin && !linux

package repos

import (
	"errors"
	"os"
)

// The supported AgentKit targets use kernel no-replace rename primitives. This
// fallback keeps other Go targets buildable and fails closed on a visible target.
func renameNoReplace(from, to string) error {
	if _, err := os.Lstat(to); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(from, to)
}
