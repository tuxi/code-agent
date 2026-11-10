// Command codeagent-preview-r4a runs a deterministic but real Backend agent
// turn through the production agent loop, typed ui_preview tool, durable SQLite
// event store, preview materializer, and in-process trusted viewer handoff.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
)

func main() {
	workspace := flag.String("workspace", "", "workspace root; empty creates a temporary workspace")
	stateDir := flag.String("state-dir", "", "CodeAgent state directory; empty creates a temporary directory")
	viewerDir := flag.String("viewer-dir", "", "trusted viewer output directory; empty creates a temporary directory")
	viewerHTMLFile := flag.String("viewer-html-file", "", "viewer HTML fragment file; empty uses the acceptance default")
	fallbackFile := flag.String("fallback-file", "", "fallback PNG/JPEG/WebP file; empty uses the generated default")
	title := flag.String("title", "", "preview title; empty uses the acceptance default")
	summaryText := flag.String("summary", "", "preview summary; empty uses the acceptance default")
	artifactKey := flag.String("artifact-key", "", "preview artifact key; empty uses the acceptance default")
	preferredAspectRatio := flag.Float64("preferred-aspect-ratio", 1.5, "preferred viewer aspect ratio")
	minHeight := flag.Int("min-height", 360, "preferred minimum viewer height")
	turnPrompt := flag.String("turn-prompt", "", "dedicated acceptance turn prompt; empty uses the R4a default")
	finalAnswer := flag.String("final-answer", "", "dedicated completion text; empty uses the R4a default")
	openViewer := flag.Bool("open", true, "open the verified trusted viewer in the system browser")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected arguments:", flag.Args())
		os.Exit(2)
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" && *openViewer {
		fmt.Fprintln(os.Stderr, "warning: -open is supported only on darwin/linux; continuing with -open=false")
		*openViewer = false
	}
	summary, err := runAcceptance(context.Background(), AcceptanceOptions{
		Workspace: *workspace, StateDir: *stateDir, ViewerDir: *viewerDir,
		ViewerHTMLFile: *viewerHTMLFile, FallbackFile: *fallbackFile,
		Title: *title, Summary: *summaryText, ArtifactKey: *artifactKey,
		PreferredAspectRatio: *preferredAspectRatio, MinHeight: *minHeight,
		TurnPrompt: *turnPrompt, FinalAnswer: *finalAnswer,
		Open: *openViewer, Output: os.Stdout,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if err := writeSummary(os.Stdout, summary); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
