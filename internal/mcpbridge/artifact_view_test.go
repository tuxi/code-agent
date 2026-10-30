package mcpbridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

const sampleDiff = `diff --git a/Sources/A.swift b/Sources/A.swift
index 1111111..2222222 100644
--- a/Sources/A.swift
+++ b/Sources/A.swift
@@ -1,3 +1,4 @@
 // 中文注释：主轨封面 🎬
+let emoji = "🎬🎞️"
 let x = 1
diff --git a/docs/shot.png b/docs/shot.png
new file mode 100644
index 0000000..3333333
Binary files /dev/null and b/docs/shot.png differ
diff --git a/B.swift b/B.swift
deleted file mode 100644
--- a/B.swift
+++ /dev/null
@@ -1 +0,0 @@
-old line
`

func sampleArtifacts() Artifacts {
	sum := sha256.Sum256([]byte(sampleDiff))
	return Artifacts{
		SessionID: "20261030-052252-b8f78ce8", TurnID: "turn_1",
		TurnStatus: "completed", Baseline: strings.Repeat("a", 40),
		DeliveryCommit: strings.Repeat("b", 40), EvidenceSource: "commit_range",
		Diff: sampleDiff, DiffSHA256: hex.EncodeToString(sum[:]),
		CommandResults:  []CommandEvidence{{CallID: "c1", Command: "swift test", ExitCode: intPtr(0), Stdout: strings.Repeat("x", 9000)}},
		UnparsedResults: []string{},
	}
}

func intPtr(v int) *int { return &v }

// Splitting must be byte preserving: the parts must rebuild the whole diff.
func TestSplitDiffReconstructsExactly(t *testing.T) {
	files, err := splitDiff(sampleDiff)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("files = %d, want 3", len(files))
	}
	var rebuilt strings.Builder
	for _, f := range files {
		rebuilt.WriteString(f.raw)
	}
	if rebuilt.String() != sampleDiff {
		t.Fatal("concatenated file patches differ from the original diff")
	}
	if files[0].path != "Sources/A.swift" || files[0].status != "modified" {
		t.Fatalf("file 0 = %+v", files[0])
	}
	if files[1].path != "docs/shot.png" || !files[1].binary || files[1].status != "added" {
		t.Fatalf("binary file = %+v", files[1])
	}
	if files[2].path != "B.swift" || files[2].status != "deleted" {
		t.Fatalf("deleted file = %+v", files[2])
	}
	sum := sha256.Sum256([]byte(files[0].raw))
	if files[0].sha != hex.EncodeToString(sum[:]) {
		t.Fatal("per-file hash mismatch")
	}
}

func TestSummaryDropsDiffAndCommandOutput(t *testing.T) {
	a, err := projectArtifacts(sampleArtifacts(), ArtifactRequest{TurnID: "turn_1", Mode: "summary"}, "/b.json", "digest")
	if err != nil {
		t.Fatal(err)
	}
	if a.Diff != "" {
		t.Fatal("summary must not carry the diff body")
	}
	if len(a.CommandResults) != 0 {
		t.Fatal("summary must not repeat command output")
	}
	if a.View == nil || a.View.Mode != "summary" || len(a.View.Files) != 3 {
		t.Fatalf("view = %+v", a.View)
	}
	if a.View.EvidenceBundle != "/b.json" || a.View.BundleSHA256 != "digest" {
		t.Fatal("summary must bind the evidence identity")
	}
	if a.View.TotalAdditions != 1 || a.View.TotalDeletions != 1 {
		t.Fatalf("totals = %d/%d", a.View.TotalAdditions, a.View.TotalDeletions)
	}
	if a.DiffSHA256 == "" || a.Baseline == "" || a.DeliveryCommit == "" {
		t.Fatal("binding fields must survive the projection")
	}
}

// Every page must be valid UTF-8, and paging must terminate and reassemble the
// exact patch. Small limits force cuts in the middle of Chinese and emoji.
func TestPerFilePaginationIsUTF8SafeAndComplete(t *testing.T) {
	for _, limit := range []int{1, 2, 3, 5, 7, 11, 64} {
		a, err := projectArtifacts(sampleArtifacts(), ArtifactRequest{
			TurnID: "turn_1", Mode: "file", Path: "Sources/A.swift", Limit: limit}, "", "")
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if a.View == nil {
			t.Fatalf("limit %d: nil view", limit)
		}
		var rebuilt strings.Builder
		offset := 0
		pages := 0
		for {
			pages++
			if pages > 5000 {
				t.Fatalf("limit %d: pagination did not terminate", limit)
			}
			page, err := projectArtifacts(sampleArtifacts(), ArtifactRequest{
				TurnID: "turn_1", Mode: "file", Path: "Sources/A.swift", Offset: offset, Limit: limit}, "", "")
			if err != nil {
				t.Fatalf("limit %d offset %d: %v", limit, offset, err)
			}
			if !utf8.ValidString(page.View.Patch) {
				t.Fatalf("limit %d offset %d: page is not valid UTF-8", limit, offset)
			}
			sum := sha256.Sum256([]byte(page.View.Patch))
			if page.View.ChunkSHA256 != hex.EncodeToString(sum[:]) {
				t.Fatalf("limit %d offset %d: chunk hash mismatch", limit, offset)
			}
			rebuilt.WriteString(page.View.Patch)
			if page.View.Complete {
				if page.View.NextOffset != 0 {
					t.Fatalf("limit %d: final page must set next_offset=0, got %d", limit, page.View.NextOffset)
				}
				if rebuilt.Len() != page.View.PatchBytes {
					t.Fatalf("limit %d: reassembled %d bytes, want %d", limit, rebuilt.Len(), page.View.PatchBytes)
				}
				full := sha256.Sum256([]byte(rebuilt.String()))
				if hex.EncodeToString(full[:]) != page.View.FilePatchSHA256 {
					t.Fatalf("limit %d: reassembled patch does not match file_patch_sha256", limit)
				}
				break
			}
			if page.View.NextOffset <= offset {
				t.Fatalf("limit %d offset %d: next_offset did not advance", limit, offset)
			}
			offset = page.View.NextOffset
		}
	}
}

func TestPaginationRejectsWrongEvidenceVersion(t *testing.T) {
	_, err := projectArtifacts(sampleArtifacts(), ArtifactRequest{
		TurnID: "turn_1", Mode: "summary", ExpectDiffSHA: strings.Repeat("0", 64)}, "", "")
	if !errors.Is(err, ErrEvidenceMismatch) {
		t.Fatalf("err = %v, want ErrEvidenceMismatch", err)
	}
	// The matching hash must pass.
	if _, err := projectArtifacts(sampleArtifacts(), ArtifactRequest{
		TurnID: "turn_1", Mode: "summary", ExpectDiffSHA: sampleArtifacts().DiffSHA256}, "", ""); err != nil {
		t.Fatalf("matching hash rejected: %v", err)
	}
}

// A binary file must never look like reviewed content.
func TestBinaryReturnsMetadataOnly(t *testing.T) {
	a, err := projectArtifacts(sampleArtifacts(), ArtifactRequest{
		TurnID: "turn_1", Mode: "file", Path: "docs/shot.png"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !a.View.Binary || a.View.ContentProvided || a.View.Patch != "" {
		t.Fatalf("binary view = %+v", a.View)
	}
	if !strings.Contains(a.View.Note, "NOT a visual review") {
		t.Fatalf("binary note must refuse visual-review claims: %q", a.View.Note)
	}
	if !a.View.Complete {
		t.Fatal("binary metadata should be complete on the first call")
	}
}

func TestUnknownPathIsRejected(t *testing.T) {
	if _, err := projectArtifacts(sampleArtifacts(), ArtifactRequest{
		TurnID: "turn_1", Mode: "file", Path: "nope.swift"}, "", ""); err == nil {
		t.Fatal("unknown path must be rejected")
	}
}

func TestOffsetPastPatchIsRejected(t *testing.T) {
	if _, err := projectArtifacts(sampleArtifacts(), ArtifactRequest{
		TurnID: "turn_1", Mode: "file", Path: "Sources/A.swift", Offset: 1 << 20}, "", ""); err == nil {
		t.Fatal("offset past the patch must be rejected")
	}
}

// The executor response must not gain any new key.
func TestExecutorArtifactsShapeUnchanged(t *testing.T) {
	raw, err := json.Marshal(sampleArtifacts())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"view"`) {
		t.Fatal("executor artifacts must not carry a view key")
	}
	if strings.Contains(string(raw), `"output_truncated"`) {
		t.Fatal("executor command evidence must not carry output_truncated when not truncated")
	}
}

// Opt-in: run the real Case 001 bundle through the reviewer view.
//
//	CODEAGENT_REVIEW_BUNDLE=/path/to/case-001.review.json go test ./internal/mcpbridge -run TestRealBundleBudget -v
func TestRealBundleBudget(t *testing.T) {
	path := os.Getenv("CODEAGENT_REVIEW_BUNDLE")
	if path == "" {
		t.Skip("explicit review bundle required")
	}
	reader, err := OpenBundle(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	summary, err := reader.ReviewArtifacts(ctx, ArtifactRequest{TurnID: "turn_18e33a6ab0de1858_45", Mode: "summary"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("summary bytes = %d, files = %d", len(raw), len(summary.View.Files))
	if len(raw) > 8<<10 {
		t.Fatalf("summary is %d bytes, budget is 8 KiB", len(raw))
	}
	if summary.Diff != "" || len(summary.CommandResults) != 0 {
		t.Fatal("summary leaked diff or command output")
	}

	// Walk the largest text file page by page and verify the three hash levels.
	var target ArtifactFile
	for _, f := range summary.View.Files {
		if !f.Binary && f.PatchBytes > target.PatchBytes {
			target = f
		}
	}
	if target.Path == "" {
		t.Fatal("no text file in evidence")
	}
	// Binary screenshots must never be presented as reviewed content.
	for _, f := range summary.View.Files {
		if !f.Binary {
			continue
		}
		page, err := reader.ReviewArtifacts(ctx, ArtifactRequest{
			TurnID: "turn_18e33a6ab0de1858_45", Mode: "file", Path: f.Path})
		if err != nil {
			t.Fatal(err)
		}
		if page.View.Patch != "" || page.View.ContentProvided || !strings.Contains(page.View.Note, "NOT a visual review") {
			t.Fatalf("binary %s leaked content or accepted a visual-review claim", f.Path)
		}
	}
	// Page with a small limit as well: the real file must survive multi-page reads.
	for _, limit := range []int{0, 4096} {
		var rebuilt strings.Builder
		offset, pages := 0, 0
		for {
			pages++
			page, err := reader.ReviewArtifacts(ctx, ArtifactRequest{
				TurnID: "turn_18e33a6ab0de1858_45", Mode: "file", Path: target.Path,
				Offset: offset, Limit: limit, ExpectDiffSHA: summary.DiffSHA256})
			if err != nil {
				t.Fatal(err)
			}
			if page.View.ChunkBytes > maxChunkBytes {
				t.Fatalf("chunk %d exceeds hard cap", page.View.ChunkBytes)
			}
			if !utf8.ValidString(page.View.Patch) {
				t.Fatal("page is not valid UTF-8")
			}
			rebuilt.WriteString(page.View.Patch)
			if page.View.Complete {
				if page.View.NextOffset != 0 {
					t.Fatal("final page must set next_offset=0")
				}
				break
			}
			offset = page.View.NextOffset
		}
		sum := sha256.Sum256([]byte(rebuilt.String()))
		if hex.EncodeToString(sum[:]) != target.FilePatchSHA256 {
			t.Fatalf("limit %d: reassembled %s does not match file_patch_sha256", limit, target.Path)
		}
		t.Logf("limit %d: reassembled %s in %d pages, %d bytes, sha ok", limit, target.Path, pages, rebuilt.Len())
	}
}
