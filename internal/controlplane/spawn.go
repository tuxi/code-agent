package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"code-agent/internal/session"
	"code-agent/internal/tools"
)

type spawnRPCRequest struct {
	Request tools.SessionForkRequest `json:"request"`
	ChildID string                   `json:"child_id"`
}

type spawnReservation struct {
	RequestID       string
	PayloadHash     string
	ParentSessionID string
	ChildSessionID  string
	SourceSessionID string
	Kind            string
	Status          string
}

func (m *Manager) CreateSession(ctx context.Context, request tools.SessionCreateRequest) (tools.SessionSpawnResult, error) {
	if request.RequestID == "" || request.ParentSessionID == "" || request.WorkspacePath == "" {
		return tools.SessionSpawnResult{}, errors.New("control plane: incomplete create_session request")
	}
	owned, err := m.owns(ctx, request.ParentSessionID)
	if err != nil || !owned {
		return tools.SessionSpawnResult{}, offline(request.ParentSessionID, err)
	}
	reservation, err := m.reserveSpawn(ctx, request.RequestID, request.ParentSessionID, "", "spawn", request)
	if err != nil {
		return tools.SessionSpawnResult{}, err
	}
	result, err := m.createOwned(ctx, request, reservation.ChildSessionID)
	if err != nil {
		return tools.SessionSpawnResult{}, err
	}
	if err := m.completeSpawn(ctx, reservation.ChildSessionID); err != nil {
		return tools.SessionSpawnResult{}, err
	}
	if err := m.Heartbeat(context.WithoutCancel(ctx)); err != nil {
		return tools.SessionSpawnResult{}, fmt.Errorf("control plane: created child but owner reconciliation failed: %w", err)
	}
	return result, nil
}

func (m *Manager) ForkSession(ctx context.Context, request tools.SessionForkRequest) (tools.SessionSpawnResult, error) {
	if request.RequestID == "" || request.ParentSessionID == "" || request.SourceSessionID == "" {
		return tools.SessionSpawnResult{}, errors.New("control plane: incomplete fork_session request")
	}
	parentOwned, err := m.owns(ctx, request.ParentSessionID)
	if err != nil || !parentOwned {
		return tools.SessionSpawnResult{}, offline(request.ParentSessionID, err)
	}
	reservation, err := m.reserveSpawn(ctx, request.RequestID, request.ParentSessionID, request.SourceSessionID, "fork", request)
	if err != nil {
		return tools.SessionSpawnResult{}, err
	}
	lease, err := m.resolveLease(ctx, request.SourceSessionID)
	if err != nil {
		return tools.SessionSpawnResult{}, err
	}
	localOwners.RLock()
	owner := localOwners.byInstance[lease.InstanceID]
	localOwners.RUnlock()
	var result tools.SessionSpawnResult
	if owner != nil {
		result, err = owner.forkOwned(ctx, request, reservation.ChildSessionID)
	} else {
		result, err = m.forkRemote(ctx, *lease, request, reservation.ChildSessionID)
	}
	if err != nil {
		return tools.SessionSpawnResult{}, err
	}
	if err := m.completeSpawn(ctx, reservation.ChildSessionID); err != nil {
		return tools.SessionSpawnResult{}, err
	}
	return result, nil
}

func (m *Manager) createOwned(ctx context.Context, request tools.SessionCreateRequest, childID string) (tools.SessionSpawnResult, error) {
	owned, err := m.owns(ctx, request.ParentSessionID)
	if err != nil || !owned {
		return tools.SessionSpawnResult{}, offline(request.ParentSessionID, err)
	}
	m.mu.Lock()
	target := m.target
	m.mu.Unlock()
	if target == nil {
		return tools.SessionSpawnResult{}, offline(request.ParentSessionID, errors.New("target session creator unavailable"))
	}
	return target.CreateSession(ctx, request, childID)
}

func (m *Manager) forkOwned(ctx context.Context, request tools.SessionForkRequest, childID string) (tools.SessionSpawnResult, error) {
	owned, err := m.owns(ctx, request.SourceSessionID)
	if err != nil || !owned {
		return tools.SessionSpawnResult{}, offline(request.SourceSessionID, err)
	}
	m.mu.Lock()
	target := m.target
	m.mu.Unlock()
	if target == nil {
		return tools.SessionSpawnResult{}, offline(request.SourceSessionID, errors.New("target session forker unavailable"))
	}
	result, err := target.ForkSession(ctx, request, childID)
	if err != nil {
		return tools.SessionSpawnResult{}, err
	}
	if err := m.Heartbeat(context.WithoutCancel(ctx)); err != nil {
		return tools.SessionSpawnResult{}, fmt.Errorf("control plane: forked child but owner reconciliation failed: %w", err)
	}
	return result, nil
}

func (m *Manager) forkRemote(ctx context.Context, lease Lease, request tools.SessionForkRequest, childID string) (tools.SessionSpawnResult, error) {
	payload, err := json.Marshal(spawnRPCRequest{Request: request, ChildID: childID})
	if err != nil {
		return tools.SessionSpawnResult{}, err
	}
	endpoint := strings.TrimRight(lease.Endpoint, "/") + ownerSessionPathPrefix + url.PathEscape(request.SourceSessionID) + "/forks"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return tools.SessionSpawnResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+lease.AuthToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return tools.SessionSpawnResult{}, offline(request.SourceSessionID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized {
			return tools.SessionSpawnResult{}, offline(request.SourceSessionID, fmt.Errorf("owner RPC returned %s", resp.Status))
		}
		return tools.SessionSpawnResult{}, fmt.Errorf("control plane: target %s rejected fork: %s", request.SourceSessionID, strings.TrimSpace(string(body)))
	}
	var result tools.SessionSpawnResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return tools.SessionSpawnResult{}, offline(request.SourceSessionID, err)
	}
	if result.ID != childID || result.ParentSessionID != request.ParentSessionID || result.SourceSessionID != request.SourceSessionID || result.Kind != "fork" {
		return tools.SessionSpawnResult{}, offline(request.SourceSessionID, errors.New("owner RPC fork identity mismatch"))
	}
	return result, nil
}

func (m *Manager) reserveSpawn(ctx context.Context, requestID, parentID, sourceID, kind string, payload any) (spawnReservation, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return spawnReservation{}, err
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(encoded))
	nowMS := m.cfg.Now().UTC().UnixMilli()
	childID := session.NewID()
	_, err = m.db.ExecContext(ctx, `
		INSERT INTO session_spawn_edges(request_id,payload_hash,parent_session_id,child_session_id,source_session_id,kind,status,created_at_ms,updated_at_ms)
		VALUES(?,?,?,?,?,?,'provisioning',?,?) ON CONFLICT(request_id) DO NOTHING`,
		requestID, hash, parentID, childID, sourceID, kind, nowMS, nowMS)
	if err != nil {
		return spawnReservation{}, fmt.Errorf("control plane: reserve spawn: %w", err)
	}
	var out spawnReservation
	err = m.db.QueryRowContext(ctx, `SELECT request_id,payload_hash,parent_session_id,child_session_id,source_session_id,kind,status FROM session_spawn_edges WHERE request_id=?`, requestID).
		Scan(&out.RequestID, &out.PayloadHash, &out.ParentSessionID, &out.ChildSessionID, &out.SourceSessionID, &out.Kind, &out.Status)
	if err != nil {
		return spawnReservation{}, fmt.Errorf("control plane: read spawn reservation: %w", err)
	}
	if out.PayloadHash != hash || out.ParentSessionID != parentID || out.SourceSessionID != sourceID || out.Kind != kind {
		return spawnReservation{}, errors.New("control plane: spawn request_id was already used with a different payload")
	}
	return out, nil
}

func (m *Manager) completeSpawn(ctx context.Context, childID string) error {
	result, err := m.db.ExecContext(ctx, `UPDATE session_spawn_edges SET status='open',updated_at_ms=? WHERE child_session_id=?`, m.cfg.Now().UTC().UnixMilli(), childID)
	if err != nil {
		return fmt.Errorf("control plane: complete spawn edge: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return sql.ErrNoRows
	}
	return nil
}
