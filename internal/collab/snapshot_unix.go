//go:build unix

package collab

import (
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func readReviewFile(root, path string) (FileImage, error) {
	image := FileImage{Path: path}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return image, err
	}
	parts := strings.Split(path, "/")
	for _, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if missingReviewFile(err) {
			return image, nil
		}
		if err != nil {
			return image, ErrForbidden
		}
		fd = next
	}
	defer unix.Close(fd)
	fileFD, err := unix.Openat(fd, parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if missingReviewFile(err) {
		return image, nil
	}
	if err != nil {
		return image, ErrForbidden
	}
	f := os.NewFile(uintptr(fileFD), "review source")
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() > maxFileBytes {
		return image, ErrInvalid
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return image, err
	}
	after, err := f.Stat()
	if err != nil || len(data) > maxFileBytes || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return image, ErrConflict
	}
	image.Exists = true
	image.Mode = uint32(before.Mode().Perm())
	image.Data = data
	return image, nil
}
