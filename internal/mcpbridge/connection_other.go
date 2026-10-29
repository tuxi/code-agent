//go:build !unix

package mcpbridge

import "errors"

func readPrivateConnection(string) ([]byte, error) {
	return nil, errors.New("local connection manifests require a Unix host")
}

func connectionProcessAlive(int) bool { return false }
