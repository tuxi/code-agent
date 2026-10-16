package controlplane

import (
	"context"
	"fmt"
	"testing"

	"code-agent/internal/session"
)

type metadataOwnerRepo struct {
	*fakeRepo
	calls int
}

func (r *metadataOwnerRepo) List(context.Context) ([]session.Meta, error) {
	return nil, fmt.Errorf("full List must not run")
}
func (r *metadataOwnerRepo) ListMetadata(ctx context.Context) ([]session.Meta, error) {
	r.calls++
	return r.fakeRepo.List(ctx)
}

func TestHeartbeatUsesMetadataAndSocketClaimDoesNotRelist(t *testing.T) {
	repo := &metadataOwnerRepo{fakeRepo: testRepo()}
	m, err := NewManager(t.TempDir(), "server", repo, nil, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx := context.Background()
	if err := m.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.ClaimKnownSession(ctx, "session-1"); err != nil {
		t.Fatal(err)
	}
	if repo.calls != 1 {
		t.Fatalf("metadata queries=%d, want 1", repo.calls)
	}
	owned, err := m.owns(ctx, "session-1")
	if err != nil || !owned {
		t.Fatalf("owned=%v err=%v", owned, err)
	}
}
