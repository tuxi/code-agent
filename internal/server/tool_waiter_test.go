package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"code-agent/internal/agent"
)

func TestWaiterDeliverUnblocksWait(t *testing.T) {
	w := NewRemoteToolResultWaiter()

	result := agent.ToolCallResult{Subtype: "result", Content: "done", IsError: false}

	// Deliver from another goroutine after a short delay.
	go func() {
		time.Sleep(50 * time.Millisecond)
		w.Deliver("call_1", result)
	}()

	got, err := w.Wait(context.Background(), "call_1", 5*time.Second)
	if err != nil {
		t.Fatalf("Wait returned error: %v", err)
	}
	if got.Content != "done" || got.IsError {
		t.Errorf("got %+v, want content=done isError=false", got)
	}
}

func TestWaiterTimeout(t *testing.T) {
	w := NewRemoteToolResultWaiter()

	got, err := w.Wait(context.Background(), "call_timeout", 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Wait returned unexpected error: %v", err)
	}
	if got.Content != "tool error: client timeout" || !got.IsError {
		t.Errorf("got %+v, want timeout error", got)
	}
}

func TestWaiterContextCancel(t *testing.T) {
	w := NewRemoteToolResultWaiter()

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := w.Wait(ctx, "call_cancel", 5*time.Second)
	if err == nil {
		t.Fatal("Wait should have returned context.Canceled error")
	}
	if err != context.Canceled {
		t.Errorf("got error %v, want context.Canceled", err)
	}
}

func TestWaiterCancelAll(t *testing.T) {
	w := NewRemoteToolResultWaiter()

	done := make(chan error, 3)

	// Start three waits.
	for i := 0; i < 3; i++ {
		go func(id string) {
			_, err := w.Wait(context.Background(), id, 10*time.Second)
			done <- err
		}(fmt.Sprintf("call_%d", i))
	}

	time.Sleep(50 * time.Millisecond) // let goroutines start

	w.CancelAll()

	// All three should unblock quickly.
	for i := 0; i < 3; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Logf("waiter %d returned error: %v", i, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("waiter %d did not unblock after CancelAll", i)
		}
	}
}

func TestWaiterDeliverAfterTimeout(t *testing.T) {
	w := NewRemoteToolResultWaiter()

	// Wait with a very short timeout.
	got, err := w.Wait(context.Background(), "call_late", 10*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.IsError {
		t.Fatal("should have timed out")
	}

	// Late delivery must not panic or block — the done channel guards it.
	w.Deliver("call_late", agent.ToolCallResult{Subtype: "result", Content: "too late"})
	// If we reach here without hanging, the test passes.
}

func TestWaiterDeliverUnknownCallID(t *testing.T) {
	w := NewRemoteToolResultWaiter()

	// Deliver to a callID that was never registered.
	w.Deliver("no_such_call", agent.ToolCallResult{Subtype: "result", Content: "orphan"})
	// Must not panic.
}

func TestWaiterMultipleCallsIsolated(t *testing.T) {
	w := NewRemoteToolResultWaiter()

	// Start deliver goroutines.
	go func() {
		time.Sleep(100 * time.Millisecond)
		w.Deliver("call_a", agent.ToolCallResult{Subtype: "result", Content: "result_a"})
	}()
	go func() {
		time.Sleep(50 * time.Millisecond)
		w.Deliver("call_b", agent.ToolCallResult{Subtype: "result", Content: "result_b"})
	}()

	// Wait calls must run in their own goroutines — Wait blocks.
	raCh := make(chan agent.ToolCallResult, 1)
	rbCh := make(chan agent.ToolCallResult, 1)
	go func() {
		r, _ := w.Wait(context.Background(), "call_a", 5*time.Second)
		raCh <- r
	}()
	go func() {
		r, _ := w.Wait(context.Background(), "call_b", 5*time.Second)
		rbCh <- r
	}()

	ra := <-raCh
	if ra.Content != "result_a" {
		t.Errorf("call_a = %q, want result_a", ra.Content)
	}
	rb := <-rbCh
	if rb.Content != "result_b" {
		t.Errorf("call_b = %q, want result_b", rb.Content)
	}
}
