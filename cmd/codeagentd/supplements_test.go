package main

import (
	"code-agent/internal/conversation"
	sqlitestore "code-agent/internal/session/sqlite"
	"path/filepath"
	"testing"
)

func TestExperimentalTextSupplementsAreExplicitAndRequireSQLite(t *testing.T) {
	executor := &conversation.TurnExecutor{}
	if err := configureExperimentalSupplements(executor, nil, false); err != nil {
		t.Fatal(err)
	}
	if executor.SupportsTextSupplements() {
		t.Fatal("enabled by default")
	}
	if err := configureExperimentalSupplements(executor, nil, true); err == nil {
		t.Fatal("missing durable store accepted")
	}
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = configureExperimentalSupplements(executor, store, true); err != nil {
		t.Fatal(err)
	}
	if !executor.SupportsTextSupplements() {
		t.Fatal("real startup wiring missing")
	}
}
