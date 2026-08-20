package app

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// Watcher observes an atomically-written settings file and invokes Apply after
// a successful change. It deliberately polls instead of depending on a native
// filesystem watcher: settings files are replaced by rename on macOS/iOS and
// this keeps the embedded and daemon implementations identical.
type Watcher struct {
	path     string
	interval time.Duration
	apply    func() error
	stop     chan struct{}
	done     chan struct{}
	onError  func(error)
	one      sync.Once
}

// Watch starts watching path. The initial file state is only recorded; Apply
// runs after a later change. interval values below 100ms are clamped.
func Watch(path string, interval time.Duration, apply func() error, onError func(error)) (*Watcher, error) {
	if path == "" {
		return nil, fmt.Errorf("settings watcher: empty path")
	}
	if apply == nil {
		return nil, fmt.Errorf("settings watcher: nil apply callback")
	}
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	w := &Watcher{
		path: path, interval: interval, apply: apply,
		stop: make(chan struct{}), done: make(chan struct{}), onError: onError,
	}
	go w.loop(fileFingerprint(path))
	return w, nil
}

func (w *Watcher) loop(last fileState) {
	defer close(w.done)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			current := fileFingerprint(w.path)
			if current == last {
				continue
			}
			if err := w.apply(); err != nil {
				if w.onError != nil {
					w.onError(err)
				}
				// Keep the old fingerprint so a transient partial write is retried.
				continue
			}
			last = current
		case <-w.stop:
			return
		}
	}
}

func (w *Watcher) Close() error {
	w.one.Do(func() { close(w.stop) })
	<-w.done
	return nil
}

type fileState struct {
	modTime time.Time
	size    int64
	mode    os.FileMode
}

func fileFingerprint(path string) fileState {
	info, err := os.Stat(path)
	if err != nil {
		return fileState{}
	}
	return fileState{modTime: info.ModTime(), size: info.Size(), mode: info.Mode()}
}
