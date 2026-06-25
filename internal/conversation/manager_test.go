package conversation

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"code-agent/internal/agent"
	"code-agent/internal/session"
)

// fakeFactory builds throwaway conversations and counts its calls.
type fakeFactory struct {
	mu          sync.Mutex
	createCount int
	resumeCount int
	resumeErr   error
}

func (f *fakeFactory) Create(context.Context) (*Conversation, error) {
	f.mu.Lock()
	f.createCount++
	id := fmt.Sprintf("new_%d", f.createCount)
	f.mu.Unlock()
	return New(&agent.Runner{}, &session.Session{ID: id}, nil), nil
}

func (f *fakeFactory) Resume(_ context.Context, id string) (*Conversation, error) {
	f.mu.Lock()
	f.resumeCount++
	err := f.resumeErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return New(&agent.Runner{}, &session.Session{ID: id}, nil), nil
}

func (f *fakeFactory) resumes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resumeCount
}

func TestManagerCreateRegisters(t *testing.T) {
	m := NewManager(&fakeFactory{})
	c, err := m.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, ok := m.Get(c.ID())
	if !ok || got != c {
		t.Errorf("Create did not register the conversation")
	}
}

func TestManagerResumeCaches(t *testing.T) {
	f := &fakeFactory{}
	m := NewManager(f)

	c1, err := m.Resume(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	c2, err := m.Resume(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if c1 != c2 {
		t.Error("second Resume returned a different conversation")
	}
	if f.resumes() != 1 {
		t.Errorf("factory.Resume called %d times, want 1 (second served from cache)", f.resumes())
	}
}

func TestManagerResumeError(t *testing.T) {
	m := NewManager(&fakeFactory{resumeErr: errors.New("not found")})
	if _, err := m.Resume(context.Background(), "x"); err == nil {
		t.Error("want error from Resume when the factory fails")
	}
	if len(m.List()) != 0 {
		t.Error("a failed Resume must not register anything")
	}
}

func TestManagerRemoveClosesSubscribers(t *testing.T) {
	m := NewManager(&fakeFactory{})
	c, _ := m.Create(context.Background())
	ch, _ := c.Subscribe()

	m.Remove(c.ID())

	if _, ok := m.Get(c.ID()); ok {
		t.Error("Remove did not drop the conversation")
	}
	if _, open := <-ch; open {
		t.Error("Remove must close live subscriber channels")
	}
}

func TestManagerShutdownClosesAll(t *testing.T) {
	m := NewManager(&fakeFactory{})
	a, _ := m.Create(context.Background())
	b, _ := m.Create(context.Background())
	chA, _ := a.Subscribe()
	chB, _ := b.Subscribe()

	m.Shutdown()

	if len(m.List()) != 0 {
		t.Errorf("registry not empty after Shutdown: %d", len(m.List()))
	}
	for i, ch := range []<-chan agent.Event{chA, chB} {
		if _, open := <-ch; open {
			t.Errorf("subscriber %d channel not closed by Shutdown", i)
		}
	}
}

func TestManagerResumeConcurrentCollapses(t *testing.T) {
	f := &fakeFactory{}
	m := NewManager(f)

	const n = 20
	var wg sync.WaitGroup
	results := make([]*Conversation, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := m.Resume(context.Background(), "same")
			if err != nil {
				t.Error(err)
				return
			}
			results[i] = c
		}(i)
	}
	wg.Wait()

	for i := 1; i < n; i++ {
		if results[i] != results[0] {
			t.Fatalf("goroutine %d got a different conversation: concurrent Resume did not collapse", i)
		}
	}
	if len(m.List()) != 1 {
		t.Errorf("registry has %d entries, want exactly 1", len(m.List()))
	}
}
