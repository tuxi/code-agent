package main

import (
	"code-agent/internal/agent"
	"fmt"
	"io"
	"sync"
	"time"
)

// liveProgress decorates an Emitter with a live "Thinking… Ns" ticker between
// EventModelStarted and EventModelFinished, so a long model call reads as
// progress instead of a hang. It forwards every event to next unchanged — it is
// a pure renderer add-on fed by the same event stream, with zero changes to the
// loop. This is the proof that P3.8 is a renderer concern, not a runtime one.
type liveProgress struct {
	next agent.Emitter
	w    io.Writer

	mu     sync.Mutex
	stop   chan struct{}
	active bool
}

func newLiveProgress(next agent.Emitter, w io.Writer) *liveProgress {
	return &liveProgress{next: next, w: w}
}

func (p *liveProgress) Emit(e agent.Event) {
	switch e.Kind {
	case agent.EventModelStarted:
		p.start()
	case agent.EventModelFinished:
		p.stopAndClear()
	}
	p.next.Emit(e)
}

// start spins up a ticker goroutine that rewrites the "Thinking… Ns" line in
// place once a second. Idempotent: a second start while active is a no-op.
func (p *liveProgress) start() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active {
		return
	}
	p.active = true
	p.stop = make(chan struct{})
	begin := time.Now()
	stop := p.stop
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				p.mu.Lock()
				if p.active { // re-check under lock: stopAndClear may have just run
					fmt.Fprintf(p.w, "\rThinking... %ds ", int(time.Since(begin).Seconds()))
				}
				p.mu.Unlock()
			}
		}
	}()
}

// stopAndClear stops the ticker and erases the progress line so the next event
// renders on a clean line. Idempotent.
func (p *liveProgress) stopAndClear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.active {
		return
	}
	close(p.stop)
	p.active = false
	fmt.Fprint(p.w, "\r\033[K") // carriage return + erase to end of line
}
