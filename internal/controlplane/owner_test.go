package controlplane

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"code-agent/internal/session"
	"code-agent/internal/sessionfork"
	"code-agent/internal/tools"
)

type fakeRepo struct {
	mu       sync.Mutex
	sessions map[string]*session.Session
}

func (r *fakeRepo) List(context.Context) ([]session.Meta, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]session.Meta, 0, len(r.sessions))
	for _, sess := range r.sessions {
		status, _ := sess.Metadata["turn_status"].(string)
		out = append(out, session.Meta{ID: sess.ID, WorkspacePath: sess.WorkspacePath, TurnStatus: status})
	}
	return out, nil
}

func (r *fakeRepo) Load(_ context.Context, id string) (*session.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sess := r.sessions[id]
	if sess == nil {
		return nil, errors.New("not found")
	}
	copy := *sess
	copy.Metadata = make(map[string]any, len(sess.Metadata))
	for key, value := range sess.Metadata {
		copy.Metadata[key] = value
	}
	return &copy, nil
}

type fakeActivity map[string]bool

func (a fakeActivity) HasActivity(id string) bool { return a[id] }

type fakeTarget struct {
	request       tools.SessionSendRequest
	createRequest tools.SessionCreateRequest
	createChild   string
	forkRequest   sessionfork.Request
	forkChild     string
}

func (t *fakeTarget) Accept(_ context.Context, _ context.Context, sessionID string, request tools.SessionSendRequest) (tools.SessionDelivery, error) {
	t.request = request
	return tools.SessionDelivery{Accepted: true, Delivery: "queued", SessionID: sessionID, TurnID: "target-turn", Cursor: 12}, nil
}

func (*fakeTarget) EventsSince(_ context.Context, sessionID, turnID string, cursor int64) (*tools.SessionWaitCompletion, int64, error) {
	if cursor < 14 {
		return &tools.SessionWaitCompletion{SessionID: sessionID, TurnID: turnID, Status: "completed", Cursor: 14}, 14, nil
	}
	return nil, cursor, nil
}

func (t *fakeTarget) BlockForTurn(ctx context.Context, sessionID, turnID string, cursor int64, timeout time.Duration) (*tools.SessionWaitCompletion, int64, error) {
	return t.EventsSince(ctx, sessionID, turnID, cursor)
}

func (t *fakeTarget) CreateSession(_ context.Context, request tools.SessionCreateRequest, childID string) (tools.SessionSpawnResult, error) {
	t.createRequest = request
	t.createChild = childID
	return tools.SessionSpawnResult{ID: childID, ParentSessionID: request.ParentSessionID, WorkspacePath: request.WorkspacePath, Kind: "spawn", Status: "open"}, nil
}

func (t *fakeTarget) ForkSession(_ context.Context, request sessionfork.Request, childID string) (sessionfork.Result, error) {
	t.forkRequest = request
	t.forkChild = childID
	baseCommit := ""
	if request.ExecutionPolicy == session.ExecutionPolicyIsolatedWorktree {
		baseCommit = "exact-remote-commit"
	}
	return sessionfork.Result{ID: childID, ParentSessionID: request.ParentSessionID, SourceSessionID: request.SourceSessionID, WorkspacePath: "/fork", Kind: "fork", Status: "open", BaseCommit: baseCommit}, nil
}

func testRepo() *fakeRepo {
	return &fakeRepo{sessions: map[string]*session.Session{
		"session-1": {ID: "session-1", WorkspacePath: "/workspace/one", Metadata: map[string]any{"turn_status": "running"}},
	}}
}

type failingRepo struct{}

func (failingRepo) List(context.Context) ([]session.Meta, error) {
	return nil, errors.New("list failed")
}

func (failingRepo) Load(context.Context, string) (*session.Session, error) {
	return nil, errors.New("load failed")
}

func TestManagerResolvesLocalOwner(t *testing.T) {
	m, err := NewManager(t.TempDir(), "server-1", testRepo(), fakeActivity{"session-1": true}, Config{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Close()

	resolution, err := m.Resolve(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !resolution.Local || resolution.Lease.InstanceID != m.Identity().InstanceID {
		t.Fatalf("unexpected route: %+v", resolution)
	}
	if !resolution.State.Active || resolution.State.TurnStatus != "running" || resolution.State.WorkspacePath != "/workspace/one" {
		t.Fatalf("unexpected state: %+v", resolution.State)
	}
	if _, err := m.Resolve(context.Background(), "missing"); !errors.Is(err, ErrTargetOffline) {
		t.Fatalf("missing Resolve error = %v", err)
	}
}

func TestManagerRoutesThroughAuthenticatedLoopbackRPC(t *testing.T) {
	base := t.TempDir()
	owner, err := NewManager(base, "owner-server", testRepo(), nil, Config{})
	if err != nil {
		t.Fatalf("NewManager owner: %v", err)
	}
	if err := owner.Start(context.Background()); err != nil {
		t.Fatalf("Start owner: %v", err)
	}
	defer owner.Close()

	router, err := NewManager(base, "router-server", &fakeRepo{sessions: map[string]*session.Session{}}, nil, Config{})
	if err != nil {
		t.Fatalf("NewManager router: %v", err)
	}
	defer router.Close()

	// Simulate a different process: the shared database still contains the lease,
	// but the process-local fast-path registry does not contain the owner.
	localOwners.Lock()
	delete(localOwners.byInstance, owner.Identity().InstanceID)
	localOwners.Unlock()
	defer func() {
		localOwners.Lock()
		localOwners.byInstance[owner.Identity().InstanceID] = owner
		localOwners.Unlock()
	}()

	resolution, err := router.Resolve(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("remote Resolve: %v", err)
	}
	if resolution.Local || resolution.State.InstanceID != owner.Identity().InstanceID {
		t.Fatalf("unexpected remote route: %+v", resolution)
	}

	resp, err := http.Get(owner.Identity().Endpoint + ownerIdentityPath)
	if err != nil {
		t.Fatalf("unauthenticated identity request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", resp.StatusCode)
	}
}

func TestManagerRoutesSendAndCursorWaitThroughOwnerRPC(t *testing.T) {
	base := t.TempDir()
	target := &fakeTarget{}
	owner, err := NewManager(base, "owner-server", testRepo(), nil, Config{})
	if err != nil {
		t.Fatal(err)
	}
	owner.SetTarget(target)
	if err := owner.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	router, err := NewManager(base, "router-server", &fakeRepo{sessions: map[string]*session.Session{}}, nil, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer router.Close()
	localOwners.Lock()
	delete(localOwners.byInstance, owner.Identity().InstanceID)
	localOwners.Unlock()
	defer func() {
		localOwners.Lock()
		localOwners.byInstance[owner.Identity().InstanceID] = owner
		localOwners.Unlock()
	}()

	delivery, err := router.Send(context.Background(), tools.SessionSendRequest{TargetSessionID: "session-1", Message: "work", SenderSessionID: "sender", MessageID: "message", CorrelationID: "corr", Intent: "request"})
	if err != nil || delivery.TurnID != "target-turn" || delivery.Cursor != 12 || delivery.Delivery != "queued" {
		t.Fatalf("delivery=%+v err=%v", delivery, err)
	}
	if target.request.SenderSessionID != "sender" || target.request.CorrelationID != "corr" {
		t.Fatalf("provenance lost: %+v", target.request)
	}
	result, err := router.WaitAny(context.Background(), []tools.SessionWaitTarget{{SessionID: "session-1", TurnID: delivery.TurnID, Cursor: delivery.Cursor}}, 0)
	if err != nil || len(result.Completed) != 1 || result.Completed[0].Cursor != 14 || result.TimedOut {
		t.Fatalf("wait=%+v err=%v", result, err)
	}
}

func TestManagerCreateSessionReservesStableSpawnEdge(t *testing.T) {
	target := &fakeTarget{}
	m, err := NewManager(t.TempDir(), "server", testRepo(), nil, Config{})
	if err != nil {
		t.Fatal(err)
	}
	m.SetTarget(target)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	request := tools.SessionCreateRequest{RequestID: "create-1", ParentSessionID: "session-1", WorkspacePath: "/workspace/child", ExecutionPolicy: session.ExecutionPolicySharedWorkspace}
	first, err := m.CreateSession(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.CreateSession(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || second.ID != first.ID || target.createChild != first.ID {
		t.Fatalf("first=%+v second=%+v target_child=%q", first, second, target.createChild)
	}
	var status string
	if err := m.db.QueryRow(`SELECT status FROM session_spawn_edges WHERE child_session_id=?`, first.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "open" {
		t.Fatalf("edge status = %q", status)
	}
	changed := request
	changed.Name = "different"
	if _, err := m.CreateSession(context.Background(), changed); err == nil {
		t.Fatal("reused request id with a changed payload succeeded")
	}
}

func TestManagerRoutesForkThroughSourceOwnerRPC(t *testing.T) {
	base := t.TempDir()
	ownerTarget := &fakeTarget{}
	owner, err := NewManager(base, "source-owner", testRepo(), nil, Config{})
	if err != nil {
		t.Fatal(err)
	}
	owner.SetTarget(ownerTarget)
	if err := owner.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer owner.Close()

	parentRepo := &fakeRepo{sessions: map[string]*session.Session{
		"parent": {ID: "parent", WorkspacePath: "/workspace/parent", Metadata: map[string]any{}},
	}}
	router, err := NewManager(base, "parent-owner", parentRepo, nil, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := router.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer router.Close()

	localOwners.Lock()
	delete(localOwners.byInstance, owner.Identity().InstanceID)
	localOwners.Unlock()
	defer func() {
		localOwners.Lock()
		localOwners.byInstance[owner.Identity().InstanceID] = owner
		localOwners.Unlock()
	}()

	request := sessionfork.Request{
		RequestID: "fork-1", ParentSessionID: "parent", SourceSessionID: "session-1", Name: "remote fork",
		ExecutionPolicy: session.ExecutionPolicyIsolatedWorktree, WorktreeName: "remote-snapshot",
	}
	result, err := router.ForkSession(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.ID == "" || result.SourceSessionID != "session-1" || result.BaseCommit != "exact-remote-commit" || ownerTarget.forkChild != result.ID || ownerTarget.forkRequest.Name != "remote fork" || ownerTarget.forkRequest.WorktreeName != "remote-snapshot" {
		t.Fatalf("result=%+v target_request=%+v target_child=%q", result, ownerTarget.forkRequest, ownerTarget.forkChild)
	}
	var status, source string
	if err := router.db.QueryRow(`SELECT status,source_session_id FROM session_spawn_edges WHERE child_session_id=?`, result.ID).Scan(&status, &source); err != nil {
		t.Fatal(err)
	}
	if status != "open" || source != "session-1" {
		t.Fatalf("edge status=%q source=%q", status, source)
	}
}

func TestActiveLeaseCannotBeStolenAndReleasedOwnerCanBeReplaced(t *testing.T) {
	base := t.TempDir()
	first, err := NewManager(base, "server-1", testRepo(), nil, Config{})
	if err != nil {
		t.Fatalf("NewManager first: %v", err)
	}
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("Start first: %v", err)
	}

	second, err := NewManager(base, "server-2", testRepo(), nil, Config{})
	if err != nil {
		t.Fatalf("NewManager second: %v", err)
	}
	if err := second.Start(context.Background()); err != nil {
		t.Fatalf("Start second: %v", err)
	}
	defer second.Close()

	resolution, err := second.Resolve(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("Resolve while first owns: %v", err)
	}
	if resolution.Lease.InstanceID != first.Identity().InstanceID {
		t.Fatalf("active lease was stolen: %+v", resolution.Lease)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close first: %v", err)
	}
	if err := second.Heartbeat(context.Background()); err != nil {
		t.Fatalf("second Heartbeat: %v", err)
	}
	resolution, err = second.Resolve(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("Resolve after release: %v", err)
	}
	if resolution.Lease.InstanceID != second.Identity().InstanceID || !resolution.Local {
		t.Fatalf("second did not claim released lease: %+v", resolution)
	}
}

func TestExpiredLeaseCanBeReclaimed(t *testing.T) {
	base := t.TempDir()
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	cfg := Config{LeaseTTL: 10 * time.Second, HeartbeatInterval: 9 * time.Second, Now: clock}
	first, err := NewManager(base, "server-1", testRepo(), nil, cfg)
	if err != nil {
		t.Fatalf("NewManager first: %v", err)
	}
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("Start first: %v", err)
	}
	defer first.Close()

	now = now.Add(11 * time.Second)
	second, err := NewManager(base, "server-2", testRepo(), nil, cfg)
	if err != nil {
		t.Fatalf("NewManager second: %v", err)
	}
	if err := second.Start(context.Background()); err != nil {
		t.Fatalf("Start second: %v", err)
	}
	defer second.Close()

	resolution, err := second.Resolve(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolution.Lease.InstanceID != second.Identity().InstanceID {
		t.Fatalf("expired lease was not reclaimed: %+v", resolution.Lease)
	}
}

func TestProtocolMismatchFailsClosed(t *testing.T) {
	m, err := NewManager(t.TempDir(), "server-1", testRepo(), nil, Config{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Close()
	if _, err := m.db.Exec(`UPDATE runtime_instances SET protocol_version=? WHERE instance_id=?`, ProtocolVersion+1, m.Identity().InstanceID); err != nil {
		t.Fatalf("update protocol: %v", err)
	}
	if _, err := m.Resolve(context.Background(), "session-1"); !errors.Is(err, ErrProtocolMismatch) {
		t.Fatalf("Resolve error = %v", err)
	}
}

func TestFailedStartDoesNotLeaveLiveLease(t *testing.T) {
	m, err := NewManager(t.TempDir(), "server-1", failingRepo{}, nil, Config{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Close()
	if err := m.Start(context.Background()); err == nil {
		t.Fatal("Start succeeded with failing repository")
	}
	var runtimes int
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM runtime_instances WHERE instance_id=?`, m.Identity().InstanceID).Scan(&runtimes); err != nil {
		t.Fatalf("count runtime leases: %v", err)
	}
	if runtimes != 0 {
		t.Fatalf("failed Start left %d runtime leases", runtimes)
	}
}

func TestControlDatabasePermissions(t *testing.T) {
	m, err := NewManager(t.TempDir(), "server-1", testRepo(), nil, Config{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Close()
	info, err := os.Stat(m.DBPath())
	if err != nil {
		t.Fatalf("stat control.db: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("control.db permissions = %o, want 600", got)
	}
}

func TestHeartbeatPreservesClaimTimeAndReleasesRemovedSessions(t *testing.T) {
	repo := testRepo()
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	cfg := Config{LeaseTTL: 10 * time.Second, HeartbeatInterval: 9 * time.Second, Now: func() time.Time { return now }}
	m, err := NewManager(t.TempDir(), "server-1", repo, nil, cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Close()
	var claimedBefore int64
	if err := m.db.QueryRow(`SELECT claimed_at_ms FROM session_owners WHERE session_id='session-1'`).Scan(&claimedBefore); err != nil {
		t.Fatalf("read initial claim: %v", err)
	}
	now = now.Add(time.Second)
	if err := m.Heartbeat(context.Background()); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	var claimedAfter int64
	if err := m.db.QueryRow(`SELECT claimed_at_ms FROM session_owners WHERE session_id='session-1'`).Scan(&claimedAfter); err != nil {
		t.Fatalf("read renewed claim: %v", err)
	}
	if claimedAfter != claimedBefore {
		t.Fatalf("claimed_at changed from %d to %d", claimedBefore, claimedAfter)
	}

	repo.mu.Lock()
	delete(repo.sessions, "session-1")
	repo.mu.Unlock()
	if err := m.Heartbeat(context.Background()); err != nil {
		t.Fatalf("Heartbeat after delete: %v", err)
	}
	if _, err := m.Resolve(context.Background(), "session-1"); !errors.Is(err, ErrTargetOffline) {
		t.Fatalf("Resolve removed session error = %v", err)
	}
}

func TestConcurrentCloseWaitsForLeaseRelease(t *testing.T) {
	base := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	m, err := NewManager(base, "server-1", testRepo(), nil, Config{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	instanceID := m.Identity().InstanceID
	cancel() // races the lifecycle watcher with this explicit Close.
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	checker, err := NewManager(base, "server-check", &fakeRepo{sessions: map[string]*session.Session{}}, nil, Config{})
	if err != nil {
		t.Fatalf("NewManager checker: %v", err)
	}
	defer checker.Close()
	var count int
	if err := checker.db.QueryRow(`SELECT COUNT(*) FROM runtime_instances WHERE instance_id=?`, instanceID).Scan(&count); err != nil {
		t.Fatalf("count released instance: %v", err)
	}
	if count != 0 {
		t.Fatalf("concurrent Close left %d runtime leases", count)
	}
}
