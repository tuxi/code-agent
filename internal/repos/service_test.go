package repos

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
)

func newFakeCloneService(t *testing.T) (*Service, string, string) {
	t.Helper()
	root, state := t.TempDir(), t.TempDir()
	s, err := NewService(root, state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, root, state
}

func fakeCloneFile(_ context.Context, _ normalizedRequest, temp string) error {
	return os.WriteFile(filepath.Join(temp, "README.md"), []byte("ok\n"), 0o644)
}

func TestServiceIdempotentReplayAndUniqueDestination(t *testing.T) {
	s, root, _ := newFakeCloneService(t)
	var calls atomic.Int32
	s.clone = func(ctx context.Context, req normalizedRequest, temp string) error {
		calls.Add(1)
		return fakeCloneFile(ctx, req, temp)
	}
	req := Request{RequestID: "request-1", URL: "https://gitlab.com/team/repo.git"}
	first, err := s.Clone(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.Clone(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || first.AbsPath != replay.AbsPath || first.Rel != "repo" {
		t.Fatalf("calls=%d first=%+v replay=%+v", calls.Load(), first, replay)
	}
	second, err := s.Clone(context.Background(), Request{RequestID: "request-2", URL: req.URL})
	if err != nil {
		t.Fatal(err)
	}
	if second.Rel != "repo1" {
		t.Fatalf("second.Rel=%q, want repo1", second.Rel)
	}
	if _, err := os.Stat(filepath.Join(root, "repo", "README.md")); err != nil {
		t.Fatal(err)
	}
}

func TestServiceRequestIDPayloadConflict(t *testing.T) {
	s, _, _ := newFakeCloneService(t)
	s.clone = fakeCloneFile
	if _, err := s.Clone(context.Background(), Request{RequestID: "same", URL: "https://example.com/a.git"}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Clone(context.Background(), Request{RequestID: "same", URL: "https://example.com/b.git"})
	var ce *CloneError
	if !errors.As(err, &ce) || ce.Code != "destination_conflict" {
		t.Fatalf("err=%v, want destination_conflict", err)
	}
}

func TestServiceCoalescesConcurrentRequests(t *testing.T) {
	s, _, _ := newFakeCloneService(t)
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	s.clone = func(ctx context.Context, req normalizedRequest, temp string) error {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return fakeCloneFile(ctx, req, temp)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	req := Request{RequestID: "concurrent", URL: "https://example.com/repo.git"}
	var wg sync.WaitGroup
	results := make([]*CloneResult, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = s.Clone(context.Background(), req)
		}(i)
	}
	<-started
	close(release)
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || calls.Load() != 1 || results[0].AbsPath != results[1].AbsPath {
		t.Fatalf("calls=%d results=%+v errors=%v", calls.Load(), results, errs)
	}
}

func TestServicePersistsSuccessfulReplay(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	s, err := NewService(root, state)
	if err != nil {
		t.Fatal(err)
	}
	s.clone = fakeCloneFile
	req := Request{RequestID: "durable", URL: "https://example.com/repo.git"}
	first, err := s.Clone(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	// Simulate an embedded sandbox re-anchor: durable identity is the relative
	// project name, while the absolute Documents prefix changes across launches.
	newRoot := t.TempDir()
	canonicalRoot, err := filepath.EvalSymlinks(newRoot)
	if err != nil {
		t.Fatal(err)
	}
	s, err = NewService(newRoot, state)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.clone = func(context.Context, normalizedRequest, string) error {
		t.Fatal("durable replay executed clone again")
		return nil
	}
	replay, err := s.Clone(context.Background(), req)
	wantReplay := filepath.Join(canonicalRoot, first.Rel)
	if err != nil || replay.AbsPath != wantReplay {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
}

func TestServiceCancellationCleansTemporaryDirectory(t *testing.T) {
	s, _, _ := newFakeCloneService(t)
	s.clone = func(ctx context.Context, _ normalizedRequest, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	defer cancel()
	_, err := s.Clone(ctx, Request{RequestID: "cancel", URL: "https://example.com/repo.git"})
	var ce *CloneError
	if !errors.As(err, &ce) || ce.Code != "cancelled" {
		t.Fatalf("err=%v, want cancelled", err)
	}
	entries, err := os.ReadDir(s.tempRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temp entries=%v err=%v", entries, err)
	}
}

func TestPublicIPPolicy(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.100.100.200", "192.0.2.1", "::1", "fc00::1"} {
		if isPublicIP(netip.MustParseAddr(raw)) {
			t.Errorf("%s accepted as public", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !isPublicIP(netip.MustParseAddr(raw)) {
			t.Errorf("%s rejected as non-public", raw)
		}
	}
}

func TestServicePublicCloneIntegration(t *testing.T) {
	if os.Getenv("CODEAGENT_TEST_PUBLIC_GIT") != "1" {
		t.Skip("set CODEAGENT_TEST_PUBLIC_GIT=1 to run the public network integration test")
	}
	s, _, _ := newFakeCloneService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result, err := s.Clone(ctx, Request{
		RequestID: "public-integration",
		URL:       "https://github.com/octocat/Hello-World.git",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := gogit.PlainOpen(result.AbsPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := repo.Config()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Remotes["origin"].URLs[0]; got != "https://github.com/octocat/Hello-World.git" {
		t.Fatalf("origin=%q", got)
	}
}
