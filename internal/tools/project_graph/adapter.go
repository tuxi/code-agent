package projectgraph

import (
	"context"
	"fmt"
	"os/exec"
)

// LanguageAdapter is the contract every language backend implements. The tool
// talks only to this interface; it never knows which CLI sits behind it.
//
// Implementations must be cheap to construct (no process is spawned until a
// method runs) and must report Available() honestly so the tool can skip a
// backend whose toolchain is not installed instead of failing the whole call.
type LanguageAdapter interface {
	// Language is the canonical name the tool routes on: "go", "swift",
	// "rust", "python".
	Language() string
	// Available reports whether the underlying toolchain (gopls, sourcekitten,
	// ...) is installed and usable.
	Available() bool
	// FindSymbol returns the symbols matching query in the workspace at root.
	FindSymbol(ctx context.Context, root, query string) ([]Symbol, error)
	// FindReferences returns the use-sites of the named symbol in the workspace
	// at root.
	FindReferences(ctx context.Context, root, symbol string) ([]Reference, error)
}

// pendingAdapter is an adapter whose semantic backend is not yet wired up. It
// still participates fully in the unified interface — it reports whether its
// toolchain is installed and names the command it will delegate to — but its
// query methods return a clear, actionable "not yet implemented" message rather
// than pretending to work or silently returning nothing.
//
// This keeps the architecture honest: Swift/Rust/Python are first-class members
// of the registry with real availability detection, and filling them in later
// is a localized change behind this same interface.
type pendingAdapter struct {
	language string // routing name
	binary   string // CLI it will delegate to, e.g. "sourcekitten"
	plan     string // the command it intends to run, surfaced in messages
}

func (a pendingAdapter) Language() string { return a.language }

func (a pendingAdapter) Available() bool {
	_, err := exec.LookPath(a.binary)
	return err == nil
}

func (a pendingAdapter) FindSymbol(context.Context, string, string) ([]Symbol, error) {
	return nil, a.notImplemented()
}

func (a pendingAdapter) FindReferences(context.Context, string, string) ([]Reference, error) {
	return nil, a.notImplemented()
}

func (a pendingAdapter) notImplemented() error {
	return fmt.Errorf("%s adapter is not implemented yet (planned backend: %q via %q)", a.language, a.plan, a.binary)
}

// NewSwiftAdapter returns the Swift backend. It is a stub pending a sourcekitten
// integration (the PRD permits a stub here); Available() reflects whether
// sourcekitten is installed.
func NewSwiftAdapter() LanguageAdapter {
	return pendingAdapter{language: "swift", binary: "sourcekitten", plan: "sourcekitten structure"}
}

// NewRustAdapter returns the Rust backend, pending a rust-analyzer integration.
func NewRustAdapter() LanguageAdapter {
	return pendingAdapter{language: "rust", binary: "rust-analyzer", plan: "rust-analyzer (LSP symbol/reference)"}
}

// NewPythonAdapter returns the Python backend, pending a pyright integration.
func NewPythonAdapter() LanguageAdapter {
	return pendingAdapter{language: "python", binary: "pyright", plan: "pyright (LSP symbol/reference)"}
}
