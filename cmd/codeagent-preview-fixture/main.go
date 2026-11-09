// Command codeagent-preview-fixture serves the frozen preview v1 fixtures over
// authenticated loopback HTTP for AgentKit integration tests.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"code-agent/internal/preview/fixtureserver"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	defaultFixtures := filepath.Join("docs", "capability-runtime-upgrade", "fixtures", "preview", "v1")
	fixtures := flag.String("fixtures", defaultFixtures, "frozen preview v1 fixture directory")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flag.Args())
	}

	srv, err := fixtureserver.Start(*fixtures)
	if err != nil {
		return err
	}
	defer func() { _ = srv.Close() }()
	ready, err := json.Marshal(srv.Ready())
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(os.Stdout, string(ready)); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case <-srv.Done():
		return srv.Err()
	}
}
