package server

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestMessageContractGolden locks the inbound command/control message shapes —
// the contract Mac / iOS / Web clients encode against. Any field rename or
// envelope change shows up as a golden diff in CI. Regenerate after an
// intentional change with: go test ./internal/server -run Golden -update.
func TestMessageContractGolden(t *testing.T) {
	cases := map[string]any{
		// control plane
		"approval_request":  NewApprovalRequest("appr_1", "sess_root", "turn_7", "run_command", `{"command":"git push"}`, 120000),
		"approval_response": ApprovalResponse{Type: MsgTypeApprovalResponse, ID: "appr_1", Approved: true},
		// command plane
		"send_message": SendMessage{Type: MsgTypeSendMessage, Text: "迁移到新 API"},
		"cancel_turn":  CancelTurn{Type: MsgTypeCancelTurn},
		// plan approval
		"plan_approval_request": PlanApprovalRequest{
			Type: "plan_approval_request", ID: "plan_appr_1",
			SessionID: "sess_root", TurnID: "turn_7",
			PlanID: "plan_abc", Title: "Add Auth",
			Content: "# Plan\n1. Step one\n2. Step two",
			DeadlineMS: 120000,
		},
		"plan_approval_response": PlanApprovalResponse{
			Type: MsgTypePlanApprovalResponse, ID: "plan_appr_1", Approved: true,
		},
	}

	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(msg)
			if err != nil {
				t.Fatal(err)
			}
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, raw, "", "  "); err != nil {
				t.Fatal(err)
			}
			pretty.WriteByte('\n')
			path := filepath.Join("testdata", "messages", name+".json")

			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (run with -update to create): %v", err)
			}
			if !bytes.Equal(pretty.Bytes(), want) {
				t.Errorf("message %s contract drift\n got:\n%s\nwant:\n%s", name, pretty.Bytes(), want)
			}
		})
	}
}
