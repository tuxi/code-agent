package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestLoadOrCreateServerStateConcurrentUsesOneIdentity(t *testing.T) {
	base := t.TempDir()
	oldBase := StoreBaseDir()
	SetStoreBaseDir(base)
	t.Cleanup(func() { SetStoreBaseDir(oldBase) })

	const callers = 32
	results := make(chan ServerState, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			state, err := LoadOrCreateServerState(base, []byte(`{"models":[]}`))
			if err != nil {
				errs <- err
				return
			}
			results <- state
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatal(err)
	}
	var first ServerState
	for state := range results {
		if first.ServerID == "" {
			first = state
			continue
		}
		if state.ServerID != first.ServerID {
			t.Fatalf("concurrent initialization created multiple server IDs: %q and %q", first.ServerID, state.ServerID)
		}
	}
	if first.ServerID == "" {
		t.Fatal("no server state returned")
	}

	data, err := os.ReadFile(filepath.Join(base, serverStateFilename))
	if err != nil {
		t.Fatal(err)
	}
	var persisted ServerState
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("persisted state is invalid JSON: %v", err)
	}
	if persisted.ServerID != first.ServerID {
		t.Fatalf("persisted server ID = %q, want %q", persisted.ServerID, first.ServerID)
	}
}
