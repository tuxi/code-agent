package runtime

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

const serverStateFilename = "runtime-server-state.json"

// ServerState is non-secret durable Runtime metadata. It lives beside the
// session data source so a restart or listener-port change preserves identity,
// while a genuinely different data directory gets a different server_id.
type ServerState struct {
	ServerID           string `json:"server_id"`
	CatalogRevision    int64  `json:"catalog_revision"`
	CatalogFingerprint string `json:"catalog_fingerprint"`
}

// RuntimeStateDir returns the durable metadata directory for the same data
// source OpenStore(root) uses.
func RuntimeStateDir(root string) (string, error) {
	path, err := storePath(root)
	if err != nil {
		return "", err
	}
	return filepath.Dir(path), nil
}

// LoadOrCreateServerState returns the stable server identity and advances the
// model-catalog revision if its safe DTO fingerprint changed. A corrupt existing
// state is an error: silently replacing server_id would make clients bind an
// existing endpoint to a different Runtime identity.
func LoadOrCreateServerState(root string, catalogJSON []byte) (ServerState, error) {
	dir, err := RuntimeStateDir(root)
	if err != nil {
		return ServerState{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ServerState{}, fmt.Errorf("create runtime state directory: %w", err)
	}
	path := filepath.Join(dir, serverStateFilename)
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(catalogJSON))

	var state ServerState
	data, readErr := os.ReadFile(path)
	switch {
	case readErr == nil:
		if err := json.Unmarshal(data, &state); err != nil {
			return ServerState{}, fmt.Errorf("decode runtime server state: %w", err)
		}
		if state.ServerID == "" {
			return ServerState{}, errors.New("runtime server state has empty server_id")
		}
	case errors.Is(readErr, os.ErrNotExist):
		state.ServerID = "srv_" + uuid.NewString()
	default:
		return ServerState{}, fmt.Errorf("read runtime server state: %w", readErr)
	}

	if state.CatalogRevision < 1 || state.CatalogFingerprint != fingerprint {
		state.CatalogRevision++
		if state.CatalogRevision < 1 {
			state.CatalogRevision = 1
		}
		state.CatalogFingerprint = fingerprint
	}
	if err := writeServerState(path, state); err != nil {
		return ServerState{}, err
	}
	return state, nil
}

func writeServerState(path string, state ServerState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write runtime server state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit runtime server state: %w", err)
	}
	return nil
}
