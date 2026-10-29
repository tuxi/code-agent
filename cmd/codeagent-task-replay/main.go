// codeagent-task-replay is a local GUI test reader, not a coding Runtime.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"code-agent/internal/taskreplay"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	fixture := flag.String("fixture", "", "offline managed export JSON (required)")
	state := flag.String("state-parent", os.TempDir(), "parent for a new private connection directory")
	lifetime := flag.Duration("lifetime", time.Hour, "maximum service lifetime")
	flag.Parse()
	if *fixture == "" || *lifetime <= 0 || *lifetime > 12*time.Hour {
		return fmt.Errorf("fixture and lifetime (0,12h] required")
	}
	f, err := os.Open(*fixture)
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(f, taskreplay.MaxFixtureBytes+1))
	f.Close()
	if err != nil {
		return err
	}
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		return err
	}
	token := hex.EncodeToString(key)
	h, err := taskreplay.New(raw, token)
	if err != nil {
		return err
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer l.Close()
	dir, err := os.MkdirTemp(*state, "codeagent-offline-replay-")
	if err != nil {
		return err
	}
	connection := filepath.Join(dir, "connection.json")
	defer os.Remove(connection)
	expires := time.Now().Add(*lifetime)
	data, _ := json.MarshalIndent(map[string]any{"display_name": "P0.5 Offline Replay (read-only)", "endpoint": "http://" + l.Addr().String(), "access_token": token, "pid": os.Getpid(), "expires_at": expires.UTC(), "fixture_only": true}, "", "  ")
	if err = os.WriteFile(connection, data, 0600); err != nil {
		return err
	}
	fmt.Printf("Offline Replay (GET only)\nEndpoint: http://%s\nPrivate connection file: %s\nExpires: %s (connection file removed on exit)\n", l.Addr(), connection, expires.Format(time.RFC3339))
	server := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 15 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *lifetime)
	defer cancel()
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			c, end := context.WithTimeout(context.Background(), 3*time.Second)
			defer end()
			_ = server.Shutdown(c)
		case <-done:
		}
	}()
	err = server.Serve(l)
	close(done)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
