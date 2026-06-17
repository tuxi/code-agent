// Package projectgraph is a language-service-driven semantic layer over the
// workspace. Where grep answers "where does this text appear", project_graph
// answers "what is this symbol, where is it defined, and what references it".
//
// It deliberately does NOT implement any language parsing of its own. Every
// answer is delegated to the language's own toolchain — gopls for Go,
// sourcekitten for Swift, rust-analyzer for Rust, pyright for Python — and the
// per-language results are normalized into one schema (Symbol / Reference). The
// package composes existing compilers and language servers into a unified
// interface; it is not, and does not try to be, an IDE-grade index.
package projectgraph

// Symbol is the unified, language-agnostic representation of a code symbol.
// Every adapter must map its toolchain's output into exactly this shape.
type Symbol struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // e.g. struct, function, method, interface, variable
	File string `json:"file"` // workspace-relative path, slash-separated
	Line int    `json:"line"` // 1-based
}

// Reference is a single use-site of a symbol.
type Reference struct {
	File    string `json:"file"`    // workspace-relative path, slash-separated
	Line    int    `json:"line"`    // 1-based
	Context string `json:"context"` // the source line at File:Line, trimmed
}

// RenameCheck is the result of a rename safety analysis: how many files a rename
// would touch, whether it looks safe, and any warnings the agent should heed
// before performing the rename with edit_file / apply_patch.
type RenameCheck struct {
	From          string   `json:"from"`
	To            string   `json:"to"`
	AffectedFiles int      `json:"affected_files"`
	References    int      `json:"references"`
	Safe          bool     `json:"safe"`
	Warnings      []string `json:"warnings"`
}
