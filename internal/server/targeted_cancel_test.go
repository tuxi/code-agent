package server

import (
	"context"
	"testing"
)

type targetedCommands struct {
	*fakeCommands
	turn string
}

func (c *targetedCommands) CancelTurn(_ context.Context, turn string) error {
	c.turn = turn
	return nil
}

func TestRouterTargetedCancelNeverFallsBackToCurrentTurn(t *testing.T) {
	commands := &targetedCommands{fakeCommands: newFakeCommands()}
	Router{Commands: commands}.Route(context.Background(), []byte(`{"type":"cancel_turn","turn_id":"old"}`))
	if commands.turn != "old" {
		t.Fatal("target lost")
	}
	select {
	case <-commands.canceled:
		t.Fatal("targeted stop also cancelled current turn")
	default:
	}
	legacy := newFakeCommands()
	Router{Commands: legacy}.Route(context.Background(), []byte(`{"type":"cancel_turn","turn_id":"old"}`))
	select {
	case <-legacy.canceled:
		t.Fatal("unsupported target fell back to current turn")
	default:
	}
	for _, data := range []string{`{"type":"cancel_turn","turn_id":null}`, `{"type":"cancel_turn","turn_id":""}`} {
		Router{Commands: legacy}.Route(context.Background(), []byte(data))
		select {
		case <-legacy.canceled:
			t.Fatal("invalid target fell back to current turn")
		default:
		}
	}
}
