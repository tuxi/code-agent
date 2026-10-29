//go:build !unix

package collab

import "os"

func fileIdentity(os.FileInfo) (string, error) { return "", ErrUnavailable }
