package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"code-agent/internal/tools"
)

const pollInterval = 200 * time.Millisecond

type eventPollResponse struct {
	Completion *tools.SessionWaitCompletion `json:"completion,omitempty"`
	Cursor     int64                        `json:"cursor"`
}

func (m *Manager) Send(ctx context.Context, request tools.SessionSendRequest) (tools.SessionDelivery, error) {
	lease, err := m.resolveLease(ctx, request.TargetSessionID)
	if err != nil {
		return tools.SessionDelivery{}, err
	}
	localOwners.RLock()
	owner := localOwners.byInstance[lease.InstanceID]
	localOwners.RUnlock()
	if owner != nil {
		return owner.acceptOwned(ctx, request.TargetSessionID, request)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return tools.SessionDelivery{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(lease.Endpoint, "/")+ownerSessionPathPrefix+url.PathEscape(request.TargetSessionID)+"/turns", bytes.NewReader(payload))
	if err != nil {
		return tools.SessionDelivery{}, err
	}
	req.Header.Set("Authorization", "Bearer "+lease.AuthToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return tools.SessionDelivery{}, offline(request.TargetSessionID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized {
			return tools.SessionDelivery{}, offline(request.TargetSessionID, fmt.Errorf("owner RPC returned %s", resp.Status))
		}
		return tools.SessionDelivery{}, fmt.Errorf("control plane: target %s rejected submission: %s", request.TargetSessionID, strings.TrimSpace(string(body)))
	}
	var delivery tools.SessionDelivery
	if err := json.NewDecoder(resp.Body).Decode(&delivery); err != nil {
		return tools.SessionDelivery{}, offline(request.TargetSessionID, err)
	}
	if !delivery.Accepted || delivery.SessionID != request.TargetSessionID || delivery.TurnID == "" || delivery.Cursor <= 0 || (delivery.Delivery != "started" && delivery.Delivery != "queued") {
		return tools.SessionDelivery{}, offline(request.TargetSessionID, errors.New("owner RPC admission identity mismatch"))
	}
	return delivery, nil
}

func (m *Manager) WaitAny(ctx context.Context, targets []tools.SessionWaitTarget, timeout time.Duration) (tools.SessionWaitResult, error) {
	if len(targets) == 0 || len(targets) > 8 {
		return tools.SessionWaitResult{}, errors.New("control plane: wait requires 1 to 8 targets")
	}
	working := append([]tools.SessionWaitTarget(nil), targets...)
	deadline := time.Now().Add(timeout)
	for {
		for i := range working {
			completion, cursor, err := m.pollTarget(ctx, working[i])
			if err != nil {
				return tools.SessionWaitResult{}, err
			}
			if cursor > working[i].Cursor {
				working[i].Cursor = cursor
			}
			if completion != nil {
				waiting := append([]tools.SessionWaitTarget(nil), working[:i]...)
				waiting = append(waiting, working[i+1:]...)
				return tools.SessionWaitResult{Completed: []tools.SessionWaitCompletion{*completion}, Waiting: waiting}, nil
			}
		}
		if timeout <= 0 || !time.Now().Before(deadline) {
			return tools.SessionWaitResult{Waiting: working, TimedOut: true}, nil
		}
		wait := pollInterval
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return tools.SessionWaitResult{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (m *Manager) pollTarget(ctx context.Context, target tools.SessionWaitTarget) (*tools.SessionWaitCompletion, int64, error) {
	lease, err := m.resolveLease(ctx, target.SessionID)
	if err != nil {
		return nil, target.Cursor, err
	}
	localOwners.RLock()
	owner := localOwners.byInstance[lease.InstanceID]
	localOwners.RUnlock()
	if owner != nil {
		return owner.eventsOwned(ctx, target.SessionID, target.TurnID, target.Cursor)
	}
	endpoint := strings.TrimRight(lease.Endpoint, "/") + ownerSessionPathPrefix + url.PathEscape(target.SessionID) + "/events?turn_id=" + url.QueryEscape(target.TurnID) + "&after=" + fmt.Sprint(target.Cursor)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, target.Cursor, err
	}
	req.Header.Set("Authorization", "Bearer "+lease.AuthToken)
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, target.Cursor, offline(target.SessionID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, target.Cursor, offline(target.SessionID, fmt.Errorf("owner RPC returned %s", resp.Status))
	}
	var result eventPollResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, target.Cursor, offline(target.SessionID, err)
	}
	if result.Cursor < target.Cursor {
		return nil, target.Cursor, offline(target.SessionID, errors.New("owner RPC cursor regressed"))
	}
	if result.Completion != nil && (result.Completion.SessionID != target.SessionID || result.Completion.TurnID != target.TurnID || result.Completion.Cursor <= target.Cursor) {
		return nil, target.Cursor, offline(target.SessionID, errors.New("owner RPC event identity mismatch"))
	}
	return result.Completion, result.Cursor, nil
}

func (m *Manager) acceptOwned(ctx context.Context, sessionID string, request tools.SessionSendRequest) (tools.SessionDelivery, error) {
	owned, err := m.owns(ctx, sessionID)
	if err != nil || !owned {
		return tools.SessionDelivery{}, offline(sessionID, err)
	}
	m.mu.Lock()
	target, executionCtx := m.target, m.runCtx
	m.mu.Unlock()
	if target == nil {
		return tools.SessionDelivery{}, offline(sessionID, errors.New("target executor unavailable"))
	}
	return target.Accept(ctx, executionCtx, sessionID, request)
}

func (m *Manager) eventsOwned(ctx context.Context, sessionID, turnID string, cursor int64) (*tools.SessionWaitCompletion, int64, error) {
	owned, err := m.owns(ctx, sessionID)
	if err != nil || !owned {
		return nil, cursor, offline(sessionID, err)
	}
	m.mu.Lock()
	target := m.target
	m.mu.Unlock()
	if target == nil {
		return nil, cursor, offline(sessionID, errors.New("target event store unavailable"))
	}
	return target.EventsSince(ctx, sessionID, turnID, cursor)
}
