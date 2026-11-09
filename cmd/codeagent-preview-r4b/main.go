// Command codeagent-preview-r4b starts a persistent authenticated Runtime session
// for the R4b normal-turn preview harness. The first stdout line is a readiness
// JSON record; the process remains alive until SIGINT or SIGTERM.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var opts ServerOptions
	flag.StringVar(&opts.Workspace, "workspace", "", "workspace root; empty creates a temporary workspace")
	flag.StringVar(&opts.StateDir, "state-dir", "", "CodeAgent state directory; empty creates a temporary directory")
	flag.StringVar(&opts.SessionID, "session-id", "", "pre-created session ID; empty generates one")
	flag.StringVar(&opts.TurnPrompt, "turn-prompt", DefaultTurnPrompt, "deterministic prompt that invokes ui_preview")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flag.Args())
	}
	srv, readiness, err := Start(context.Background(), opts)
	if err != nil {
		return err
	}
	defer srv.Close()
	if err := json.NewEncoder(os.Stdout).Encode(readiness); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-srv.Done():
		return err
	}
}
