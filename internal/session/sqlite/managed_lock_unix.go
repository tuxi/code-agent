//go:build unix

package sqlite

import (
	"context"
	"os"
	"sync"

	"code-agent/internal/collab"
	"golang.org/x/sys/unix"
)

func (r *taskRepository) LockManagedHost(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fd, err := unix.Open(r.store.path+".managed.lock", unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "managed runtime lock")
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, collab.ErrManagedWorkspace
	}
	var once sync.Once
	return func() { once.Do(func() { _ = unix.Flock(fd, unix.LOCK_UN); _ = f.Close() }) }, nil
}
