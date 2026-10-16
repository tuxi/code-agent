package session

import (
	"context"
	"errors"
)

var ErrEventPaginationUnsupported = errors.New("session: history pagination is not supported by this event store")

// EventPageStore is an optional event-store extension for reverse history
// pagination. beforeSeq is exclusive; zero selects the latest events. Returned
// records are in ascending seq order. maxRows is a soft target: a page extends
// backward to turn_started unless maxBytes or the hard row cap is reached.
// A truncated page may begin inside a large turn; callers must backfill and
// merge all fragments before reducing that turn into a history graph.
type EventPageStore interface {
	SessionEventPage(ctx context.Context, sessionID string, beforeSeq int64, maxRows int, maxBytes int64) ([]EventRecord, bool, bool, error)
}

const eventPageHardRows = 10000

// BuildEventPage consumes a newest-first iterator. It is shared by the SQLite,
// memory, and adapter paths so their turn boundaries match exactly. The caller
// calculates hasMoreOlder against the oldest returned seq.
func BuildEventPage(maxRows int, maxBytes int64, next func() (EventRecord, bool, error)) ([]EventRecord, bool, error) {
	var newest []EventRecord
	var bytes int64
	truncated := false
	for {
		rec, ok, err := next()
		if err != nil {
			return nil, false, err
		}
		if !ok {
			break
		}
		if len(newest) > 0 && (len(newest) >= eventPageHardRows || bytes+int64(len(rec.Payload)) > maxBytes) {
			truncated = newest[len(newest)-1].Kind != "turn_started"
			break
		}
		newest = append(newest, rec)
		bytes += int64(len(rec.Payload))
		if len(newest) >= maxRows && rec.Kind == "turn_started" {
			break
		}
	}
	for i, j := 0, len(newest)-1; i < j; i, j = i+1, j-1 {
		newest[i], newest[j] = newest[j], newest[i]
	}
	return newest, truncated, nil
}
