//go:build unix

package settings

import (
	"os"
	"syscall"
)

// flockExclusive takes an exclusive advisory lock on f. Advisory flock(2)
// coordinates writers that all honor it — every settings write goes through
// Persist, so the lock is respected by CLI agents and the server process alike.
func flockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// flockUnlock releases the lock. Best-effort: the lock is also released when f
// is closed, so a failure here is harmless.
func flockUnlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
