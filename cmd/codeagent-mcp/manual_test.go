package main

import (
	"context"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestManualCLIProcess(t *testing.T) {
	if os.Getenv("CODEAGENT_MANUAL_CLI_HELPER") != "1" {
		t.Skip("stdio helper")
	}
	flag.CommandLine = flag.NewFlagSet("codeagent-mcp", flag.ContinueOnError)
	os.Args = []string{"codeagent-mcp", "--experimental-p0a", "--profile", "manual", "--workspace", os.Getenv("MANUAL_CLI_WORKSPACE"), "--connection-manifest", os.Getenv("MANUAL_CLI_MANIFEST")}
	if err := run(); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestManualCLIInitializesOverStdioWithoutRuntime(t *testing.T) {
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(t.TempDir(), "repo")
	if out, err := exec.Command("git", "clone", "--quiet", "--shared", strings.TrimSpace(string(root)), workspace).CombinedOutput(); err != nil {
		t.Fatalf("clone: %s %v", out, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.Command(os.Args[0], "-test.run=^TestManualCLIProcess$")
	cmd.Env = append(os.Environ(), "CODEAGENT_MANUAL_CLI_HELPER=1", "MANUAL_CLI_WORKSPACE="+workspace, "MANUAL_CLI_MANIFEST="+filepath.Join(t.TempDir(), "missing.json"))
	client, err := mcp.NewClient(&mcp.Implementation{Name: "manual-stdio-client"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	list, err := client.ListTools(ctx, nil)
	if err != nil || len(list.Tools) != 11 {
		t.Fatalf("native stdio catalog: %+v %v", list, err)
	}
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "codeagent_connection_info", Arguments: map[string]any{}})
	if err != nil || result == nil || !result.IsError {
		t.Fatal(result, err)
	}
	if _, err := client.ListTools(ctx, nil); err != nil {
		t.Fatal("diagnostic failure closed MCP", err)
	}
}

func TestManualConfigDefaultsAndLegacyProfileCompatibility(t *testing.T) {
	root := t.TempDir()
	config, err := projectConfig("/local/bridge", "/private/missing.json", root, "", "", "")
	if err != nil || !strings.Contains(config, `"--profile","manual"`) || strings.Contains(config, "--baseline-commit") {
		t.Fatal(config, err)
	}
	legacy, err := projectConfig("/local/bridge", "/private/missing.json", root, "", "s1", strings.Repeat("a", 40))
	if err != nil || !strings.Contains(legacy, `"--profile","executor"`) {
		t.Fatal(legacy, err)
	}
	if _, err := projectConfigForProfile("/local/bridge", "/private/missing.json", root, "", "s1", strings.Repeat("a", 40), "manual"); err == nil {
		t.Fatal("manual ignored fixed Session flags")
	}
	for _, profile := range []string{"executor", "managed", "reviewer"} {
		config, err := projectConfigForProfile("/local/bridge", "/private/missing.json", root, "", "s1", strings.Repeat("a", 40), profile)
		if err != nil || !strings.Contains(config, `"--profile","`+profile+`"`) {
			t.Fatal(config, err)
		}
	}
}
