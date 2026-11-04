package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"code-agent/internal/executionevidence"
	"code-agent/internal/session"
)

func contractEvent(t *testing.T, id, turn, request string, contract session.ValidationContract) Event {
	t.Helper()
	hash, err := contract.Hash()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(contract)
	return Event{SessionID: id, TurnID: turn, RequestID: request, Seq: 10, Kind: "turn_accepted", ValidationContract: raw, ValidationContractHash: hash}
}

func TestDurableValidationManualRejectsUnsupportedBeforeSending(t *testing.T) {
	s, f, _, _ := manualFixture(t)
	ctx := context.Background()
	info, err := s.ConnectionInfo(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := s.Create(ctx, ManualCreateInput{ManualIdentity: ManualIdentity{ServerID: info.ServerID, StoreID: info.StoreID, Baseline: info.HeadCommit}, RequestID: "contract-create"})
	if err != nil {
		t.Fatal(err)
	}
	// Fake Runtime has no durable capability. The old local-only fallback
	// would accept and dispatch; the Manual profile must now reject instead.
	_, _, err = s.send(ctx, manualSendInput{manualTarget: manualTarget{BindingID: ref.BindingID}, SendInput: SendInput{SessionID: ref.ID, RequestID: "contract-request", Message: "task", Development: &executionevidence.Expectations{ValidationCommands: []string{"go version"}}}})
	var coded *BridgeError
	if !errors.As(err, &coded) || coded.Code != "validation_contract_unsupported" {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inputs[ref.ID] != 0 {
		t.Fatal("unsupported contract was dispatched")
	}
}

func TestDurableValidationRecoveryRejectsAmbiguousOrCorruptEvidence(t *testing.T) {
	c := &Client{binding: &connectionBinding{verifiedServerID: "server", verifiedStoreID: "store"}}
	valid := contractEvent(t, "s", "t", "r", session.ValidationContract{Version: 1, ValidationCommands: []string{"swift test"}})
	contract, p, err := c.validationFromEvents([]Event{valid}, "s", "t")
	if err != nil || contract == nil || p.ValidationSource != "persisted_turn_accepted" || p.ValidationContractRef.ServerID != "server" || p.ValidationContractRef.StoreID != "store" {
		t.Fatal(p, err)
	}
	for name, events := range map[string][]Event{
		"missing":         {{SessionID: "s", TurnID: "t", Seq: 20, Kind: "turn_finished"}},
		"duplicate":       {valid, {SessionID: "s", TurnID: "t", RequestID: "other", Seq: 20, Kind: "turn_accepted"}},
		"unordered":       {{SessionID: "s", TurnID: "other", Seq: 21, Kind: "turn_started"}, valid},
		"foreign":         {{SessionID: "foreign", TurnID: "t", RequestID: "r", Seq: 10, Kind: "turn_accepted"}},
		"missing hash":    {{SessionID: "s", TurnID: "t", RequestID: "r", Seq: 10, Kind: "turn_accepted", ValidationContract: valid.ValidationContract}},
		"bad hash":        {{SessionID: "s", TurnID: "t", RequestID: "r", Seq: 10, Kind: "turn_accepted", ValidationContract: valid.ValidationContract, ValidationContractHash: "wrong"}},
		"null":            {{SessionID: "s", TurnID: "t", RequestID: "r", Seq: 10, Kind: "turn_accepted", ValidationContract: json.RawMessage(`null`), ValidationContractHash: valid.ValidationContractHash}},
		"unknown version": {{SessionID: "s", TurnID: "t", RequestID: "r", Seq: 10, Kind: "turn_accepted", ValidationContract: json.RawMessage(`{"version":2}`), ValidationContractHash: valid.ValidationContractHash}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := c.validationFromEvents(events, "s", "t"); err == nil {
				t.Fatal("untrusted acceptance restored")
			}
		})
	}
}

func TestDurableValidationReadRestoresWithoutMemoryAndPreservesStatus(t *testing.T) {
	for _, status := range []string{"completed", "failed", "cancelled", "running", "waiting_for_human"} {
		t.Run(status, func(t *testing.T) {
			c, f := setup(t)
			ref, err := c.Create(context.Background(), "contract-read")
			if err != nil {
				t.Fatal(err)
			}
			accepted := contractEvent(t, ref.ID, "t", "r", session.ValidationContract{Version: 1, ValidationCommands: []string{"swift test"}, ManualChecks: []string{"check UI"}})
			kind := map[string]string{"completed": "turn_finished", "failed": "turn_failed", "cancelled": "turn_cancelled", "running": "turn_started", "waiting_for_human": "turn_started"}[status]
			f.mu.Lock()
			f.events = []Event{accepted,
				{SessionID: ref.ID, TurnID: "t", Seq: 11, Kind: "tool_started", CallID: "check", ToolName: "run_command", ToolArgs: map[string]any{"command": "swift test"}},
				{SessionID: ref.ID, TurnID: "t", Seq: 12, Kind: "tool_finished", CallID: "check", ToolName: "run_command", Output: map[string]any{"command": "swift test", "status": "completed", "exit_code": 0, "decision": "confirm"}},
				{SessionID: ref.ID, TurnID: "t", Seq: 13, Kind: kind}}
			f.mu.Unlock()
			if status == "waiting_for_human" {
				original := c.http.Transport
				c.http.Transport = uxRoundTrip(func(req *http.Request) (*http.Response, error) {
					if req.URL.Path == "/v1/activity" {
						body, _ := json.Marshal(map[string]any{"sessions": []any{map[string]any{"session_id": ref.ID, "pending_approval_count": 1, "state": "waiting_approval"}}})
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
					}
					return original.RoundTrip(req)
				})
			}
			r, err := c.Read(context.Background(), ref.ID, "t", 12)
			if err != nil || r.Status != status || r.ValidationSource != "persisted_turn_accepted" || r.ExecutionEvidence == nil || r.ExecutionEvidence.Status == "evidence_complete" || len(r.PendingManualChecks) != 1 || len(c.expectations) != 0 {
				t.Fatal(r, err)
			}
			if status != "running" && status != "waiting_for_human" {
				h, err := c.Handoff(context.Background(), ref.ID, "t")
				if err != nil || h.ExecutionStatus != status || h.ValidationStatus != "partial" || h.AcceptanceStatus != "not_reviewed" || h.ValidationContractRef.RequestID != "r" {
					t.Fatal(h, err)
				}
			} else if status == "waiting_for_human" {
				if r.Terminal || !r.AttentionRequired || r.ExecutionEvidence.Status != "attention_required" {
					t.Fatal(r)
				}
				if _, err := c.Handoff(context.Background(), ref.ID, "t"); err == nil {
					t.Fatal("attention became terminal handoff")
				}
			}
		})
	}
}

func TestDurableValidationCapabilityFailureNeverSends(t *testing.T) {
	for _, body := range []string{`{}`, `{"capabilities":{"durable_validation_contract_v1":false}}`, `not json`} {
		c, _ := setup(t)
		ref, err := c.Create(context.Background(), "contract-cap")
		if err != nil {
			t.Fatal(err)
		}
		original := c.http.Transport
		writes := 0
		c.http.Transport = uxRoundTrip(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/v1/runtime/capabilities" {
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
			}
			writes++
			return original.RoundTrip(r)
		})
		_, err = c.sendWithExpectations(context.Background(), ref.ID, "contract-r", "task", "", &executionevidence.Expectations{ValidationCommands: []string{"swift test"}}, true)
		if err == nil || writes != 0 || len(c.submissions) != 0 {
			t.Fatal("capability failure sent work", writes, err)
		}
	}
}

func TestDurableValidationLegacySourceNeverBecomesPersisted(t *testing.T) {
	c, f := setup(t)
	ctx := context.Background()
	ref, err := c.Create(ctx, "legacy-source")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := c.SendWithExpectations(ctx, ref.ID, "legacy-r", "task", "", &executionevidence.Expectations{ValidationCommands: []string{"swift test"}})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.events = append(f.events, Event{SessionID: ref.ID, TurnID: receipt.TurnID, Seq: 20, Kind: "turn_finished"})
	f.mu.Unlock()
	r, err := c.Read(ctx, ref.ID, receipt.TurnID, 0)
	if err != nil || r.ValidationSource != "legacy_process_local" || r.ValidationContractRef != nil || r.ExecutionEvidence == nil || len(r.ExecutionEvidence.MissingValidationCommands) != 1 {
		t.Fatal(r, err)
	}
	// No acceptance contract exists; losing the old cache cannot reconstruct
	// the original declaration from tools or assistant claims.
	c.mu.Lock()
	c.expectations = map[string]*executionevidence.Expectations{}
	c.mu.Unlock()
	h, err := c.Handoff(ctx, ref.ID, receipt.TurnID)
	if err != nil || h.ValidationSource != "not_declared" || h.ValidationStatus != "unknown" || h.Validation != nil || h.ValidationContractRef != nil {
		t.Fatal(h, err)
	}
}
