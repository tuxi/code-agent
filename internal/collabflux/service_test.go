package collabflux

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"code-agent/internal/collab"
	sessionsqlite "code-agent/internal/session/sqlite"
)

func TestServiceRequiresExclusiveHostAndMatchingFluxIdentity(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := sessionsqlite.New(filepath.Join(dir, "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	opts := ServiceOptions{Tasks: s.TaskRepository("server"), Actor: collab.Principal{ID: "runtime-owner", LocalSessions: true}, Dispatch: func(context.Context, collab.ManagedTurnRef) error { return nil }}
	path := filepath.Join(dir, "managed", "flux.db")
	first, err := OpenService(ctx, path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if other, err := OpenService(ctx, path, opts); !errors.Is(err, collab.ErrManagedWorkspace) {
		if other != nil {
			other.Close()
		}
		t.Fatal("another managed host was admitted", err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	otherStore, err := sessionsqlite.New(filepath.Join(dir, "other.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer otherStore.Close()
	wrong := opts
	wrong.Tasks = otherStore.TaskRepository("server")
	if other, err := OpenService(ctx, path, wrong); !errors.Is(err, collab.ErrConflict) {
		if other != nil {
			other.Close()
		}
		t.Fatal("private Flux DB accepted a different Store", err)
	}
	// An unsuccessful bind releases its host lock; the original identity can
	// still reopen and publish readiness.
	again, err := OpenService(ctx, path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if !again.Ready() {
		t.Fatal("matching store did not regain readiness")
	}
	bindings := map[int]collab.WorkflowBinding{}
	for _, n := range []int{1, 2, 5, 1, 2} {
		def, err := CollaborationDefinition(n)
		if err != nil {
			t.Fatal(err)
		}
		binding, err := again.templateBinding(ctx, def)
		if err != nil || binding.Hash != def.Hash() {
			t.Fatal(binding, err)
		}
		if previous, ok := bindings[n]; ok && previous != binding {
			t.Fatal("repeated template published a different binding", previous, binding)
		}
		bindings[n] = binding
	}
	var versions int64
	if err := again.runtime.DB().Table("workflow_versions").Count(&versions).Error; err != nil || versions != 3 {
		t.Fatal("template versions were duplicated", versions, err)
	}
}
