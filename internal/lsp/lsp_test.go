package lsp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestServerFraming drives the client's server-mode call/notify path against
// an in-process mock LSP server that speaks standard Content-Length framing
// (the same framing gopls and every other language server use). Before the
// framing fix, the client wrote newline-delimited JSON and the mock never
// saw a valid message — this test deadlocked/failed.
func TestServerFraming(t *testing.T) {
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	c := &Client{command: "mock", mode: modeServer, r: bufio.NewReader(clientR), w: clientW, done: make(chan struct{})}
	defer clientW.Close()
	defer serverW.Close()
	defer clientR.Close()
	defer serverR.Close()

	serverErr := make(chan error, 1)
	go func() {
		br := bufio.NewReader(serverR)
		// initialize request → capabilities response
		req, err := readTestFrame(br)
		if err != nil {
			serverErr <- err
			return
		}
		if !strings.Contains(req, `"method":"initialize"`) {
			serverErr <- fmt.Errorf("expected initialize, got %s", req)
			return
		}
		if err := writeTestFrame(serverW, `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"hoverProvider":true}}}`); err != nil {
			serverErr <- err
			return
		}
		// initialized notification
		if _, err := readTestFrame(br); err != nil {
			serverErr <- err
			return
		}
		// next call → send a server notification first, then the response,
		// exercising the client's skip-notifications-without-matching-id path.
		req, err = readTestFrame(br)
		if err != nil {
			serverErr <- err
			return
		}
		if !strings.Contains(req, `"method":"test/method"`) {
			serverErr <- fmt.Errorf("expected test/method, got %s", req)
			return
		}
		if err := writeTestFrame(serverW, `{"jsonrpc":"2.0","method":"window/logMessage","params":{"type":3,"message":"noise"}}`); err != nil {
			serverErr <- err
			return
		}
		if err := writeTestFrame(serverW, `{"jsonrpc":"2.0","id":2,"result":{"ok":true}}`); err != nil {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	ctx := context.Background()
	// Simulate the Initialize handshake, then a query call.
	if _, err := c.call(ctx, "initialize", map[string]any{"rootUri": "file:///" + c.root}); err != nil {
		t.Fatalf("initialize call: %v", err)
	}
	if err := c.notify(ctx, "initialized", map[string]any{}); err != nil {
		t.Fatalf("initialized notify: %v", err)
	}
	res, err := c.call(ctx, "test/method", map[string]any{"x": 1})
	if err != nil {
		t.Fatalf("test/method call: %v", err)
	}
	if string(res) != `{"ok":true}` {
		t.Fatalf("unexpected result: %s", res)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("mock server: %v", err)
	}
}

// TestGoplsServerModeIntegration is an end-to-end check of the fixed server
// mode against a real gopls process: language detection → initialize handshake
// → workspace symbol → hover at the definition site. Skipped when gopls is not
// installed. Index warmup is handled by retrying until results appear.
func TestGoplsServerModeIntegration(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not installed")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	client, err := DetectLanguage(root)
	if err != nil {
		t.Fatalf("DetectLanguage: %v", err)
	}
	if client.mode != modeServer {
		t.Fatalf("expected server mode for Go, got mode %d", client.mode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := client.Initialize(ctx, root); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	defer client.Close()

	// Workspace indexing is async — poll until WirePlanTools is found.
	var syms []Symbol
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		syms, err = client.FindSymbol(ctx, "WirePlanTools")
		if err != nil {
			t.Fatalf("FindSymbol: %v", err)
		}
		if len(syms) > 0 {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if len(syms) == 0 {
		t.Fatal("WirePlanTools not indexed within 60s")
	}
	def := syms[0]

	// Hover at the definition site must return the signature, not a fallback.
	var hr *HoverResult
	deadline = time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		hr, err = client.Hover(ctx, def.File, def.Line, def.Col)
		if err != nil {
			t.Fatalf("Hover: %v", err)
		}
		if hr != nil && strings.Contains(hr.Content, "func WirePlanTools") {
			break
		}
		hr = nil
		time.Sleep(2 * time.Second)
	}
	if hr == nil {
		t.Fatalf("hover never returned the signature for %s:%d:%d (last err %v)", def.File, def.Line, def.Col, err)
	}
	if !strings.Contains(hr.Content, "func WirePlanTools") {
		t.Fatalf("hover content missing signature: %q", hr.Content)
	}
}

// TestDiagnosticsWholeWorkspace is a regression test for the whole-workspace
// diagnostics path. Diagnostics(ctx, "") used to run `gopls check` with no file
// arguments, which only type-checks the root directory's package (empty at the
// repo root) — so a broken file anywhere in the workspace was never reported.
// The fix enumerates all workspace .go files and passes them to gopls check.
// Skipped when gopls is not installed.
func TestDiagnosticsWholeWorkspace(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not installed")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module scratch\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	broken := `package scratch

func Broken() int {
	x := 1
	return
}
`
	// Place the broken file in a SUBDIRECTORY: bare `gopls check` (no args)
	// only type-checks the current directory's package, so pre-fix code
	// (which passed no files) would never see it. Only file enumeration —
	// the whole point of the fix — reaches subdirectory files.
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "broken.go"), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &Client{command: "gopls", mode: modeCLI, root: root, ready: true}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Per-file check must find the errors too — guards the parser.
	diags, err := c.Diagnostics(ctx, "sub/broken.go")
	if err != nil {
		t.Fatalf("Diagnostics(file): %v", err)
	}
	if len(diags) == 0 {
		t.Fatal("per-file Diagnostics found nothing in a broken file")
	}

	// Whole-workspace check must find the same errors via file enumeration.
	diags, err = c.Diagnostics(ctx, "")
	if err != nil {
		t.Fatalf("Diagnostics(all): %v", err)
	}
	found := false
	for _, d := range diags {
		if strings.Contains(d.Message, "not enough return values") {
			found = true
		}
	}
	if !found {
		t.Fatalf("whole-workspace Diagnostics missed the broken file; got %d diags: %+v", len(diags), diags)
	}
}

// readTestFrame reads one Content-Length-framed message. Implemented
// independently of the client so the test cannot pass tautologically.
func readTestFrame(r *bufio.Reader) (string, error) {
	length := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return "", err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if rest, ok := strings.CutPrefix(line, "Content-Length:"); ok {
			length, _ = strconv.Atoi(strings.TrimSpace(rest))
		}
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return "", err
	}
	return string(body), nil
}

func writeTestFrame(w io.Writer, body string) error {
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err := io.WriteString(w, body)
	return err
}
