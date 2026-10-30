package mcpbridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// These tests measure the serialized MCP result, not the patch length. The SDK
// writes the same JSON into structuredContent and content[].text, so a 1 KiB
// patch costs roughly 2.3 KiB on the wire. Budgets here are what a client
// actually receives.

type synthFile struct {
	path  string
	lines []string
}

func buildDiff(files []synthFile) string {
	var b strings.Builder
	for _, f := range files {
		b.WriteString("diff --git a/" + f.path + " b/" + f.path + "\n")
		b.WriteString("index 1111111..2222222 100644\n")
		b.WriteString("--- a/" + f.path + "\n")
		b.WriteString("+++ b/" + f.path + "\n")
		b.WriteString("@@ -1 +1," + itoa(len(f.lines)) + " @@\n")
		for _, line := range f.lines {
			b.WriteString("+" + line + "\n")
		}
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func syntheticBundle(t *testing.T, files []synthFile) *BundleReader {
	t.Helper()
	diff := buildDiff(files)
	sum := sha256.Sum256([]byte(diff))
	artifacts := Artifacts{
		SessionID: "20261030-052252-b8f78ce8", TurnID: "turn_1", TurnStatus: "completed",
		Baseline: strings.Repeat("a", 40), DeliveryCommit: strings.Repeat("b", 40),
		EvidenceSource: "commit_range", EvidenceComplete: true,
		Diff: diff, DiffSHA256: hex.EncodeToString(sum[:]),
	}
	return &BundleReader{
		path:   "/tmp/synthetic.json",
		digest: "synthetic-digest",
		bundle: EvidenceBundle{SchemaVersion: bundleSchemaVersion, SessionID: "20261030-052252-b8f78ce8",
			TurnID: "turn_1", Baseline: artifacts.Baseline, Delivery: artifacts.DeliveryCommit,
			Artifacts: artifacts},
	}
}

func callArtifacts(t *testing.T, reader ReviewReader, args map[string]any) (*mcp.CallToolResult, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, b := mcp.NewInMemoryTransports()
	server, err := ReviewerServer(reader).Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "wire"}, nil).Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "codeagent_get_artifacts", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return result, len(raw)
}

func viewOf(t *testing.T, result *mcp.CallToolResult) map[string]any {
	t.Helper()
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("structured content is not JSON: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("structured content is not a JSON object: %v", err)
	}
	view, _ := payload["view"].(map[string]any)
	if view == nil {
		t.Fatal("no view in response")
	}
	return view
}

// The summary keeps the file list and must stay small.
func TestSummaryWireBudget(t *testing.T) {
	files := []synthFile{{path: "Sources/A.swift", lines: []string{"let a = 1"}}}
	_, size := callArtifacts(t, syntheticBundle(t, files), map[string]any{"turn_id": "turn_1"})
	if size > 12<<10 {
		t.Fatalf("summary response is %d bytes, budget is 12 KiB", size)
	}
	t.Logf("summary wire = %d bytes", size)
}

// A page must not repeat the file list, and its wire size must fit the budget
// even when the patch fills the cap.
func TestPageWireBudgetAndNoFileList(t *testing.T) {
	long := strings.Repeat("0123456789", 400) // 4000 chars on one line
	files := []synthFile{
		{path: "Sources/A.swift", lines: []string{long, long}},
		{path: "Sources/B.swift", lines: []string{"let b = 2"}},
	}
	reader := syntheticBundle(t, files)
	result, size := callArtifacts(t, reader, map[string]any{
		"turn_id": "turn_1", "diff": "file", "path": "Sources/A.swift"})
	view := viewOf(t, result)
	if _, ok := view["files"]; ok {
		t.Fatal("page response must not repeat the files array")
	}
	patch, _ := view["patch"].(string)
	if len(patch) > maxChunkBytes {
		t.Fatalf("patch %d exceeds cap %d", len(patch), maxChunkBytes)
	}
	if size > 24<<10 {
		t.Fatalf("page response is %d bytes, budget is 24 KiB (patch %d)", size, len(patch))
	}
	ratio := float64(size) / float64(len(patch))
	if ratio > 3.0 {
		t.Fatalf("wire/patch ratio %.2f exceeds 3.0; the envelope grew", ratio)
	}
	t.Logf("page wire = %d bytes for %d patch bytes (%.2fx)", size, len(patch), ratio)
}

// Escaping-heavy content, CJK, emoji and a long path must survive paging with
// the hash intact and without blowing the budget.
func TestEscapingHeavyAndLongPath(t *testing.T) {
	longPath := strings.Repeat("深/", 60) + "文件.swift" // 180+ chars, multi-byte
	body := []string{
		`let quote = "he said \"hi\""`,
		`let backslash = "C:\\tmp\\x"`,
		"let tab = \"a\tb\"",
		"// 中文注释：主轨封面 🎬🎞️",
		"let emoji = \"🎬🎞️🎥\"",
		strings.Repeat("x", 500),
	}
	reader := syntheticBundle(t, []synthFile{{path: longPath, lines: body}})
	summary, _ := callArtifacts(t, reader, map[string]any{"turn_id": "turn_1"})
	view := viewOf(t, summary)
	list, _ := view["files"].([]any)
	if len(list) != 1 {
		t.Fatalf("summary files = %d", len(list))
	}
	entry := list[0].(map[string]any)
	if entry["path"] != longPath {
		t.Fatalf("long path did not round-trip: %q", entry["path"])
	}
	wantSHA, _ := entry["file_patch_sha256"].(string)

	var rebuilt strings.Builder
	offset, pages := 0, 0
	for {
		pages++
		if pages > 100 {
			t.Fatal("pagination did not terminate")
		}
		res, size := callArtifacts(t, reader, map[string]any{
			"turn_id": "turn_1", "diff": "file", "path": longPath, "offset": offset})
		if size > 24<<10 {
			t.Fatalf("page %d is %d bytes", pages, size)
		}
		v := viewOf(t, res)
		chunk, _ := v["patch"].(string)
		if !utf8.ValidString(chunk) {
			t.Fatalf("page %d is not valid UTF-8", pages)
		}
		sum := sha256.Sum256([]byte(chunk))
		if v["chunk_sha256"] != hex.EncodeToString(sum[:]) {
			t.Fatalf("page %d chunk hash mismatch", pages)
		}
		rebuilt.WriteString(chunk)
		if complete, _ := v["complete"].(bool); complete {
			break
		}
		next, _ := v["next_offset"].(float64)
		if int(next) <= offset {
			t.Fatalf("page %d did not advance", pages)
		}
		offset = int(next)
	}
	full := sha256.Sum256([]byte(rebuilt.String()))
	if hex.EncodeToString(full[:]) != wantSHA {
		t.Fatal("reassembled patch does not match file_patch_sha256")
	}
	t.Logf("long path + escaping: %d bytes in %d pages, sha ok", rebuilt.Len(), pages)
}

// A limit above the cap must clamp, and the next page must start exactly at the
// cap so pagination stays deterministic.
func TestCapClampsAndAdvancesDeterministically(t *testing.T) {
	files := []synthFile{{path: "Sources/Big.swift", lines: []string{strings.Repeat("y", 20000)}}}
	reader := syntheticBundle(t, files)
	first, _ := callArtifacts(t, reader, map[string]any{
		"turn_id": "turn_1", "diff": "file", "path": "Sources/Big.swift", "limit": 1 << 20})
	v1 := viewOf(t, first)
	if int(v1["chunk_bytes"].(float64)) != maxChunkBytes {
		t.Fatalf("chunk = %v, want cap %d", v1["chunk_bytes"], maxChunkBytes)
	}
	if int(v1["next_offset"].(float64)) != maxChunkBytes {
		t.Fatalf("next_offset = %v, want %d", v1["next_offset"], maxChunkBytes)
	}
	if complete, _ := v1["complete"].(bool); complete {
		t.Fatal("first page must not be complete")
	}
	second, _ := callArtifacts(t, reader, map[string]any{
		"turn_id": "turn_1", "diff": "file", "path": "Sources/Big.swift", "offset": maxChunkBytes})
	v2 := viewOf(t, second)
	if int(v2["offset"].(float64)) != maxChunkBytes {
		t.Fatalf("second page offset = %v", v2["offset"])
	}
	if len(v2["patch"].(string)) == 0 {
		t.Fatal("second page is empty")
	}
}

// Near-limit patch: one full page plus a short tail, and the tail ends paging.
func TestNearLimitPatchPaginatesCleanly(t *testing.T) {
	// Header is ~120 bytes, so this lands just over one cap.
	files := []synthFile{{path: "Sources/Edge.swift", lines: []string{strings.Repeat("z", maxChunkBytes-100)}}}
	reader := syntheticBundle(t, files)
	summary, _ := callArtifacts(t, reader, map[string]any{"turn_id": "turn_1"})
	entry := viewOf(t, summary)["files"].([]any)[0].(map[string]any)
	total := int(entry["patch_bytes"].(float64))
	if total <= maxChunkBytes {
		t.Fatalf("fixture is %d bytes, expected more than one page", total)
	}
	first, _ := callArtifacts(t, reader, map[string]any{"turn_id": "turn_1", "diff": "file", "path": "Sources/Edge.swift"})
	v1 := viewOf(t, first)
	if complete, _ := v1["complete"].(bool); complete {
		t.Fatal("first page should not be complete")
	}
	second, _ := callArtifacts(t, reader, map[string]any{
		"turn_id": "turn_1", "diff": "file", "path": "Sources/Edge.swift",
		"offset": int(v1["next_offset"].(float64))})
	v2 := viewOf(t, second)
	if complete, _ := v2["complete"].(bool); !complete {
		t.Fatal("second page should complete the patch")
	}
	if int(v2["next_offset"].(float64)) != 0 {
		t.Fatal("final page must set next_offset=0")
	}
	if int(v1["chunk_bytes"].(float64))+int(v2["chunk_bytes"].(float64)) != total {
		t.Fatal("pages do not add up to patch_bytes")
	}
}
