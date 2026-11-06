package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/conversation"
	"code-agent/internal/session"
	"code-agent/internal/session/sqlite"

	"github.com/coder/websocket"
)

type backgroundTestSession struct {
	configured, supported bool
	stateErr              error
	state                 *agent.BackgroundContinuationInfo
}

type backgroundSocketSession struct {
	*testSession
	*backgroundTestSession
}

func TestBackgroundContinuationActivityCarriesTargetWithoutLiveWorker(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repo := conversation.NewSQLiteRepository(store, 1000, 900, "model", "", nil)
	sess, err := repo.Create(ctx, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AdmitContinuationWork(ctx, sess.ID, "work"); err != nil {
		t.Fatal(err)
	}
	_, err = store.ParkContinuation(ctx, sess, session.Continuation{SessionID: sess.ID, TurnID: "work", Await: []string{"job:x:finished"}, Binding: json.RawMessage(`{}`), Checkpoint: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureConversationIdle(ctx, repo, sess.ID); err == nil {
		t.Fatal("waiting work may not be deleted or archived as idle")
	}
	for _, state := range []string{"waiting_background", "background_needs_attention"} {
		if state == "background_needs_attention" {
			if _, err := store.EndContinuation(ctx, sess.ID, "work", "needs_attention"); err != nil {
				t.Fatal(err)
			}
		}
		recorder := httptest.NewRecorder()
		NewMux(repo, &fakeEventStore{}, nil, MuxOptions{}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/activity", nil))
		var response activityResponse
		decodeResponse(t, recorder.Result(), &response)
		if len(response.Sessions) != 1 || response.Sessions[0].State != state || response.Sessions[0].TurnID != "work" || response.Sessions[0].ActiveTurnID != "work" {
			t.Fatalf("lost parked identity: %s", recorder.Body.String())
		}
	}
}

func TestBackgroundContinuationSocketNegotiation(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[configured], func(t *testing.T) {
			s := &backgroundSocketSession{
				testSession:           &testSession{hub: newTestHub()},
				backgroundTestSession: &backgroundTestSession{configured: configured, state: &agent.BackgroundContinuationInfo{TurnID: "work", State: "waiting", Revision: 4, JobIDs: []string{"job_x"}}},
			}
			h := &WSHandler{Resolve: func(*http.Request) (Session, error) { return s, nil }, Accept: &websocket.AcceptOptions{InsecureSkipVerify: true}}
			srv := httptest.NewServer(h)
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			_, data, err := conn.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var hello struct {
				Capabilities []string `json:"capabilities"`
			}
			if err := json.Unmarshal(data, &hello); err != nil {
				t.Fatal(err)
			}
			if hasCapability(hello.Capabilities, BackgroundContinuationCapability) != configured {
				t.Fatalf("incorrect hello: %s", data)
			}
			if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"client_capabilities","capabilities":["background_job_notifications_v1"]}`)); err != nil {
				t.Fatal(err)
			}
			_, data, err = conn.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var ack struct {
				Type       string                            `json:"type"`
				Enabled    bool                              `json:"enabled"`
				TurnID     string                            `json:"turn_id"`
				Background *agent.BackgroundContinuationInfo `json:"background"`
			}
			if err := json.Unmarshal(data, &ack); err != nil {
				t.Fatal(err)
			}
			if ack.Type != "background_continuation_ready" || ack.Enabled != configured || ack.TurnID != "work" || ack.Background == nil || ack.Background.Revision != 4 {
				t.Fatalf("bad acknowledgement: %s", data)
			}
		})
	}
}

func (s *backgroundTestSession) BackgroundContinuationEnabled(context.Context) bool {
	return s.configured
}
func (s *backgroundTestSession) SetBackgroundContinuationSupport(v bool) { s.supported = v }
func (s *backgroundTestSession) BackgroundContinuationState(context.Context) (string, *agent.BackgroundContinuationInfo, error) {
	if s.state == nil {
		return "", nil, s.stateErr
	}
	return s.state.TurnID, s.state, s.stateErr
}

func TestBackgroundContinuationRequiresBothEndsAndReadableState(t *testing.T) {
	for _, tc := range []struct {
		name                                         string
		configured, advertised, client, broken, want bool
	}{
		{"both", true, true, true, false, true},
		{"legacy_client", true, true, false, false, false},
		{"legacy_host", true, false, true, false, false},
		{"disabled", false, true, true, false, false},
		{"unreadable", true, true, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &WSHandler{}
			s := &backgroundTestSession{configured: tc.configured}
			if tc.broken {
				s.stateErr = errors.New("unavailable")
			}
			sink := &controlTestSink{}
			_, _, rev := h.claimSessionControl("s", sink)
			var caps []string
			if tc.client {
				caps = []string{BackgroundContinuationCapability}
			}
			router := Router{ClientCapabilities: func(ctx context.Context, values []string) {
				h.configureBackground(ctx, "s", rev, s, sink, values, tc.advertised)
			}}
			frame, _ := json.Marshal(map[string]any{"type": "client_capabilities", "capabilities": caps})
			router.Route(context.Background(), frame)
			if s.supported != tc.want {
				t.Fatalf("support=%v want %v", s.supported, tc.want)
			}
		})
	}
}

func TestBackgroundContinuationLatestConnectionOwnsOptIn(t *testing.T) {
	h := &WSHandler{}
	s := &backgroundTestSession{configured: true}
	sink := &controlTestSink{}
	reset := func() { s.SetBackgroundContinuationSupport(false) }
	a, _, old := h.claimSessionControl("s", sink, reset)
	caps := []string{BackgroundContinuationCapability}
	h.configureBackground(context.Background(), "s", old, s, sink, caps, true)
	if !s.supported {
		t.Fatal("opt in failed")
	}
	_, _, current := h.claimSessionControl("s", sink, reset)
	if s.supported {
		t.Fatal("new connection inherited old consent")
	}
	h.configureBackground(context.Background(), "s", old, s, sink, caps, true)
	if s.supported {
		t.Fatal("old connection re-enabled support")
	}
	h.configureBackground(context.Background(), "s", current, s, sink, caps, true)
	h.releaseSessionControl("s", old, a, reset)
	if !s.supported {
		t.Fatal("old disconnect revoked new consent")
	}
	h.releaseSessionControl("s", current, a, reset)
	if s.supported {
		t.Fatal("current disconnect left support enabled")
	}
}

func TestBackgroundContinuationWirePreservesDurableRevision(t *testing.T) {
	for _, kind := range []agent.EventKind{agent.EventTurnWaitingBackground, agent.EventTurnBackgroundResumed, agent.EventTurnBackgroundBlocked} {
		info := &agent.BackgroundContinuationInfo{TurnID: "turn", State: "waiting", Revision: 7, JobIDs: []string{"job_x"}}
		data, err := json.Marshal(toWire(agent.Event{Kind: kind, TurnID: "turn", Background: info}))
		if err != nil {
			t.Fatal(err)
		}
		var frame struct {
			Background *agent.BackgroundContinuationInfo `json:"background"`
		}
		if err := json.Unmarshal(data, &frame); err != nil {
			t.Fatal(err)
		}
		if frame.Background == nil || frame.Background.Revision != 7 || frame.Background.TurnID != "turn" {
			t.Fatalf("lost lifecycle: %s", data)
		}
	}
}
