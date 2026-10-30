// codeagent-mcp is an opt-in, loopback-only stdio bridge to an existing
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
	observe := flag.String("observe-session", "", "Read-only observation of a completed session against a recorded commit range; requires manifest, baseline-commit and delivery-commit")
	baseline := flag.String("baseline-commit", "", "Original full HEAD for reattachment/observation; dirty changes are preserved")
	delivery := flag.String("delivery-commit", "", "Recorded full delivery commit for observation; the reviewed range is baseline..delivery")
	profile := flag.String("profile", "executor", "executor (default) or reviewer (read-only tool surface)")
	bundlePath := flag.String("evidence-bundle", "", "Offline frozen evidence bundle; exclusive with every live connection flag and requires no credential")
	exportPath := flag.String("export-evidence", "", "Write a frozen evidence bundle for this bound session, then exit")
	exportTurn := flag.String("export-turn", "", "Exact terminal Turn to freeze into --export-evidence")
	exportTask := flag.String("export-task", "", "Optional Task to include in the evidence bundle")
	exportArtifact := flag.String("export-artifact", "", "Optional managed review artifact to include")
	exportedBy := flag.String("exported-by", "", "Optional non-secret label recorded in the bundle (for example: codex)")
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
	if *profile != "executor" && *profile != "reviewer" {
		return fmt.Errorf("--profile must be executor or reviewer")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Offline reviewer: a frozen bundle is served with no credential, no HTTP
	// client and no daemon access. This is the only mode a third-party client
	// should be given.
	if *bundlePath != "" {
		if *manifest != "" || *endpoint != "" || *workspace != "" || *attach != "" || *reattach != "" || *observe != "" || *check || *exportPath != "" {
			return fmt.Errorf("--evidence-bundle is exclusive with every live connection flag")
		}
		reader, err := mcpbridge.OpenBundle(*bundlePath)
		if err != nil {
			return err
		}
		return mcpbridge.ReviewerServer(reader).Run(ctx, &mcp.StdioTransport{})
	}
	if (*reattach != "" && (*attach != "" || *manifest == "" || *baseline == "")) || (*baseline != "" && *reattach == "" && *observe == "") {
		return fmt.Errorf("reattach-session requires manifest and baseline-commit, exclusive with attach-empty-session")
	}
	if *observe != "" && (*reattach != "" || *attach != "" || *manifest == "" || *workspace == "" || *baseline == "" || *delivery == "") {
		return fmt.Errorf("observe-session requires connection-manifest, workspace, baseline-commit and delivery-commit, and is exclusive with attach/reattach")
	}
	if *delivery != "" && *observe == "" {
		return fmt.Errorf("delivery-commit is only valid with observe-session")
	}
	if *attach != "" && *manifest == "" {
		return fmt.Errorf("attach-empty-session requires connection-manifest")
	}
	var c *mcpbridge.Client
	var info mcpbridge.ConnectionInfo
	var err error
	if *observe != "" {
		c, info, _, err = mcpbridge.NewObserverFromManifest(ctx, *manifest, *workspace, *observe, *baseline, *delivery)
	} else if *manifest != "" {
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
	if *exportPath != "" {
		if *exportTurn == "" {
			return fmt.Errorf("export-evidence requires export-turn")
		}
		bundle, digest, err := mcpbridge.ExportEvidence(ctx, c, *exportTurn, *exportTask, *exportArtifact, *exportedBy, *exportPath)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(struct {
			Path      string `json:"path"`
			SHA256    string `json:"sha256"`
			SessionID string `json:"session_id"`
			TurnID    string `json:"turn_id"`
			Baseline  string `json:"baseline_commit"`
			Delivery  string `json:"delivery_commit"`
			Evidence  string `json:"evidence_source"`
		}{*exportPath, digest, bundle.SessionID, bundle.TurnID, bundle.Baseline, bundle.Delivery, bundle.Artifacts.EvidenceSource})
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
			DeliveryCommit      string `json:"delivery_commit,omitempty"`
			NativeToolDiscovery string `json:"native_tool_discovery"`
		}{info, func() string {
			if *reattach != "" {
				return *reattach
			}
			if *observe != "" {
				return *observe
			}
			return *attach
		}(), binding.Workspace, binding.Baseline, binding.Delivery, "not_tested: check codeagent_connection_info in the actual project chat; do not use a temporary stdio/PTY driver"})
	}
	// Observation is read-only by construction, so it always uses the reviewer
	// surface even when the profile flag was left at its default.
	if *profile == "reviewer" || *observe != "" {
		return mcpbridge.ReviewerServer(mcpbridge.NewClientReviewReader(c)).Run(ctx, &mcp.StdioTransport{})
	}
	return mcpbridge.Server(c).Run(ctx, &mcp.StdioTransport{})
}
