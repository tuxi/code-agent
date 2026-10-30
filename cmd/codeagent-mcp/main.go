// codeagent-mcp is an opt-in, loopback-only P0-A stdio bridge to an existing
// CodeAgent daemon. It must never write logs to stdout (MCP owns that stream).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"code-agent/internal/mcpbridge"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "codeagent-mcp:", err)
		os.Exit(1)
	}
}
func run() error {
	endpoint := flag.String("runtime-url", "", "Explicit http(s)://loopback:port endpoint")
	manifest := flag.String("connection-manifest", "", "Private Chater connection grant (exclusive with runtime-url/token-env)")
	attach := flag.String("attach-empty-session", "", "Explicitly bind this existing empty session; requires verified manifest/store, no execution recovery")
	reattach := flag.String("reattach-session", "", "Explicit existing session; requires manifest and baseline-commit; never resumes execution")
	baseline := flag.String("baseline-commit", "", "Original full HEAD for reattachment; dirty changes are preserved")
	check := flag.Bool("check-connection", false, "Verify granted instance and print non-secret identity without starting MCP or a turn")
	printConfig := flag.Bool("print-project-config", false, "Print project-scoped TOML for the actual workspace; no credentials read, files written or network calls")
	workspace := flag.String("workspace", "", "Absolute isolated Git repository (clean unless explicit existing-session reattach)")
	tokenEnv := flag.String("token-env", "CODEAGENT_BRIDGE_TOKEN", "Environment variable containing the daemon access token")
	experimental := flag.Bool("experimental-p0a", false, "Enable isolated technical experiment; no production recovery guarantees")
	flag.Parse()
	if *printConfig {
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		config, err := projectConfig(binary, *manifest, *workspace, *attach, *reattach, *baseline)
		if err != nil {
			return err
		}
		_, err = fmt.Fprint(os.Stdout, config)
		return err
	}
	if !*experimental {
		return fmt.Errorf("explicit --experimental-p0a is required")
	}
	if (*reattach != "" && (*attach != "" || *manifest == "" || *baseline == "")) || (*baseline != "" && *reattach == "") {
		return fmt.Errorf("reattach-session requires manifest and baseline-commit, exclusive with attach-empty-session")
	}
	if *attach != "" && *manifest == "" {
		return fmt.Errorf("attach-empty-session requires connection-manifest")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var c *mcpbridge.Client
	var info mcpbridge.ConnectionInfo
	var err error
	if *manifest != "" {
		conflict := *endpoint != ""
		flag.Visit(func(f *flag.Flag) { conflict = conflict || f.Name == "token-env" })
		if conflict {
			return fmt.Errorf("connection-manifest cannot be combined with runtime-url or token-env")
		}
		if *reattach != "" {
			c, info, err = mcpbridge.NewReattachedFromManifest(ctx, *manifest, *workspace, *reattach, *baseline)
		} else {
			c, info, err = mcpbridge.NewFromManifest(ctx, *manifest, *workspace)
		}
	} else {
		if *check {
			return fmt.Errorf("check-connection requires connection-manifest")
		}
		c, err = mcpbridge.New(ctx, *endpoint, os.Getenv(*tokenEnv), *workspace)
	}
	if err != nil {
		return err
	}
	defer c.Close()
	if *attach != "" {
		if _, err = c.AttachEmptySession(ctx, *attach); err != nil {
			return fmt.Errorf("session_binding: %w; a previously used Session requires explicit reattach and its original baseline", err)
		}
	}
	if *check {
		binding, err := c.BindingInfo(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(struct {
			mcpbridge.ConnectionInfo
			AttachedSessionID   string `json:"attached_session_id,omitempty"`
			WorkspacePath       string `json:"workspace_path"`
			BaselineCommit      string `json:"baseline_commit"`
			NativeToolDiscovery string `json:"native_tool_discovery"`
		}{info, func() string {
			if *reattach != "" {
				return *reattach
			}
			return *attach
		}(), binding.Workspace, binding.Baseline, "not_tested: check codeagent_connection_info in the actual project chat; do not use a temporary stdio/PTY driver"})
	}
	return mcpbridge.Server(c).Run(ctx, &mcp.StdioTransport{})
}
