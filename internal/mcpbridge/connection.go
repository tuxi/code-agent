package mcpbridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// connectionManifest is secret-bearing input only. Never expose it as MCP output
// or include decoding errors (which can quote input) in diagnostics.
type connectionManifest struct {
	SchemaVersion int    `json:"schema_version"`
	Endpoint      string `json:"endpoint"`
	ServerID      string `json:"server_id"`
	StoreID       string `json:"store_id,omitempty"`
	InstanceID    string `json:"instance_id"`
	DaemonPID     int    `json:"daemon_pid"`
	ExpiresAt     string `json:"expires_at"`
	AccessToken   string `json:"access_token"`
}

type connectionBinding struct {
	path             string
	digest           [32]byte
	verifiedServerID string
	verifiedStoreID  string // nonempty only after server and store identity checks
}

// ConnectionInfo deliberately contains no credential or model configuration.
type ConnectionInfo struct {
	Endpoint         string `json:"endpoint"`
	ServerID         string `json:"server_id"`
	StoreID          string `json:"store_id,omitempty"`
	IdentityVerified bool   `json:"identity_verified"`
}

func loadConnection(path string) (connectionManifest, [32]byte, error) {
	var m connectionManifest
	b, err := readPrivateConnection(path)
	if err != nil {
		return m, [32]byte{}, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&m) != nil || d.Decode(new(any)) != io.EOF {
		return connectionManifest{}, [32]byte{}, errors.New("invalid connection manifest")
	}
	expires, err := time.Parse(time.RFC3339, m.ExpiresAt)
	if err != nil || !expires.After(time.Now()) || expires.After(time.Now().Add(24*time.Hour)) || m.SchemaVersion != 1 || !validID(m.ServerID) || !validID(m.InstanceID) || (m.StoreID != "" && !validID(m.StoreID)) || m.DaemonPID <= 0 || !connectionProcessAlive(m.DaemonPID) || len(m.AccessToken) < 32 || len(m.AccessToken) > 4096 || bytes.ContainsAny([]byte(m.AccessToken), "\r\n\t ") {
		return connectionManifest{}, [32]byte{}, errors.New("connection manifest is invalid, expired, or its daemon has stopped")
	}
	if _, err := parseEndpoint(m.Endpoint); err != nil {
		return connectionManifest{}, [32]byte{}, err
	}
	return m, sha256.Sum256(b), nil
}

// NewFromManifest connects to an explicitly granted existing instance. It never
// starts a daemon, creates a session, selects a model, or submits a turn.
func NewFromManifest(ctx context.Context, path, workspace string) (*Client, ConnectionInfo, error) {
	return newFromManifest(ctx, path, workspace, "", "")
}

func newFromManifest(ctx context.Context, path, workspace, baseline, delivery string) (*Client, ConnectionInfo, error) {
	m, digest, err := loadConnection(path)
	if err != nil {
		return nil, ConnectionInfo{}, fmt.Errorf("connection_grant: %w", err)
	}
	c, err := newClient(ctx, m.Endpoint, m.AccessToken, workspace, baseline, delivery)
	if err != nil {
		return nil, ConnectionInfo{}, fmt.Errorf("workspace_binding: %w", err)
	}
	c.binding = &connectionBinding{path: path, digest: digest}
	var runtime struct {
		ServerID string `json:"server_id"`
	}
	if err = c.request(ctx, "GET", "/v1/runtime/info", nil, &runtime); err == nil && runtime.ServerID != m.ServerID {
		err = errors.New("runtime identity does not match the granted instance")
	}
	if err == nil && m.StoreID != "" {
		var store struct {
			ServerID string `json:"server_id"`
			StoreID  string `json:"store_id"`
		}
		err = c.request(ctx, "GET", "/v1/tasks/capabilities", nil, &store)
		if err == nil && (store.ServerID != m.ServerID || store.StoreID != m.StoreID) {
			err = errors.New("task store identity does not match the granted instance")
		}
	}
	if err != nil {
		c.Close()
		return nil, ConnectionInfo{}, fmt.Errorf("runtime_identity: %w", err)
	}
	c.binding.verifiedStoreID = m.StoreID
	c.binding.verifiedServerID = m.ServerID
	go c.watchBinding()
	return c, ConnectionInfo{Endpoint: m.Endpoint, ServerID: m.ServerID, StoreID: m.StoreID, IdentityVerified: true}, nil
}

func (c *Client) checkBinding() error {
	if c.binding == nil {
		return nil
	}
	if c.ctx.Err() != nil {
		return errors.New("local MCP connection is closed; authorize a new connection")
	}
	_, digest, err := loadConnection(c.binding.path)
	if err != nil || digest != c.binding.digest {
		return errors.New("local MCP grant was revoked, replaced, expired, or its daemon stopped; authorize a new connection")
	}
	return nil
}

// Revocation closes this bridge's observation/control channels only. It does
// not cancel execution in the daemon and does not follow a replacement grant.
func (c *Client) watchBinding() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			if c.checkBinding() != nil {
				c.Close()
				return
			}
		}
	}
}
