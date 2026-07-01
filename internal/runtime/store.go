package runtime

import (
	"code-agent/internal/session"
	"code-agent/internal/session/sqlite"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// StoreFactory is the injection point for storage backends. External consumers
// (like Flux/DreamAI) set this before calling entry-point functions to swap in
// their own storage implementation. The default is the SQLite-based factory that
// stores per-project databases under ~/.codeagent.
//
// This variable mirrors app.Config.StoreFactory but lives at the package level
// so WorkspaceRegistry and other internal callers can access it without plumbing
// a Config through every constructor.
var StoreFactory session.StoreFactory

// OpenStore returns a Store for the given workspace root. If StoreFactory is set
// (by an external consumer), it delegates to it. Otherwise it falls back to the
// built-in SQLite store — the default for CLI/AgentKit users.
func OpenStore(root string) (session.Store, error) {
	if StoreFactory != nil {
		return StoreFactory(root)
	}
	return openSQLiteStore(root)
}

// openSQLiteStore opens (creating if needed) the per-project session database.
// The DB lives under the user's home (see storePath), NOT inside the project
// directory: a project under a synced folder (iCloud Drive, Dropbox, …) would
// otherwise have its SQLite file replaced underneath the open connection, which
// SQLite rejects as SQLITE_READONLY_DBMOVED and which can corrupt the file.
// Sessions stay project-scoped: you resume the conversation for the repo you are in.
func openSQLiteStore(root string) (session.Store, error) {
	path, err := storePath(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create session store dir: %w", err)
	}
	// One-time migration: if there is no DB at the new (non-synced) location but an
	// old in-project .codeagent/sessions.db exists, copy it over so existing
	// sessions are not orphaned by the move. Best-effort — a failed copy just
	// starts the new store empty.
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if old := filepath.Join(root, ".codeagent", "sessions.db"); old != path {
			if _, err := os.Stat(old); err == nil {
				_ = copyDB(old, path)
			}
		}
	}

	store, err := sqlite.New(path)
	if err != nil {
		// The DB won't open — it may be a corrupt copy migrated from a synced
		// folder that was being clobbered. Quarantine it (non-destructively, kept
		// for manual recovery) and start fresh rather than block startup forever.
		quarantine := path + ".corrupt-" + time.Now().Format("20060102-150405")
		if os.Rename(path, quarantine) == nil {
			// Move the WAL sidecars aside with it: a stale -wal/-shm left next to
			// the path would attach to the freshly-created DB and re-corrupt it,
			// and keeping them with the quarantine preserves a complete recovery
			// set. Best-effort — a missing sidecar is normal.
			for _, ext := range dbSidecars {
				_ = os.Rename(path+ext, quarantine+ext)
			}
			fmt.Fprintf(os.Stderr, "warning: session DB unreadable (%v); moved aside to %s, starting fresh\n", err, quarantine)
			return sqlite.New(path)
		}
		return nil, err
	}
	return store, nil
}

// storePath returns the session DB path for a workspace root, under the user's
// home rather than the project dir (so it is never in a cloud-synced folder).
// Sessions remain project-scoped via a per-project key: the basename plus a short
// hash of the absolute path, so two projects sharing a basename do not collide.
//
// When a storeBaseDir is set (embedded/iOS host), the path no longer hashes the
// root — because on iOS the root's absolute path contains the sandbox container
// UUID, which changes across reinstalls. Hashing a moving target would orphan
// every previous session DB. Instead a fixed key keeps the DB at a single,
// stable location under the host-supplied data directory. (A non-embedded desktop
// has stable $HOME paths so the hash remains correct there.)
func storePath(root string) (string, error) {
	base := storeBaseDir
	if base == "" {
		abs, err := filepath.Abs(root)
		if err != nil {
			return "", err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".codeagent")
		sum := sha256.Sum256([]byte(abs))
		key := filepath.Base(abs) + "-" + hex.EncodeToString(sum[:])[:12]
		return filepath.Join(base, "projects", key, "sessions.db"), nil
	}
	// Embedded host (iOS): storeBaseDir is already per-app and writable.
	// Use a fixed key so the DB location survives sandbox-path (UUID) changes.
	return filepath.Join(base, "sessions.db"), nil
}

// storeBaseDir optionally overrides the directory under which session databases
// live. Empty => $HOME/.codeagent (the desktop default, with per-project hashing).
// Embedded hosts set it because on iOS $HOME is the read-only sandbox container;
// they point it at a writable app-data directory such as Library/Application
// Support. When set, per-project hashing is disabled (the host is assumed to have
// a single workspace), so the DB location is stable across reinstalls.
var storeBaseDir string

// SetStoreBaseDir overrides the session-store base directory process-wide. Pass an
// absolute, writable path; call it before any store is opened. Empty restores the
// $HOME/.codeagent default.
func SetStoreBaseDir(dir string) { storeBaseDir = dir }

// dbSidecars are the WAL-mode auxiliary files SQLite keeps beside the main DB.
// Since v1.2 the store runs in WAL (see sqlite.Store.open), so every copy / move /
// quarantine of the DB must carry these too: a main file without its -wal drops
// every committed-but-not-yet-checkpointed transaction — precisely the tail a
// crash-safe checkpoint exists to preserve. See
// docs/protocols/agent-wire-v1.2-lifecycle-suspend-resume.md §2.2.2.
var dbSidecars = []string{"-wal", "-shm"}

// copyDB copies a SQLite database and its WAL sidecars from src to dst. It is the
// WAL-safe form of copyFile used by the one-time migration: copying only the main
// file would silently lose the -wal tail. Sidecar copies are best-effort — an
// absent sidecar (the DB was checkpointed, or predates WAL) is normal, not an error.
func copyDB(src, dst string) error {
	if err := copyFile(src, dst); err != nil {
		return err
	}
	for _, ext := range dbSidecars {
		if _, err := os.Stat(src + ext); err == nil {
			_ = copyFile(src+ext, dst+ext)
		}
	}
	return nil
}

// copyFile copies src to dst — a best-effort one-time migration of an existing
// session DB snapshot to the new location.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
