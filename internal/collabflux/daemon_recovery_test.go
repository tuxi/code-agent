//go:build unix

package collabflux_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/collab"
	"code-agent/internal/collabflux"
	"code-agent/internal/conversation"
	"code-agent/internal/mcpbridge"
	"code-agent/internal/server"
	"code-agent/internal/session"
	sessionsqlite "code-agent/internal/session/sqlite"
	"code-agent/internal/settings"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const fixtureToken = "offline-b2c-owner-token-0123456789abcdef"

func durableFile(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func processCheckpoint(dir, target, stage, run string) {
	if target != stage {
		return
	}
	_ = durableFile(filepath.Join(dir, "checkpoint"), []byte(stage+"\n"+run))
	select {}
}

type processBuilder struct {
	dir, root, target string
	executor          *conversation.TurnExecutor
}

type approvalCheckpointSink struct{ dir string }

func (s approvalCheckpointSink) Send(frame []byte) error {
	if err := durableFile(filepath.Join(s.dir, "approval.json"), frame); err != nil {
		return err
	}
	processCheckpoint(s.dir, "approval_wait", "approval_wait", "")
	return nil
}

func (*processBuilder) ResolveModel(string) (*settings.ModelConfig, error) {
	return &settings.ModelConfig{Name: "default", Model: "default"}, nil
}
func (b *processBuilder) Build(c conversation.RuntimeContext) conversation.TurnRunner {
	if b.target == "approval_wait" {
		approval := server.NewRemoteApprover(0, nil)
		approval.AddSink(1, approvalCheckpointSink{dir: b.dir})
		b.executor.SetApprover(c.Session.ID, approval)
		c.Approver = approval
	}
	return &processRunner{b: b, c: c}
}

type processRunner struct {
	b *processBuilder
	c conversation.RuntimeContext
}

func (r *processRunner) RunTurn(ctx context.Context, s *session.Session, _ string) (agent.TurnResult, error) {
	return r.RunPreparedTurn(ctx, s)
}
func (r *processRunner) ResumeTurn(ctx context.Context, s *session.Session) (agent.TurnResult, error) {
	return r.RunPreparedTurn(ctx, s)
}
func (r *processRunner) RunPreparedTurn(ctx context.Context, s *session.Session) (agent.TurnResult, error) {
	if r.c.ManagedTask == nil || r.c.Model != "default" || r.c.ResolvedModel != "default" {
		return agent.TurnResult{}, errors.New("lost managed source or Runtime default model")
	}
	emit := func(e agent.Event) { e.SessionID = s.ID; e.TurnID = r.c.TurnID; r.c.Publisher.Emit(e) }
	emit(agent.Event{Kind: agent.EventTurnStarted})
	// Hold execution before its first side effect while the dispatch stage has
	// committed admission but has not returned its node output.
	if r.b.target == "turn_admitted" {
		processCheckpoint(r.b.dir, "turn_admitted", "turn_admitted", r.c.ManagedTask.RunID)
	}
	if r.b.target == "approval_wait" && r.c.Approver.Approve("edit_file", json.RawMessage(`{"path":"main.txt"}`)) != agent.VerdictAllow {
		return agent.TurnResult{}, errors.New("not approved")
	}
	emit(agent.Event{Kind: agent.EventToolStarted, CallID: "edit", ToolName: "edit_file", ToolArgs: `{"path":"main.txt"}`})
	countPath := filepath.Join(r.b.dir, "effects")
	old, _ := os.ReadFile(countPath)
	n, _ := strconv.Atoi(string(old))
	n++
	if err := durableFile(countPath, []byte(strconv.Itoa(n))); err != nil {
		return agent.TurnResult{}, err
	}
	if err := durableFile(filepath.Join(r.b.root, "main.txt"), []byte(fmt.Sprintf("candidate %d\n", n))); err != nil {
		return agent.TurnResult{}, err
	}
	processCheckpoint(r.b.dir, r.b.target, "side_effect", r.c.ManagedTask.RunID)
	emit(agent.Event{Kind: agent.EventToolFinished, CallID: "edit", ToolName: "edit_file", Output: json.RawMessage(`{"ok":true}`)})
	emit(agent.Event{Kind: agent.EventToolStarted, CallID: "check", ToolName: "run_command", ToolArgs: `{"command":"git diff --check"}`})
	cmd := exec.CommandContext(ctx, "git", "-C", r.b.root, "diff", "--check")
	if out, err := cmd.CombinedOutput(); err != nil {
		return agent.TurnResult{}, fmt.Errorf("local verification failed: %s: %w", out, err)
	}
	emit(agent.Event{Kind: agent.EventToolFinished, CallID: "check", ToolName: "run_command", Output: json.RawMessage(`{"command":"git diff --check","status":"completed","decision":"confirm","exit_code":0}`)})
	emit(agent.Event{Kind: agent.EventTurnFinished})
	return agent.TurnResult{}, nil
}

// A real OS child hosts the production HTTP mux, TurnExecutor, SQLite and
// managed Flux service. Only its model runner is scripted; no API is contacted.
func TestManagedDaemonProcess(t *testing.T) {
	if os.Getenv("CODEAGENT_B2C_HELPER") != "daemon" {
		t.Skip("subprocess helper")
	}
	dir := os.Getenv("B2C_STATE")
	root := os.Getenv("B2C_ROOT")
	target := os.Getenv("B2C_STOP")
	store, err := sessionsqlite.New(filepath.Join(dir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repo := conversation.NewSQLiteRepository(store, 32000, 24000, "default", "", nil)
	events := &conversation.StoreEventAdapter{Store: store}
	builder := &processBuilder{dir: dir, root: root, target: target}
	var runBuilder conversation.RunBuilder = builder
	if os.Getenv("B2C_REAL_LOOP") == "1" {
		runBuilder = &actualLoopBuilder{provider: &scriptedLoopProvider{steps: map[string]int{}, root: root}, dir: dir, root: root}
	}
	executor := conversation.NewTurnExecutor(repo, events, conversation.NewActiveTurnRegistry(), conversation.NewSubscriptionManager(), runBuilder)
	builder.executor = executor
	defer executor.Shutdown()
	if err = executor.ReconcileInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	tasks := store.TaskRepository("offline-server")
	actor := collab.Principal{ID: "runtime-owner", LocalSessions: true}
	svc, err := collabflux.OpenService(context.Background(), filepath.Join(dir, "managed", "flux.db"), collabflux.ServiceOptions{Tasks: tasks, Actor: actor, Dispatch: func(ctx context.Context, ref collab.ManagedTurnRef) error {
		_, err := executor.AcceptManagedDispatch(ctx, context.Background(), actor, ref)
		return err
	}, Checkpoint: func(stage, id string) { processCheckpoint(dir, target, stage, id) }})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	auth := server.ServerAuth{Enabled: true, Token: fixtureToken}
	base := server.NewMux(repo, events, executor, server.MuxOptions{ServerAuth: auth, RuntimeInfo: server.RuntimeInfo{Schema: "runtime-info/v1", ServerID: "offline-server"}})
	handler := server.WithManagedTasks(base, tasks, svc, auth)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := tasks.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	info, _ := json.Marshal(map[string]any{"endpoint": "http://" + lis.Addr().String(), "store_id": identity.StoreID, "pid": os.Getpid()})
	if err = durableFile(filepath.Join(dir, "ready.json"), info); err != nil {
		t.Fatal(err)
	}
	if err = http.Serve(lis, handler); err != nil {
		t.Fatal(err)
	}
}

func TestManagedBridgeProcess(t *testing.T) {
	if os.Getenv("CODEAGENT_B2C_HELPER") != "bridge" {
		t.Skip("subprocess helper")
	}
	var c *mcpbridge.Client
	var err error
	id := os.Getenv("B2C_SESSION")
	ctx := context.Background()
	if id == "" {
		c, _, err = mcpbridge.NewFromManifest(ctx, os.Getenv("B2C_MANIFEST"), os.Getenv("B2C_ROOT"))
	} else {
		c, _, err = mcpbridge.NewReattachedFromManifest(ctx, os.Getenv("B2C_MANIFEST"), os.Getenv("B2C_ROOT"), id, os.Getenv("B2C_BASELINE"))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "managed Bridge startup:", err)
		os.Exit(3)
	}
	defer c.Close()
	if err = mcpbridge.Server(c).Run(ctx, &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "managed Bridge protocol:", err)
		os.Exit(3)
	}
}

type processFixture struct {
	mu                  sync.Mutex
	t                   *testing.T
	dir, root, baseline string
	daemon              *exec.Cmd
	manifest            string
	client              *mcp.ClientSession
	sessionID, taskID   string
	run                 collab.StartRunInput
	realLoop            bool
}

func newProcessFixture(t *testing.T, stop string) *processFixture {
	t.Helper()
	f := &processFixture{t: t, dir: t.TempDir(), root: t.TempDir(), realLoop: stop == "real_loop"}
	t.Cleanup(func() {
		f.closeBridge()
		if f.daemon != nil {
			_ = f.daemon.Process.Kill()
			_ = f.daemon.Wait()
		}
		for _, name := range []string{"daemon.log", "bridge.log"} {
			data, _ := os.ReadFile(filepath.Join(f.dir, name))
			if strings.Contains(string(data), "WARNING: DATA RACE") {
				t.Errorf("race detector reported a race in %s", name)
			}
		}
		if evidence := os.Getenv("CODEAGENT_B2C_EVIDENCE_DIR"); evidence != "" {
			out := filepath.Join(evidence, strings.ReplaceAll(t.Name(), "/", "__"))
			_ = os.MkdirAll(out, 0700)
			for _, name := range []string{"daemon.log", "bridge.log", "effects", "checkpoint", "approval.json", "result.json"} {
				if data, err := os.ReadFile(filepath.Join(f.dir, name)); err == nil {
					_ = durableFile(filepath.Join(out, name), data)
				}
			}
		}
	})
	dir, err := filepath.EvalSymlinks(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	f.dir = dir
	if err = os.Chmod(f.dir, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(f.root)
	if err != nil {
		t.Fatal(err)
	}
	f.root = root
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", f.root}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatal(string(out), err)
		}
		return strings.TrimSpace(string(out))
	}
	git("-c", "init.defaultBranch=main", "init", "--template=")
	if err = os.WriteFile(filepath.Join(f.root, "main.txt"), []byte("baseline\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "--", "main.txt")
	git("-c", "user.name=Offline", "-c", "user.email=offline@example.invalid", "-c", "core.hooksPath=/dev/null", "commit", "-m", "baseline")
	f.baseline = git("rev-parse", "HEAD")
	f.start(stop)
	f.connect()
	return f
}

func (f *processFixture) start(stop string) {
	f.t.Helper()
	_ = os.Remove(filepath.Join(f.dir, "ready.json"))
	_ = os.Remove(filepath.Join(f.dir, "checkpoint"))
	exe, err := os.Executable()
	if err != nil {
		f.t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestManagedDaemonProcess$")
	cmd.Env = append(os.Environ(), "CODEAGENT_B2C_HELPER=daemon", "B2C_STATE="+f.dir, "B2C_ROOT="+f.root, "B2C_STOP="+stop)
	if f.realLoop {
		cmd.Env = append(cmd.Env, "B2C_REAL_LOOP=1")
	}
	log, err := os.OpenFile(filepath.Join(f.dir, "daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		f.t.Fatal(err)
	}
	cmd.Stdout = log
	cmd.Stderr = log
	if err = cmd.Start(); err != nil {
		f.t.Fatal(err)
	}
	log.Close()
	f.daemon = cmd
	var info struct {
		Endpoint string `json:"endpoint"`
		Store    string `json:"store_id"`
		PID      int    `json:"pid"`
	}
	f.wait(func() bool {
		b, err := os.ReadFile(filepath.Join(f.dir, "ready.json"))
		return err == nil && json.Unmarshal(b, &info) == nil
	})
	f.manifest = filepath.Join(f.dir, "connection.json")
	raw, _ := json.Marshal(map[string]any{"schema_version": 1, "endpoint": info.Endpoint, "server_id": "offline-server", "store_id": info.Store, "instance_id": "offline-instance", "daemon_pid": info.PID, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "access_token": fixtureToken})
	if err = durableFile(f.manifest, raw); err != nil {
		f.t.Fatal(err)
	}
}

func (f *processFixture) connect() {
	f.t.Helper()
	exe, err := os.Executable()
	if err != nil {
		f.t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestManagedBridgeProcess$")
	cmd.Env = append(os.Environ(), "CODEAGENT_B2C_HELPER=bridge", "B2C_MANIFEST="+f.manifest, "B2C_ROOT="+f.root, "B2C_SESSION="+f.sessionID, "B2C_BASELINE="+f.baseline)
	log, logErr := os.OpenFile(filepath.Join(f.dir, "bridge.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if logErr != nil {
		f.t.Fatal(logErr)
	}
	defer log.Close()
	cmd.Stderr = log
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "offline-codex-planner-reviewer"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		data, _ := os.ReadFile(filepath.Join(f.dir, "bridge.log"))
		f.t.Fatal(err, string(data))
	}
	f.mu.Lock()
	f.client = client
	f.mu.Unlock()
}
func (f *processFixture) closeBridge() {
	f.mu.Lock()
	client := f.client
	f.client = nil
	f.mu.Unlock()
	if client != nil {
		_ = client.Close()
	}
}
func (f *processFixture) call(name string, in, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f.mu.Lock()
	client := f.client
	f.mu.Unlock()
	if client == nil {
		return errors.New("Bridge is disconnected")
	}
	r, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: in})
	if err != nil {
		return err
	}
	if r.IsError {
		body, _ := json.Marshal(r.Content)
		return fmt.Errorf("%s: %s", name, body)
	}
	b, err := json.Marshal(r.StructuredContent)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
func (f *processFixture) must(name string, in, out any) {
	f.t.Helper()
	if err := f.call(name, in, out); err != nil {
		f.t.Fatal(err)
	}
}
func (f *processFixture) wait(fn func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	log, _ := os.ReadFile(filepath.Join(f.dir, "daemon.log"))
	if len(log) > 6000 {
		log = log[len(log)-6000:]
	}
	f.t.Fatalf("timeout at subprocess boundary\n%s", log)
}
func (f *processFixture) setupTask() {
	f.t.Helper()
	var local mcpbridge.SessionRef
	f.must("codeagent_create_session", map[string]any{"request_id": "session"}, &local)
	f.sessionID = local.ID
	var task collab.Receipt
	f.must("codeagent_create_task", map[string]any{"request_id": "task", "goal": "offline managed task", "acceptance": []string{"validate after edit"}}, &task)
	f.taskID = task.ID
	var identity struct {
		Store string `json:"store_id"`
	}
	b, _ := os.ReadFile(f.manifest)
	_ = json.Unmarshal(b, &identity)
	var executor collab.Receipt
	f.must("codeagent_add_task_participation", map[string]any{"task_id": task.ID, "participation": collab.ParticipationInput{RequestID: "executor", Role: "executor", SessionRef: collab.SessionRef{Kind: "local", ServerID: "offline-server", StoreID: identity.Store, SessionID: local.ID}}}, &executor)
	var ext collab.SessionRef
	f.must("codeagent_register_task_agent", collab.ExternalInput{RequestID: "external", RuntimeKind: "codex"}, &ext)
	parts := map[string]string{}
	for _, role := range []string{"planner", "reviewer"} {
		var part collab.Receipt
		f.must("codeagent_add_task_participation", map[string]any{"task_id": task.ID, "participation": collab.ParticipationInput{RequestID: role, Role: role, SessionRef: ext}}, &part)
		parts[role] = part.ID
	}
	var plan collab.Receipt
	f.must("codeagent_report_task", map[string]any{"task_id": task.ID, "kind": "plan", "report": collab.ReportInput{RequestID: "plan", ParticipationID: parts["planner"], SubmissionID: "plan", Iteration: 1, Content: "update main.txt then run git diff --check"}}, &plan)
	f.run = collab.StartRunInput{RequestID: "run", PlanArtifactID: plan.ID, ExecutorParticipationID: executor.ID, ReviewerParticipationID: parts["reviewer"], MaxIterations: 2, Files: []string{"main.txt"}, ValidationCommands: []string{"git diff --check"}}
}
func (f *processFixture) snapshot() collab.Snapshot {
	f.t.Helper()
	var out collab.Snapshot
	f.must("codeagent_read_task", map[string]any{"task_id": f.taskID}, &out)
	if data, err := json.Marshal(out); err == nil {
		_ = durableFile(filepath.Join(f.dir, "result.json"), data)
	}
	return out
}
func (f *processFixture) waitReview(n int) collab.Snapshot {
	f.t.Helper()
	var snap collab.Snapshot
	f.wait(func() bool {
		snap = f.snapshot()
		return snap.ManagedRun != nil && snap.ManagedRun.State == "reviewing" && snap.ManagedRun.CurrentIteration == n
	})
	return snap
}
func (f *processFixture) review(snap collab.Snapshot, verdict string) error {
	var bundle collab.ReviewBundle
	f.must("codeagent_read_review_bundle", map[string]any{"task_id": f.taskID, "artifact_id": snap.ManagedRun.CurrentBundle.ArtifactID}, &bundle)
	if bundle.ValidationStatus != "evidence_complete" {
		f.t.Fatal(bundle.ValidationStatus)
	}
	in := collab.ReviewInput{RequestID: fmt.Sprintf("review-%d", bundle.Iteration), SubmissionID: fmt.Sprintf("submission-%d", bundle.Iteration), RunID: snap.ManagedRun.RunID, Epoch: snap.ManagedRun.Epoch, ReviewerParticipationID: snap.ManagedRun.ReviewerParticipationID, AttemptID: bundle.AttemptID, Plan: bundle.Plan, Artifact: *snap.ManagedRun.CurrentBundle, Verdict: verdict, Findings: []string{"improve edge case"}}
	var receipt collab.Receipt
	return f.call("codeagent_review_task_run", map[string]any{"task_id": f.taskID, "run_id": snap.ManagedRun.RunID, "review": in}, &receipt)
}
func (f *processFixture) effects() int {
	b, _ := os.ReadFile(filepath.Join(f.dir, "effects"))
	n, _ := strconv.Atoi(string(b))
	return n
}
func (f *processFixture) killRestart() {
	f.t.Helper()
	if err := f.daemon.Process.Kill(); err != nil {
		f.t.Fatal(err)
	}
	_ = f.daemon.Wait()
	status, ok := f.daemon.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		f.t.Fatal("fault injection did not terminate helper with SIGKILL")
	}
	f.daemon = nil
	f.closeBridge()
	f.start("")
	f.connect()
}
func (f *processFixture) checkpoint() {
	f.t.Helper()
	f.wait(func() bool { _, err := os.Stat(filepath.Join(f.dir, "checkpoint")); return err == nil })
}

func TestManagedDaemonMCPFullLoopAndBridgeRestart(t *testing.T) {
	f := newProcessFixture(t, "real_loop")
	f.setupTask()
	var receipt collab.Receipt
	f.must("codeagent_start_task_run", map[string]any{"task_id": f.taskID, "run": f.run}, &receipt)
	first := f.waitReview(1)
	var duplicate collab.Receipt
	f.must("codeagent_start_task_run", map[string]any{"task_id": f.taskID, "run": f.run}, &duplicate)
	if duplicate.ID != receipt.ID || f.snapshot().ManagedRun.RunID != first.ManagedRun.RunID || f.effects() != 1 {
		t.Fatal("repeated start allocated another run or implementation", receipt, duplicate)
	}
	f.closeBridge()
	f.connect()
	if err := f.review(first, "request_changes"); err != nil {
		t.Fatal(err)
	}
	second := f.waitReview(2)
	if err := f.review(second, "pass"); err != nil {
		t.Fatal(err)
	}
	f.wait(func() bool { return f.snapshot().TaskAcceptance == "accepted" })
	if f.effects() != 2 {
		t.Fatal("unexpected side effect count", f.effects())
	}
	before := f.snapshot()
	f.killRestart()
	after := f.snapshot()
	if after.TaskAcceptance != "accepted" || after.ManagedRun.RunID != before.ManagedRun.RunID || f.effects() != 2 {
		t.Fatal("completed task replayed")
	}
	var page collab.EventPage
	f.must("codeagent_read_task_events", map[string]any{"task_id": f.taskID, "after": before.SnapshotTaskSeq, "limit": 200}, &page)
	if len(page.Events) != 0 {
		t.Fatal("completed recovery emitted a new milestone")
	}
}

func TestManagedDaemonSIGKILLRecoveryWindows(t *testing.T) {
	for _, stage := range []string{"run_prepared", "flux_submitted", "dispatch_reserved", "turn_admitted", "side_effect", "approval_wait", "turn_complete", "bundle_captured", "review_recorded", "gate_consumed"} {
		t.Run(stage, func(t *testing.T) {
			f := newProcessFixture(t, stage)
			f.setupTask()
			result := make(chan error, 2)
			go func() {
				var receipt collab.Receipt
				result <- f.call("codeagent_start_task_run", map[string]any{"task_id": f.taskID, "run": f.run}, &receipt)
			}()
			if stage == "review_recorded" || stage == "gate_consumed" {
				first := f.waitReview(1)
				go func() { result <- f.review(first, "pass") }()
			}
			f.checkpoint()
			before := f.effects()
			f.killRestart()
			if stage == "side_effect" || stage == "approval_wait" || stage == "turn_admitted" {
				snap := f.snapshot()
				if snap.WaitReason != "recovery_required" || snap.ManagedRun.State != "recovery_required" || snap.ManagedRun.Epoch != "2" || f.effects() != before {
					t.Fatal("unknown execution replayed", snap.CodingTask, before, f.effects())
				}
				time.Sleep(200 * time.Millisecond)
				if f.effects() != before {
					t.Fatal("late replay")
				}
				return
			}
			if stage == "review_recorded" || stage == "gate_consumed" {
				f.wait(func() bool { return f.snapshot().TaskAcceptance == "accepted" })
			} else {
				snap := f.waitReview(1)
				if err := f.review(snap, "pass"); err != nil {
					t.Fatal(err)
				}
				f.wait(func() bool { return f.snapshot().TaskAcceptance == "accepted" })
			}
			if f.effects() != 1 {
				t.Fatal("recovery duplicated an implementation", before, f.effects())
			}
		})
	}
}

func TestManagedDaemonDoesNotFinalizeChangedCandidateAfterSIGKILL(t *testing.T) {
	f := newProcessFixture(t, "gate_consumed")
	f.setupTask()
	var receipt collab.Receipt
	f.must("codeagent_start_task_run", map[string]any{"task_id": f.taskID, "run": f.run}, &receipt)
	snap := f.waitReview(1)
	result := make(chan error, 1)
	go func() { result <- f.review(snap, "pass") }()
	f.checkpoint()
	// External editors are outside Runtime leases. Their changed candidate must
	// still fail finalization, even though the original review already passed.
	if err := durableFile(filepath.Join(f.root, "main.txt"), []byte("external change after review\n")); err != nil {
		t.Fatal(err)
	}
	f.killRestart()
	f.wait(func() bool { return f.snapshot().WaitReason == "candidate_invalidated" })
	after := f.snapshot()
	if after.TaskAcceptance == "accepted" || after.State != "waiting" || f.effects() != 1 || len(after.Attempts) != len(snap.Attempts) || after.CurrentAttemptID != snap.CurrentAttemptID {
		t.Fatal("a different candidate was accepted or the Turn replayed", after, f.effects())
	}
	original := map[string]collab.SourceRef{}
	turns := 0
	for _, attempt := range snap.Attempts {
		original[attempt.AttemptID] = attempt.SourceRef
		if attempt.SourceRef.TurnID != "" {
			turns++
		}
	}
	if turns != 1 {
		t.Fatal("expected one execution Turn in addition to the reported plan", snap.Attempts)
	}
	for _, attempt := range after.Attempts {
		if source, ok := original[attempt.AttemptID]; !ok || source != attempt.SourceRef {
			t.Fatal("attempt identity changed during recovery", attempt)
		}
	}
}
