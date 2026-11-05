// Package mcpbridge is the isolated P0-A experiment. It drives an existing
// loopback runtime; it never constructs an agent runner or changes its policy.
package mcpbridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"code-agent/internal/collab"
	"code-agent/internal/executionevidence"
	"code-agent/internal/session"

	"github.com/coder/websocket"
)

const maxResponse = 8 << 20

// ErrObserveOnly is returned by every mutating path when the process is bound
// in observation mode. Unregistering write tools is the first boundary; this
// method-level guard is the second, so a later code path cannot dispatch by
// accident.
var ErrObserveOnly = errors.New("this bridge is bound in read-only observation mode; it cannot create, send, cancel, or run tasks")

func (c *Client) requireWritable() error {
	if c.observeOnly {
		return ErrObserveOnly
	}
	return nil
}

// Event is a bounded subset of agent-wire, not a second runtime event model.
type Event struct {
	ValidationContract     json.RawMessage `json:"validation_contract,omitempty"`
	ValidationContractHash string          `json:"validation_contract_hash,omitempty"`
	ModelCallEvidence
	Kind            string `json:"kind,omitempty"`
	Type            string `json:"type,omitempty"`
	ID              string `json:"id,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
	TurnID          string `json:"turn_id,omitempty"`
	RequestID       string `json:"request_id,omitempty"`
	Seq             int64  `json:"seq,omitempty"`
	CallID          string `json:"call_id,omitempty"`
	ToolName        string `json:"tool_name,omitempty"`
	ToolArgs        any    `json:"tool_args,omitempty"`
	Observation     string `json:"observation,omitempty"`
	Output          any    `json:"output,omitempty"`
	Text            string `json:"text,omitempty"`
	ModelName       string `json:"model,omitempty"`
	Provider        string `json:"provider,omitempty"`
	Reason          string `json:"reason,omitempty"`
	CancelledReason string `json:"cancelled_reason,omitempty"`
	Err             string `json:"err,omitempty"`
	Error           any    `json:"error,omitempty"`
}

type Receipt struct {
	Accepted  bool   `json:"accepted"`
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	RequestID string `json:"request_id"`
	Cursor    int64  `json:"cursor"`
}

type ReadResult struct {
	ValidationProvenance
	SessionID         string                    `json:"session_id"`
	TurnID            string                    `json:"turn_id"`
	Status            string                    `json:"status"`
	Cursor            int64                     `json:"cursor"`
	Events            []Event                   `json:"events"`
	PendingControls   []any                     `json:"pending_controls"`
	StreamConnected   bool                      `json:"stream_connected"`
	Terminal          bool                      `json:"terminal"`
	AttentionRequired bool                      `json:"attention_required"`
	Attention         *Attention                `json:"attention,omitempty"`
	ExecutionEvidence *executionevidence.Report `json:"execution_evidence,omitempty"`
	EvidenceNote      string                    `json:"evidence_note,omitempty"`
}

type submission struct {
	wirePayload []byte
	hash        [32]byte
	receipt     *Receipt
	recovered   bool
}

// Client is bound to one isolated repository and one experiment session.
// Explicit reattachment recovers durable admissions, never execution or approvals.
type Client struct {
	base           *url.URL
	token          string
	root           string
	rootInfo       os.FileInfo
	gitDir         string
	gitInfo        os.FileInfo
	baseline       string
	delivery       string
	observeOnly    bool
	http           *http.Client
	ctx            context.Context
	cancel         context.CancelFunc
	mu             sync.Mutex
	sendMu         sync.Mutex
	connectMu      sync.Mutex
	lastConnectTry time.Time
	id             string
	createKey      string
	conn           *websocket.Conn
	controls       map[string]json.RawMessage
	submissions    map[string]*submission
	expectations   map[string]*executionevidence.Expectations // exact turn, guarded by mu
	reattached     bool
	binding        *connectionBinding
}

func New(ctx context.Context, endpoint, token, workspace string) (*Client, error) {
	return newClient(ctx, endpoint, token, workspace, "", "")
}

func newClient(ctx context.Context, endpoint, token, workspace, baseline, delivery string) (*Client, error) {
	u, err := parseEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("runtime access token is required")
	}
	if !filepath.IsAbs(workspace) {
		return nil, errors.New("workspace must be an absolute isolated repository path")
	}
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace: %w", err)
	}
	root = filepath.Clean(root)
	gitRoot, _, err := gitOutput(ctx, root, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(strings.TrimSpace(gitRoot))
	if err != nil || canonical != root {
		return nil, errors.New("workspace must be the repository root")
	}
	// Execution binding keeps its strict clean/HEAD invariant. Observation
	// binding is a separate mode: it reviews a recorded commit range and must
	// not be able to weaken the checks that guard a live workspace.
	if delivery == "" {
		status, truncated, err := gitOutput(ctx, root, "status", "--porcelain", "--untracked-files=normal")
		if err != nil {
			return nil, err
		}
		if baseline == "" && (truncated || strings.TrimSpace(status) != "") {
			return nil, errors.New("P0-A requires a clean isolated repository, including untracked files; for a previously used Session use --reattach-session with its original --baseline-commit, never clean/reset user changes or reuse --attach-empty-session")
		}
	}
	head, _, err := gitOutput(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	pinned := strings.TrimSpace(head)
	switch {
	case delivery != "":
		if err := validateCommitRange(ctx, root, baseline, delivery); err != nil {
			return nil, err
		}
		pinned = baseline
	case baseline != "":
		if strings.TrimSpace(head) != baseline {
			return nil, bridgeError("baseline_changed", nil)
		}
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	gitDir, cut, err := gitOutput(ctx, root, "rev-parse", "--absolute-git-dir")
	if err != nil || cut {
		return nil, errors.New("cannot pin workspace git identity")
	}
	gitDir = strings.TrimSpace(gitDir)
	gitInfo, err := os.Stat(gitDir)
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	c := &Client{base: u, token: token, root: root, rootInfo: rootInfo, gitDir: gitDir, gitInfo: gitInfo, baseline: pinned, delivery: delivery, observeOnly: delivery != "", ctx: runCtx, cancel: cancel,
		http:     &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("runtime redirects are disabled") }},
		controls: map[string]json.RawMessage{}, submissions: map[string]*submission{}, expectations: map[string]*executionevidence.Expectations{}}
	return c, nil
}

func (c *Client) Close() {
	c.cancel()
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()
	if conn != nil {
		_ = conn.CloseNow()
	}
	c.http.CloseIdleConnections()
}

func (c *Client) request(ctx context.Context, method, path string, body, out any) error {
	return c.requestWithHeaders(ctx, method, path, body, out, nil)
}

// Explicit page readers report partial-history headers themselves; evidence
// readers keep rejecting incomplete replay through request above.
func (c *Client) requestWithHeaders(ctx context.Context, method, path string, body, out any, headers *http.Header) error {
	if err := c.checkWorkspaceIdentity(); err != nil {
		return err
	}
	if err := c.checkBinding(); err != nil {
		return err
	}
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	u := *c.base
	u.Path, u.RawQuery, _ = strings.Cut(path, "?")
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return bridgeError("runtime_unreachable", err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return readRuntimeAPIError(res.StatusCode, res.Body)
	}
	if headers != nil {
		*headers = res.Header.Clone()
	}
	if headers == nil && strings.HasSuffix(u.Path, "/events") {
		for _, header := range []string{"X-Codeagent-Replay-Truncated", "X-Codeagent-Has-More-Older", "X-Codeagent-Turn-Fragment"} {
			if value := res.Header.Get(header); value != "" && value != "0" && value != "false" {
				return bridgeError("evidence_incomplete", nil)
			}
		}
	}
	limit := maxResponse
	if strings.Contains(u.Path, "/immutable-artifacts/") {
		limit = collab.MaxReviewBytes + (128 << 10)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, int64(limit+1)))
	if err != nil {
		return err
	}
	if len(b) > limit {
		return errors.New("runtime response exceeds P0-A limit; result is incomplete")
	}
	// Current public APIs use the gateway-style envelope. Older daemon builds
	// returned bare payloads, which remain useful for compatibility fixtures.
	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(b, &envelope) == nil && envelope.Code != nil {
		if *envelope.Code != 0 {
			return errors.New("runtime returned an API error")
		}
		b = envelope.Data
	}
	return json.Unmarshal(b, out)
}

type SessionRef struct {
	ID            string `json:"id"`
	WorkspacePath string `json:"workspace_path"`
}

func validID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("-_.:", r)) {
			return false
		}
	}
	return true
}

func (c *Client) Create(ctx context.Context, requestID string) (SessionRef, error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if err := c.requireWritable(); err != nil {
		return SessionRef{}, err
	}
	if !validID(requestID) {
		return SessionRef{}, errors.New("valid request_id is required")
	}
	if c.id != "" {
		if c.createKey != requestID {
			return SessionRef{}, errors.New("P0-A allows one session per bridge process")
		}
		return SessionRef{c.id, c.root}, nil
	}
	var ref SessionRef
	if err := c.request(ctx, "POST", "/v1/conversations", map[string]any{"workspace_path": c.root, "execution_policy": "shared_workspace"}, &ref); err != nil {
		return ref, admissionError("create_unknown", "", requestID, err)
	}
	if !validID(ref.ID) || ref.WorkspacePath != c.root {
		return ref, errors.New("runtime created an unexpected session/workspace")
	}
	c.mu.Lock()
	c.id, c.createKey = ref.ID, requestID
	c.mu.Unlock()
	return ref, nil
}

func (c *Client) checkSession(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id == "" || c.id != id {
		return bridgeError("session_mismatch", nil)
	}
	return nil
}

// connect serializes handshakes independently of submission. The reader never grants approvals; it only
// records pending human controls, leaving the runtime's existing policy intact.
func (c *Client) connect(ctx context.Context, id string) (*websocket.Conn, error) {
	c.connectMu.Lock()
	defer c.connectMu.Unlock()
	if err := c.checkBinding(); err != nil {
		return nil, err
	}
	if err := c.checkWorkspaceIdentity(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		return conn, nil
	}
	if c.reattached && time.Since(c.lastConnectTry) < time.Second {
		return nil, errors.New("observation reconnect is backing off")
	}
	c.lastConnectTry = time.Now()
	u := *c.base
	u.Scheme, u.Path = "ws", "/v1/conversations/"+id+"/stream"
	if c.base.Scheme == "https" {
		u.Scheme = "wss"
	}
	conn, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPClient: c.http, HTTPHeader: http.Header{"Authorization": []string{"Bearer " + c.token}}})
	if err != nil {
		return nil, bridgeError("runtime_unreachable", err)
	}
	conn.SetReadLimit(maxResponse)
	_, hello, err := conn.Read(ctx)
	var h struct {
		Type string `json:"type"`
	}
	if err != nil || json.Unmarshal(hello, &h) != nil || h.Type != "hello" {
		_ = conn.CloseNow()
		return nil, errors.New("runtime did not send agent-wire hello")
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	go c.readControls(conn)
	return conn, nil
}

func parseEndpoint(endpoint string) (*url.URL, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.Opaque != "" {
		return nil, errors.New("an explicit http(s)://loopback:port runtime endpoint is required")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() || u.Port() == "" {
		return nil, errors.New("runtime must use a numeric loopback address and explicit port")
	}
	return u, nil
}

func (c *Client) readControls(conn *websocket.Conn) {
	defer func() {
		c.mu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.mu.Unlock()
		_ = conn.CloseNow()
	}()
	for {
		_, data, err := conn.Read(c.ctx)
		if err != nil {
			return
		}
		var e Event
		if json.Unmarshal(data, &e) != nil {
			continue
		}
		c.mu.Lock()
		switch e.Type {
		case "approval_request", "plan_approval_request", "ask_user_request":
			c.controls[e.ID] = append(json.RawMessage(nil), data...)
		case "agent_input_rejected":
			c.controls["rejected:"+e.RequestID] = append(json.RawMessage(nil), data...)
		}
		c.mu.Unlock()
	}
}

func (c *Client) Send(ctx context.Context, id, requestID, message, model string) (Receipt, error) {
	return c.SendWithExpectations(ctx, id, requestID, message, model, nil)
}

// SendWithExpectations opts a development turn into evidence checks. The
// contract is process-local P0-A metadata, not a new Runtime Task or loop gate.
func (c *Client) SendWithExpectations(ctx context.Context, id, requestID, message, model string, development *executionevidence.Expectations) (Receipt, error) {
	return c.sendWithExpectations(ctx, id, requestID, message, model, development, false)
}

func (c *Client) sendWithExpectations(ctx context.Context, id, requestID, message, model string, development *executionevidence.Expectations, durable bool) (Receipt, error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if err := c.requireWritable(); err != nil {
		return Receipt{}, err
	}
	if err := c.checkSession(id); err != nil {
		return Receipt{}, err
	}
	if !validID(requestID) || strings.TrimSpace(message) == "" || len(message) > 64<<10 {
		return Receipt{}, bridgeError("invalid_tool_arguments", nil)
	}
	payload := map[string]any{"type": "agent_input", "kind": "text", "request_id": requestID, "text": message, "model": model}
	var contract *session.ValidationContract
	if development != nil {
		var err error
		contract, err = (&session.ValidationContract{Version: session.ValidationContractVersion, ValidationCommands: development.ValidationCommands, ManualChecks: development.ManualChecks}).CloneValidated()
		if err != nil {
			return Receipt{}, bridgeError("invalid_validation_contract", err)
		}
		development = &executionevidence.Expectations{ValidationCommands: contract.ValidationCommands, ManualChecks: contract.ManualChecks}
		if durable {
			if err := c.requireValidationCapability(ctx); err != nil {
				return Receipt{}, err
			}
			payload["validation_contract"] = contract
		}
	}
	data, _ := json.Marshal(payload)
	identity, _ := json.Marshal(struct {
		Payload     json.RawMessage
		Development *executionevidence.Expectations
	}{data, development})
	hash := sha256.Sum256(identity)
	if previous := c.submissions[requestID]; previous != nil {
		if previous.recovered {
			e := admissionError("already_admitted", id, requestID, nil)
			if previous.receipt != nil {
				e.TurnID = previous.receipt.TurnID
			}
			return Receipt{}, e
		}
		if previous.hash != hash {
			return Receipt{}, admissionError("request_conflict", id, requestID, nil)
		}
		if previous.receipt != nil {
			return *previous.receipt, nil
		}
		data = previous.wirePayload
	} else {
		if c.reattached {
			if err := c.checkReattachedAdmission(ctx, requestID); err != nil {
				return Receipt{}, err
			}
		}
		if model != "" {
			resolved, err := c.resolveModel(ctx, model)
			if err != nil {
				return Receipt{}, err
			}
			payload["model"] = resolved
			data, _ = json.Marshal(payload)
		}
		for previousRequest, previous := range c.submissions {
			if previous.receipt == nil {
				return Receipt{}, admissionError("admission_unknown", id, previousRequest, nil)
			}
			r, err := c.Read(ctx, id, previous.receipt.TurnID, 0)
			if err != nil {
				return Receipt{}, err
			}
			if !terminal(r.Status) {
				return Receipt{}, bridgeError("session_busy", nil)
			}
		}
		c.mu.Lock()
		c.controls = map[string]json.RawMessage{}
		c.mu.Unlock()
		c.submissions[requestID] = &submission{hash: hash, wirePayload: data}
	}
	admitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := c.connect(admitCtx, id)
	if err != nil {
		return Receipt{}, err
	}
	if err := conn.Write(admitCtx, websocket.MessageText, data); err != nil {
		return Receipt{}, admissionError("admission_unknown", id, requestID, err)
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		c.mu.Lock()
		rejected := c.controls["rejected:"+requestID]
		c.mu.Unlock()
		if len(rejected) > 0 {
			return Receipt{}, errors.New("runtime rejected this input before admission")
		}
		events, err := c.events(admitCtx, id, 0)
		if err != nil {
			return Receipt{}, admissionError("admission_unknown", id, requestID, err)
		}
		for _, e := range events {
			if e.Kind == "turn_accepted" && e.RequestID == requestID && e.TurnID != "" && e.Seq > 0 {
				if durable && contract != nil {
					persisted, _, err := c.validationFromEvents(events, id, e.TurnID)
					if err != nil {
						return Receipt{}, admissionError("admission_unknown", id, requestID, err)
					}
					want, _ := contract.Hash()
					if persisted == nil || persistedHash(persisted) != want {
						return Receipt{}, admissionError("admission_unknown", id, requestID, bridgeError("evidence_incomplete", nil))
					}
				}
				r := Receipt{true, id, e.TurnID, requestID, e.Seq}
				c.submissions[requestID].receipt = &r
				if development != nil && !durable {
					c.mu.Lock()
					c.expectations[r.TurnID] = development
					c.mu.Unlock()
				}
				return r, nil
			}
		}
		select {
		case <-admitCtx.Done():
			return Receipt{}, admissionError("admission_unknown", id, requestID, admitCtx.Err())
		case <-ticker.C:
		}
	}
}

func (c *Client) events(ctx context.Context, id string, since int64) ([]Event, error) {
	var events []Event
	err := c.request(ctx, "GET", fmt.Sprintf("/v1/conversations/%s/events?since=%d", id, since), nil, &events)
	if err != nil {
		return nil, err
	}
	for _, e := range events {
		if e.SessionID != id {
			return nil, errors.New("runtime event belongs to a different session")
		}
	}
	return events, nil
}

func terminal(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled"
}

func (c *Client) Read(ctx context.Context, id, turnID string, since int64) (ReadResult, error) {
	if err := c.checkSession(id); err != nil {
		return ReadResult{}, err
	}
	if !validID(turnID) || since < 0 {
		return ReadResult{}, errors.New("valid turn_id and nonnegative cursor required")
	}
	if c.reattached {
		if err := c.verifyRuntimeIdentity(ctx); err != nil {
			return ReadResult{}, err
		}
		observeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, _ = c.connect(observeCtx, id) // read-only reconnect; durable HTTP evidence remains readable
		cancel()
	}
	all, err := c.events(ctx, id, 0)
	if err != nil {
		return ReadResult{}, err
	}
	// Derive lifecycle and delta from one durable observation. Two requests can
	// otherwise pair a newer terminal state with an older delivery cursor.
	r := ReadResult{SessionID: id, TurnID: turnID, Status: "waiting", Cursor: since, Events: []Event{}, PendingControls: []any{}}
	for _, e := range all {
		if e.Seq > r.Cursor {
			r.Cursor = e.Seq
		}
		if e.Seq <= since || e.TurnID != turnID {
			continue
		}
		r.Events = append(r.Events, e)
	}
	for _, e := range all {
		if e.TurnID != turnID {
			continue
		}
		switch e.Kind {
		case "turn_started", "turn_resumed":
			r.Status = "running"
		case "turn_queued":
			r.Status = "queued"
		case "turn_paused":
			r.Status = "paused"
		case "turn_finished":
			r.Status = "completed"
		case "turn_failed":
			r.Status = "failed"
		case "turn_cancelled":
			r.Status = "cancelled"
		}
	}
	if c.reattached {
		found := false
		for _, e := range all {
			if e.TurnID == turnID && e.Kind == "turn_accepted" {
				found = true
				break
			}
		}
		if !found {
			return ReadResult{}, errors.New("exact Turn admission not found in the bound Session")
		}
	}
	c.mu.Lock()
	r.StreamConnected = c.conn != nil
	if !terminal(r.Status) {
		for _, v := range c.controls {
			var ref Event
			if json.Unmarshal(v, &ref) != nil || (ref.SessionID != "" && ref.SessionID != id) || (ref.TurnID != "" && ref.TurnID != turnID) {
				continue
			}
			if ref.Type != "approval_request" && ref.Type != "plan_approval_request" && ref.Type != "ask_user_request" {
				continue
			}
			var control any
			if json.Unmarshal(v, &control) == nil {
				r.PendingControls = append(r.PendingControls, control)
			}
		}
	}
	c.mu.Unlock()
	// Controls are broadcast without a resolution frame on current runtimes.
	// Count-only runtime attention prevents already-resolved UI requests from
	// permanently reporting waiting_for_human. Payloads remain session-scoped.
	if !terminal(r.Status) {
		var activity struct {
			Sessions []struct {
				SessionID string `json:"session_id"`
				Pending   int    `json:"pending_approval_count"`
				State     string `json:"state"`
			} `json:"sessions"`
		}
		if c.request(ctx, "GET", "/v1/activity", nil, &activity) == nil {
			pending := false
			for _, a := range activity.Sessions {
				if a.SessionID == id && (a.Pending > 0 || a.State == "waiting_approval" || a.State == "waiting_client_tool") {
					pending = true
				}
			}
			if !pending {
				r.PendingControls = []any{}
			} else {
				r.Status = "waiting_for_human"
			}
		}
	}
	if len(r.PendingControls) > 0 {
		r.Status = "waiting_for_human"
	}
	r.Terminal = terminal(r.Status)
	if r.Status == "waiting_for_human" || r.Status == "paused" {
		r.AttentionRequired = true
		r.Attention = attentionFor(r)
	}
	contract, provenance, err := c.validationFromEvents(all, id, turnID)
	if err != nil {
		return ReadResult{}, err
	}
	r.ValidationProvenance = provenance
	var expected *executionevidence.Expectations
	if contract != nil {
		expected = &executionevidence.Expectations{ValidationCommands: contract.ValidationCommands, ManualChecks: contract.ManualChecks}
	} else {
		c.mu.Lock()
		expected = c.expectations[turnID]
		c.mu.Unlock()
		if expected != nil {
			r.ValidationSource = "legacy_process_local"
		}
	}
	if expected == nil && c.reattached {
		r.EvidenceNote = "no persisted validation contract was declared; process-local expectations cannot be recovered; no execution-evidence verdict is inferred"
	}
	if expected != nil {
		evidence := developmentEvidence(all, turnID, *expected, r.Status, r.AttentionRequired)
		if len(expected.ManualChecks) > 0 {
			r.PendingManualChecks = append([]string(nil), expected.ManualChecks...)
		}
		r.ExecutionEvidence = &evidence
	}
	return r, nil
}

// Wait always replays from the admission cursor, so an earlier terminal cannot
// be lost when callers retain only the newest polling cursor.
func (c *Client) Wait(ctx context.Context, id, turnID string, cursor int64, timeout time.Duration) (ReadResult, error) {
	if timeout < 0 || timeout > 30*time.Second {
		return ReadResult{}, errors.New("timeout must be between 0 and 30 seconds")
	}
	if cursor < 0 {
		return ReadResult{}, errors.New("nonnegative cursor required")
	}
	c.sendMu.Lock()
	for _, s := range c.submissions {
		if s.receipt != nil && s.receipt.SessionID == id && s.receipt.TurnID == turnID {
			cursor = s.receipt.Cursor
			break
		}
	}
	c.sendMu.Unlock()
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		r, err := c.Read(ctx, id, turnID, cursor)
		if err != nil {
			return r, err
		}
		if terminal(r.Status) || r.Status == "waiting_for_human" || r.Status == "paused" || timeout == 0 {
			return r, nil
		}
		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return r, ctx.Err()
			}
			return r, nil
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (c *Client) Cancel(ctx context.Context, id string) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if err := c.requireWritable(); err != nil {
		return err
	}
	if err := c.checkSession(id); err != nil {
		return err
	}
	conn, err := c.connect(ctx, id)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, []byte(`{"type":"cancel_turn"}`))
}
