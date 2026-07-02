package server

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"code-agent/internal/agent"
	"code-agent/internal/assetref"
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
			Content:    "# Plan\n1. Step one\n2. Step two",
			DeadlineMS: 120000,
		},
		"plan_approval_response": PlanApprovalResponse{
			Type: MsgTypePlanApprovalResponse, ID: "plan_appr_1", Approved: true,
		},
		// v1.1 agent_input unified envelope
		"agent_input_text": AgentInput{
			Type: "agent_input", Kind: "text", Text: "分析这个项目",
		},
		"agent_input_tool_result": AgentInput{
			Type: "agent_input", Kind: "tool_result",
			ToolResult: &ToolResult{
				ToolUseID: "call_abc123",
				Subtype:   "result",
				Content:   "修剪完成",
				Output:    json.RawMessage(`{"kind":"file","asset_id":"asset_turn_1_call_abc123_001_9ad5b4c1"}`),
				Assets: []assets.Ref{{
					ID:                    "asset_turn_1_call_abc123_001_9ad5b4c1",
					Kind:                  "file",
					URI:                   "workspace://app-local/out.mp4",
					DisplayName:           "out.mp4",
					WorkspaceID:           "app-local",
					WorkspaceRelativePath: "out.mp4",
					MIMEType:              "video/mp4",
					SourceTurnID:          "turn_1",
					SourceCallID:          "call_abc123",
				}},
				IsError: false,
			},
		},
		"agent_input_command": AgentInput{
			Type: "agent_input", Kind: "command", Text: "cancel",
		},
		"agent_input_system": AgentInput{
			Type: "agent_input", Kind: "system", Command: "patch_context",
			CommandKey: "project_rules", CommandValue: "使用 Swift 6 规范",
		},
		// v1.1 client tool registration
		"register_tools": RegisterTools{
			Type: "register_tools",
			Tools: []agent.ClientToolDef{
				{Name: "get_device_info", Description: "获取当前设备的系统信息，包括系统版本、设备型号等", InputSchema: json.RawMessage(`{"type":"object","properties":{},"required":[]}`)},
			},
		},
		// v1.1 hello with capabilities
		"hello": helloFrame{
			Type: "hello", ProtocolVersion: 1, Server: "codeagent/deepseek-v4",
			Capabilities: []string{"streaming", "thinking", "tool_streaming", "plan_mode", "subagents", "session_resume", "client_tool_execution"},
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
