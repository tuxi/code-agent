package mcp

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestManagerNoServersIsNoOp(t *testing.T) {
	m := NewManager(io.Discard)
	if err := m.Connect(context.Background(), nil); err != nil {
		t.Fatalf("Connect with no servers: %v", err)
	}
	if len(m.Tools()) != 0 {
		t.Fatalf("expected no tools, got %d", len(m.Tools()))
	}
	if m.Summary() != "" {
		t.Fatalf("expected empty summary, got %q", m.Summary())
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close on empty manager: %v", err)
	}
}

func TestManagerSkipsFailedServer(t *testing.T) {
	m := NewManager(io.Discard)
	// A command that cannot start must be recorded as failed and skipped, not
	// abort the whole Connect — one broken server can't take the agent down.
	err := m.Connect(context.Background(), []ServerConfig{
		{Name: "broken", Command: "codeagent-no-such-binary-xyz"},
	})
	if err != nil {
		t.Fatalf("Connect should not return an error for a single bad server: %v", err)
	}
	if len(m.Tools()) != 0 {
		t.Fatalf("a failed server should contribute no tools, got %d", len(m.Tools()))
	}
	report := m.Report()
	if len(report) != 1 || report[0].Connected || report[0].Err == nil {
		t.Fatalf("expected one failed server in the report, got %+v", report)
	}
	if s := m.Summary(); !strings.Contains(s, "broken") || !strings.Contains(s, "FAILED") {
		t.Fatalf("summary should flag the failed server loudly, got %q", s)
	}
}
