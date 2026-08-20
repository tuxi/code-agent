package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatcherAppliesAfterAtomicReplace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	applied := make(chan struct{}, 1)
	w, err := Watch(path, 100*time.Millisecond, func() error {
		select {
		case applied <- struct{}{}:
		default:
		}
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(`{"version":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	select {
	case <-applied:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not apply replacement")
	}
}
