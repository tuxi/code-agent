package server

import (
	"context"
	"encoding/json"
	"github.com/coder/websocket"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/conversation"
	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/session/sqlite"
)

type validationCommands struct {
	*fakeCommands
	supported bool
	contracts chan *session.ValidationContract
}

func (c *validationCommands) SupportsValidationContracts() bool { return c.supported }
func (c *validationCommands) SendMessageWithValidationContract(_ context.Context, request, text, model, effort string, _ []model.GatewayAssetRef, _ []model.LocalAssetRef, contract *session.ValidationContract) (agent.TurnResult, error) {
	c.contracts <- contract
	return agent.TurnResult{}, nil
}

func TestValidationContractRouterRejectsBeforeDispatch(t *testing.T) {
	for _, data := range []string{
		`{"type":"agent_input","kind":"text","request_id":"r","text":"task","validation_contract":{"version":2},"validation_contract":{"version":1}}`,
		`{"type":"agent_input","kind":"text","text":"task","validation_contract":{"version":1}}`,
		`{"type":"agent_input","kind":"text","request_id":"bad request","text":"task","validation_contract":{"version":1}}`,
		`{"type":"agent_input","kind":"text","request_id":"r","text":"task","validation_contract":null}`,
		`{"type":"agent_input","kind":"text","request_id":"r","text":"task","validation_contract":{"version":2}}`,
		`{"type":"agent_input","kind":"text","request_id":"r","text":"task","validation_contract":{"version":1,"max_tokens":1}}`,
		`{"type":"agent_input","kind":"text","request_id":"r","text":"task","validation_contract":{"version":2,"version":1}}`,
		`{"type":"agent_input","kind":"command","request_id":"r","text":"cancel","validation_contract":{"version":1}}`,
	} {
		commands := &validationCommands{fakeCommands: newFakeCommands(), supported: true, contracts: make(chan *session.ValidationContract, 1)}
		rejected := &rejectionRecorder{ch: make(chan AgentInputRejected, 1)}
		Router{Commands: commands, Rejections: rejected}.Route(context.Background(), []byte(data))
		select {
		case r := <-rejected.ch:
			if r.Error.Code != "invalid_validation_contract" {
				t.Fatal(r)
			}
		case <-time.After(time.Second):
			t.Fatal("invalid contract did not reject")
		}
		if len(commands.contracts) != 0 || len(commands.text) != 0 || len(commands.canceled) != 0 {
			t.Fatal("invalid contract executed")
		}
	}
}

func TestValidationContractRouterNoLegacyFallback(t *testing.T) {
	data := []byte(`{"type":"agent_input","kind":"text","request_id":"r","text":"task","validation_contract":{"version":1,"validation_commands":["swift test"],"manual_checks":["check UI"]}}`)
	for _, supported := range []bool{false, true} {
		commands := &validationCommands{fakeCommands: newFakeCommands(), supported: supported, contracts: make(chan *session.ValidationContract, 1)}
		rejected := &rejectionRecorder{ch: make(chan AgentInputRejected, 1)}
		Router{Commands: commands, Rejections: rejected}.Route(context.Background(), data)
		if supported {
			select {
			case c := <-commands.contracts:
				if c.Version != 1 || c.ValidationCommands[0] != "swift test" || c.ManualChecks[0] != "check UI" {
					t.Fatal(c)
				}
			case <-time.After(time.Second):
				t.Fatal("contract not forwarded")
			}
		} else {
			if r := <-rejected.ch; r.Error.Code != "validation_contract_unsupported" {
				t.Fatal(r)
			}
		}
		if len(commands.text) != 0 {
			t.Fatal("fell back to legacy dispatch")
		}
	}
	legacy := newFakeCommands()
	rejected := &rejectionRecorder{ch: make(chan AgentInputRejected, 1)}
	Router{Commands: legacy, Rejections: rejected}.Route(context.Background(), data)
	if r := <-rejected.ch; r.Error.Code != "validation_contract_unsupported" || len(legacy.text) != 0 {
		t.Fatal(r)
	}
}

func TestValidationContractCapabilityUsesActualStore(t *testing.T) {
	store, err := sqlite.New(filepath.Join(t.TempDir(), "capabilities.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, tc := range []struct {
		store session.Store
		want  bool
	}{{session.NewMemoryStore(), false}, {store, true}} {
		repo := conversation.NewSQLiteRepository(tc.store, 32000, 24000, "local", "", nil)
		executor := conversation.NewTurnExecutor(repo, &conversation.StoreEventAdapter{Store: tc.store}, conversation.NewActiveTurnRegistry(), conversation.NewSubscriptionManager(), nil)
		defer executor.Shutdown()
		h := NewMux(repo, &conversation.StoreEventAdapter{Store: tc.store}, executor, MuxOptions{Capabilities: []string{"durable_validation_contract_v1"}, CapabilityResolver: func(context.Context) []string { return []string{"durable_validation_contract_v1"} }, RuntimeCapabilities: RuntimeCapabilities{DurableValidationContract: true}})
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest("GET", "/v1/runtime/capabilities", nil))
		var response struct {
			Data runtimeCapabilitiesResponse `json:"data"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Data.Capabilities.DurableValidationContract != tc.want {
			t.Fatal(r.Body.String())
		}
		if got := conversation.NewTransportSession("s", executor).SupportsValidationContracts(); got != tc.want {
			t.Fatal("transport capability disagrees", got)
		}
		sess, err := repo.Create(context.Background(), t.TempDir(), "")
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(h)
		func() {
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/conversations/"+sess.ID+"/stream", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			_, raw, err := conn.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var hello struct {
				Capabilities []string `json:"capabilities"`
			}
			if json.Unmarshal(raw, &hello) != nil || hasCapability(hello.Capabilities, "durable_validation_contract_v1") != tc.want {
				t.Fatal(string(raw))
			}
		}()

	}
	r := httptest.NewRecorder()
	NewMux(newFakeConversationRepo(), &fakeEventStore{}, nil, MuxOptions{RuntimeCapabilities: RuntimeCapabilities{DurableValidationContract: true}}).ServeHTTP(r, httptest.NewRequest("GET", "/v1/runtime/capabilities", nil))
	var response struct {
		Data runtimeCapabilitiesResponse `json:"data"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &response); err != nil || response.Data.Capabilities.DurableValidationContract {
		t.Fatal(r.Body.String(), err)
	}
}

func TestValidationContractWireRoundTrip(t *testing.T) {
	c := &session.ValidationContract{Version: 1, ValidationCommands: []string{"swift test"}}
	hash, _ := c.Hash()
	e := agent.Event{Kind: agent.EventTurnAccepted, SessionID: "s", TurnID: "t", RequestID: "r", Seq: 9, ValidationContract: c, ValidationContractHash: hash}
	encoded, err := Encode(e, "", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := session.AcceptedValidation(session.EventRecord{SessionID: "s", TurnID: "t", Kind: "turn_accepted", Payload: encoded}, session.TurnInput{SessionID: "s", TurnID: "t", RequestID: "r"})
	if err != nil || got == nil || got.ValidationCommands[0] != "swift test" {
		t.Fatal(string(encoded), err)
	}
}
