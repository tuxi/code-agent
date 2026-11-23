package server

import (
	"testing"

	"code-agent/internal/agent"
	"code-agent/internal/followup"
	"code-agent/internal/session"
)

func TestTurnFinishedCarriesFollowUpOnWire(t *testing.T) {
	out, _ := wireJSON(t, agent.Event{
		Kind:   agent.EventTurnFinished,
		TurnID: "turn_1",
		FollowUp: &followup.Offer{
			ID: "fu_1", TurnID: "turn_1", Detail: "done", Question: "next?",
			Options:     []followup.Option{{ID: "opt_1", Label: "Go", Message: "Go", Recommended: true}, {ID: "opt_2", Label: "Stop", Message: "Stop"}},
			Dismissible: true,
		},
	})
	card, ok := out["follow_up"].(map[string]any)
	if !ok || card["id"] != "fu_1" || card["question"] != "next?" {
		t.Fatalf("follow_up = %#v", out["follow_up"])
	}
	options := card["options"].([]any)
	first := options[0].(map[string]any)
	if first["label"] != "Go" || first["message"] != "Go" || first["recommended"] != true {
		t.Fatalf("option = %#v", first)
	}
}

func TestTurnStartedCarriesFollowUpReplyOnWire(t *testing.T) {
	out, _ := wireJSON(t, agent.Event{
		Kind:          agent.EventTurnStarted,
		TurnID:        "turn_2",
		FollowUpReply: &session.FollowUpReply{TurnID: "turn_1", FollowUpID: "fu_1", OptionID: "opt_1"},
	})
	reply, ok := out["reply_to"].(map[string]any)
	if !ok || reply["turn_id"] != "turn_1" || reply["follow_up_id"] != "fu_1" || reply["option_id"] != "opt_1" {
		t.Fatalf("follow_up_reply = %#v", out["reply_to"])
	}
}

func TestFollowUpDismissedCarriesIdentity(t *testing.T) {
	out, _ := wireJSON(t, agent.Event{Kind: agent.EventFollowUpDismissed, TurnID: "turn_1", FollowUpID: "fu_1"})
	if out["kind"] != string(agent.EventFollowUpDismissed) || out["follow_up_id"] != "fu_1" {
		t.Fatalf("dismissed wire = %#v", out)
	}
}
