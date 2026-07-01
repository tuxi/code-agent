package projectgraph

import (
	"context"
	"testing"
	"time"
)

// These tests require sourcekitten to be installed. They create real Swift
// projects on disk and exercise the adapter end-to-end.
//
// Run with:
//
//	go test -run "TestSwiftIntegration" -v ./internal/tools/project_graph/...

func TestSwiftIntegrationFindSymbol(t *testing.T) {
	adapter := NewSwiftAdapter()
	if !adapter.Available() {
		t.Skip("sourcekitten not installed — skipping integration test")
	}
	root := "/tmp/swift-test-project/Sources"

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// --- Find a struct ---
	syms, err := adapter.FindSymbol(ctx, root, "VideoFrameProvider")
	if err != nil {
		t.Fatalf("FindSymbol(VideoFrameProvider): %v", err)
	}
	if len(syms) == 0 {
		t.Fatal("expected at least 1 VideoFrameProvider symbol, got 0")
	}
	for _, s := range syms {
		t.Logf("  Symbol: name=%q kind=%q file=%q line=%d", s.Name, s.Kind, s.File, s.Line)
	}
	structSym := syms[0]
	if structSym.Kind != "struct" {
		t.Errorf("VideoFrameProvider kind = %q, want struct", structSym.Kind)
	}
	if structSym.Line == 0 {
		t.Error("line should be non-zero")
	}

	// --- Find a class ---
	syms2, err := adapter.FindSymbol(ctx, root, "FrameManager")
	if err != nil {
		t.Fatalf("FindSymbol(FrameManager): %v", err)
	}
	if len(syms2) == 0 {
		t.Fatal("expected at least 1 FrameManager symbol")
	}
	for _, s := range syms2 {
		t.Logf("  Symbol: name=%q kind=%q file=%q line=%d", s.Name, s.Kind, s.File, s.Line)
	}
	if syms2[0].Kind != "class" {
		t.Errorf("FrameManager kind = %q, want class", syms2[0].Kind)
	}

	// --- Find a method ---
	syms3, err := adapter.FindSymbol(ctx, root, "renderFrame()")
	if err != nil {
		t.Fatalf("FindSymbol(renderFrame()): %v", err)
	}
	if len(syms3) == 0 {
		t.Fatal("expected at least 1 renderFrame symbol")
	}
	for _, s := range syms3 {
		t.Logf("  Symbol: name=%q kind=%q file=%q line=%d", s.Name, s.Kind, s.File, s.Line)
	}
	if syms3[0].Kind != "method" {
		t.Errorf("renderFrame kind = %q, want method", syms3[0].Kind)
	}
}

func TestSwiftIntegrationFindReferences(t *testing.T) {
	adapter := NewSwiftAdapter()
	if !adapter.Available() {
		t.Skip("sourcekitten not installed — skipping integration test")
	}
	root := "/tmp/swift-test-project/Sources"

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// --- Find references to VideoFrameProvider (definition only) ---
	// Note: sourcekitten index operates per-file with minimal compiler args
	// (-sdk only). Cross-file references are not resolved without a full
	// build system (SPM/Xcode) producing an index store. Within-file
	// references work correctly — see the renderFrame test below.
	refs, err := adapter.FindReferences(ctx, root, "VideoFrameProvider")
	if err != nil {
		t.Fatalf("FindReferences(VideoFrameProvider): %v", err)
	}
	t.Logf("VideoFrameProvider references: %d", len(refs))
	for _, r := range refs {
		t.Logf("  Ref: file=%q line=%d context=%q", r.File, r.Line, r.Context)
	}
	if len(refs) == 0 {
		t.Error("expected at least the definition site of VideoFrameProvider")
	}
	hasDef := false
	for _, r := range refs {
		if r.Line == 4 && hasSuffix(r.File, "VideoFrameProvider.swift") {
			hasDef = true
		}
	}
	if !hasDef {
		t.Error("expected the definition site of VideoFrameProvider (VideoFrameProvider.swift:4)")
	}

	// --- Find references to Frame (used within VideoFrameProvider.swift) ---
	// Frame is defined and used within the same file, so within-file
	// references should be resolved.
	refs2, err := adapter.FindReferences(ctx, root, "Frame")
	if err != nil {
		t.Fatalf("FindReferences(Frame): %v", err)
	}
	t.Logf("Frame references: %d", len(refs2))
	for _, r := range refs2 {
		t.Logf("  Ref: file=%q line=%d context=%q", r.File, r.Line, r.Context)
	}
	// Frame is defined on line 18 and referenced in renderFrame() on lines 8 and 10.
	if len(refs2) < 2 {
		t.Errorf("expected at least 2 references to Frame (def + within-file usage), got %d", len(refs2))
	}
	hasDef = false
	hasRef := false
	for _, r := range refs2 {
		if r.Line == 18 && hasSuffix(r.File, "VideoFrameProvider.swift") {
			hasDef = true
		}
		if (r.Line == 8 || r.Line == 10) && hasSuffix(r.File, "VideoFrameProvider.swift") {
			hasRef = true
		}
	}
	if !hasDef {
		t.Error("expected Frame definition at VideoFrameProvider.swift:18")
	}
	if !hasRef {
		t.Error("expected Frame reference at VideoFrameProvider.swift:8 or :10 (in renderFrame body)")
	}
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
