//go:build unix

package mcpbridge

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func readPrivateConnection(path string) ([]byte, error) {
	denied := errors.New("connection manifest requires a canonical absolute path, an owned 0700 directory, and an owned regular 0600 file")
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, denied
	}
	dir := filepath.Dir(path)
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil || canonical != dir {
		return nil, denied
	}
	dfd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, denied
	}
	defer unix.Close(dfd)
	var ds unix.Stat_t
	if unix.Fstat(dfd, &ds) != nil || ds.Uid != uint32(os.Geteuid()) || ds.Mode&07777 != 0700 {
		return nil, denied
	}
	fd, err := unix.Openat(dfd, filepath.Base(path), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, denied
	}
	f := os.NewFile(uintptr(fd), "connection manifest")
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&07777 != 0600 || st.Nlink != 1 || st.Size > 16384 {
		return nil, denied
	}
	b, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(b) > 16384 {
		return nil, denied
	}
	return b, nil
}

func connectionProcessAlive(pid int) bool { return unix.Kill(pid, 0) == nil }
