package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/jobs"
	"code-agent/internal/session"
	"code-agent/internal/truncate"
)

func (s *JobEventSink) notifyCompletion(snap jobs.Snapshot, event agent.Event) bool {
	if !snap.Owner.Notify {
		return false
	}
	store, ok := s.store.(session.RuntimeInputStore)
	if !ok {
		return false
	}
	// Structured facts are bounded before serialization. Output remains data;
	// neither its contents nor a command name can grant execution authority.
	data := struct {
		JobID      string      `json:"job_id"`
		Status     jobs.Status `json:"status"`
		ExitCode   int         `json:"exit_code"`
		DurationMS int64       `json:"duration_ms"`
		OutputTail string      `json:"output_tail"`
	}{snap.ID, snap.Status, snap.ExitCode, snap.DurationMS, truncate.Tail(snap.OutputTail, 2000)}
	content, _ := json.Marshal(data)
	// Control bytes can expand sixfold in JSON. Bound the actual serialized
	// body too, otherwise one binary-looking tail can poison the inbox.
	for len(content) > 7000 {
		data.OutputTail = truncate.Tail(data.OutputTail, len(data.OutputTail)/2)
		content, _ = json.Marshal(data)
	}
	input := session.RuntimeInput{ID: session.JobCompletionInputID(snap.ID), SessionID: snap.Owner.SessionID,
		SourceTurnID: snap.Owner.TurnID, SourceCallID: snap.Owner.CallID, Kind: "background_job_completion", Content: string(content), CreatedAt: event.At}
	payload, err := json.Marshal(event)
	if err != nil {
		s.reportCompletionError(err)
		return true
	}
	events := []session.EventRecord{{SessionID: snap.ID, TurnID: snap.Owner.TurnID, Kind: string(event.Kind), At: event.At, Payload: payload},
		{SessionID: snap.Owner.SessionID, TurnID: snap.Owner.TurnID, Kind: string(event.Kind), At: event.At, Payload: payload}}
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	for attempt := 0; attempt < 3; attempt++ {
		var seqs []int64
		var created bool
		seqs, created, err = store.RecordRuntimeInput(ctx, input, events)
		if err == nil {
			if created {
				if len(seqs) != len(events) {
					s.reportCompletionError(fmt.Errorf("invalid runtime input event receipt"))
					return true
				}
				for i, partition := range []string{snap.ID, snap.Owner.SessionID} {
					copyEvent := event
					copyEvent.Seq = seqs[i]
					s.fanLive(partition, copyEvent)
				}
			}
			if s.OnCompletion != nil {
				s.OnCompletion(snap.Owner.SessionID)
			}
			return true
		}
		if attempt < 2 {
			select {
			case <-ctx.Done():
				s.reportCompletionError(err)
				return true
			case <-time.After(time.Duration(attempt+1) * 50 * time.Millisecond):
			}
		}
	}
	s.reportCompletionError(fmt.Errorf("persist background completion %s: %w", snap.ID, err))
	return true
}

func (s *JobEventSink) reportCompletionError(err error) {
	if s.OnCompletionError != nil {
		s.OnCompletionError(err)
		return
	}
	fmt.Fprintf(os.Stderr, "[background-completion] %v\n", err)
}
