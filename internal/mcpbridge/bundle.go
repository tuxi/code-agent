package mcpbridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"code-agent/internal/collab"
)

// An evidence bundle is the only way a third-party client can review a Session
// without holding the owner credential. It is exported once by the owner, from
// a live observation binding, and then served read-only from disk with no
// network access and no token. Its integrity is the sidecar SHA-256.
const bundleSchemaVersion = 1
const maxBundleBytes = 64 << 20

type EvidenceBundle struct {
	SchemaVersion    int                  `json:"schema_version"`
	ExportedAt       string               `json:"exported_at"`
	ExportedBy       string               `json:"exported_by,omitempty"`
	SessionID        string               `json:"session_id"`
	TurnID           string               `json:"turn_id"`
	Workspace        string               `json:"workspace_path"`
	Baseline         string               `json:"baseline_commit"`
	Delivery         string               `json:"delivery_commit"`
	Read             ReadResult           `json:"read"`
	Artifacts        Artifacts            `json:"artifacts"`
	Handoff          Handoff              `json:"handoff"`
	Task             *collab.Snapshot     `json:"task,omitempty"`
	ReviewBundle     *collab.ReviewBundle `json:"review_bundle,omitempty"`
	ReviewArtifactID string               `json:"review_artifact_id,omitempty"`
}

// ExportEvidence freezes one exact Turn into a reviewable file. It requires a
// live binding because the evidence itself lives in the daemon.
func ExportEvidence(ctx context.Context, c *Client, turnID, taskID, artifactID, exportedBy, out string) (EvidenceBundle, string, error) {
	var b EvidenceBundle
	if !validID(turnID) {
		return b, "", errors.New("valid turn_id is required")
	}
	if !filepath.IsAbs(out) {
		return b, "", errors.New("evidence output must be an absolute path")
	}
	session := c.BoundSessionID()
	if session == "" {
		return b, "", errors.New("export requires a bound Session")
	}
	read, err := c.Read(ctx, session, turnID, 0)
	if err != nil {
		return b, "", err
	}
	if !read.Terminal {
		return b, "", errors.New("the exact Turn is not terminal; freeze evidence after it finishes")
	}
	artifacts, err := c.Artifacts(ctx, session, turnID)
	if err != nil {
		return b, "", err
	}
	handoff, err := c.HandoffForTask(ctx, session, turnID, taskID)
	if err != nil {
		return b, "", err
	}
	b = EvidenceBundle{SchemaVersion: bundleSchemaVersion, ExportedAt: time.Now().UTC().Format(time.RFC3339), ExportedBy: exportedBy,
		SessionID: session, TurnID: turnID, Workspace: c.root, Baseline: c.baseline, Delivery: c.delivery,
		Read: read, Artifacts: artifacts, Handoff: handoff}
	if taskID != "" {
		snapshot, err := c.ReadTask(ctx, taskID)
		if err != nil {
			return b, "", err
		}
		b.Task = &snapshot
	}
	if artifactID != "" {
		if taskID == "" {
			return b, "", errors.New("a review bundle requires its task_id")
		}
		rb, err := c.ReadReviewBundle(ctx, taskID, artifactID)
		if err != nil {
			return b, "", err
		}
		b.ReviewBundle, b.ReviewArtifactID = &rb, artifactID
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return b, "", err
	}
	if err := writeFile0600(out, data); err != nil {
		return b, "", err
	}
	digest := sha256.Sum256(data)
	sum := hex.EncodeToString(digest[:])
	if err := writeFile0600(out+".sha256", []byte(sum+"  "+filepath.Base(out)+"\n")); err != nil {
		return b, "", err
	}
	return b, sum, nil
}

func writeFile0600(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// BundleReader serves a frozen bundle. It has no credential, no HTTP client and
// no way to reach the daemon.
type BundleReader struct {
	path   string
	bundle EvidenceBundle
	digest string
}

func (b *BundleReader) Digest() string { return b.digest }
func (b *BundleReader) Path() string   { return b.path }

func OpenBundle(path string) (*BundleReader, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("evidence bundle path must be absolute")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxBundleBytes {
		return nil, errors.New("evidence bundle is missing, not a regular file, or too large")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read evidence bundle")
	}
	sumFile, err := os.ReadFile(path + ".sha256")
	if err != nil {
		return nil, errors.New("evidence bundle requires its .sha256 sidecar")
	}
	want := strings.TrimSpace(strings.Fields(string(sumFile))[0])
	digest := sha256.Sum256(data)
	got := hex.EncodeToString(digest[:])
	if len(want) != len(got) || !strings.EqualFold(want, got) {
		return nil, errors.New("evidence bundle hash does not match its sidecar; refusing tampered or replaced evidence")
	}
	var b EvidenceBundle
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return nil, errors.New("evidence bundle is not a valid review document")
	}
	if b.SchemaVersion != bundleSchemaVersion || !validID(b.SessionID) || !validID(b.TurnID) ||
		!fullCommitPattern.MatchString(b.Baseline) || !fullCommitPattern.MatchString(b.Delivery) ||
		b.Read.SessionID != b.SessionID || b.Artifacts.SessionID != b.SessionID || b.Handoff.SessionID != b.SessionID {
		return nil, errors.New("evidence bundle is internally inconsistent")
	}
	return &BundleReader{path: path, bundle: b, digest: got}, nil
}

func (b *BundleReader) BindingInfo(context.Context) (BindingInfo, error) {
	return BindingInfo{Workspace: b.bundle.Workspace, Baseline: b.bundle.Baseline, SessionID: b.bundle.SessionID,
		Delivery: b.bundle.Delivery, EvidenceBundle: b.path, BundleVerified: true}, nil
}

func (b *BundleReader) ReadSession(_ context.Context, turnID string, cursor int64) (ReadResult, error) {
	if turnID != b.bundle.TurnID {
		return ReadResult{}, fmt.Errorf("bundle contains only Turn %s", b.bundle.TurnID)
	}
	r := b.bundle.Read
	r.Events = []Event{}
	for _, e := range b.bundle.Read.Events {
		if e.Seq > cursor {
			r.Events = append(r.Events, e)
		}
	}
	if r.PendingControls == nil {
		r.PendingControls = []any{}
	}
	return r, nil
}

// AwaitUpdate never waits: a frozen bundle cannot change.
func (b *BundleReader) AwaitUpdate(_ context.Context, turnID string, cursor int64, _ time.Duration) (Update, error) {
	r, err := b.ReadSession(context.Background(), turnID, cursor)
	if err != nil {
		return Update{}, err
	}
	return Update{SessionID: r.SessionID, TurnID: r.TurnID, Cursor: r.Cursor, Changed: false, ExecutionStatus: r.Status, Terminal: r.Terminal, Attention: r.Attention}, nil
}

func (b *BundleReader) ReviewHandoff(_ context.Context, turnID, taskID string) (Handoff, error) {
	if turnID != b.bundle.TurnID {
		return Handoff{}, fmt.Errorf("bundle contains only Turn %s", b.bundle.TurnID)
	}
	h := b.bundle.Handoff
	if taskID != "" {
		if b.bundle.Task == nil || b.bundle.Task.TaskID != taskID {
			return Handoff{}, collab.ErrNotFound
		}
		h.TaskID, h.TaskRevision, h.TaskAcceptance = b.bundle.Task.TaskID, b.bundle.Task.Revision, b.bundle.Task.TaskAcceptance
	}
	return h, nil
}

func (b *BundleReader) ReviewArtifacts(_ context.Context, turnID string) (Artifacts, error) {
	if turnID != b.bundle.TurnID {
		return Artifacts{}, fmt.Errorf("bundle contains only Turn %s", b.bundle.TurnID)
	}
	return b.bundle.Artifacts, nil
}

func (b *BundleReader) TaskSnapshot(_ context.Context, taskID string) (collab.Snapshot, error) {
	if b.bundle.Task == nil || b.bundle.Task.TaskID != taskID {
		return collab.Snapshot{}, collab.ErrNotFound
	}
	return *b.bundle.Task, nil
}

func (b *BundleReader) ReviewBundle(_ context.Context, taskID, artifactID string) (collab.ReviewBundle, error) {
	if b.bundle.ReviewBundle == nil || b.bundle.ReviewArtifactID != artifactID || (b.bundle.Task != nil && b.bundle.Task.TaskID != taskID) {
		return collab.ReviewBundle{}, collab.ErrNotFound
	}
	return *b.bundle.ReviewBundle, nil
}
