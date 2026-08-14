package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher watches for incoming files and queues them.
//
// A file is not processed the moment an event arrives: its size keeps changing
// while it is being downloaded or copied. It counts as "ready" once the size
// holds steady for StableSeconds.
type Watcher struct {
	cfg   *Config
	fsw   *fsnotify.Watcher
	queue chan string

	mu      sync.Mutex
	pending map[string]pendingFile // files waiting to settle
	queued  map[string]bool        // queued or already processed
}

type pendingFile struct {
	size     int64
	lastSeen time.Time
	stableAt time.Time
}

func NewWatcher(cfg *Config) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &Watcher{
		cfg:     cfg,
		fsw:     fsw,
		queue:   make(chan string, 256),
		pending: map[string]pendingFile{},
		queued:  map[string]bool{},
	}, nil
}

func (w *Watcher) Close() { _ = w.fsw.Close() }

// addRecursive starts watching a directory and all of its subdirectories.
func (w *Watcher) addRecursive(root string) error {
	return filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".vtrans-") {
			return filepath.SkipDir
		}
		if w.cfg.Mode == ModeCopy && w.cfg.DestRoot != "" && p == w.cfg.DestRoot {
			return filepath.SkipDir
		}
		if err := w.fsw.Add(p); err != nil {
			log.Printf("could not watch %s: %v", p, err)
		}
		return nil
	})
}

// Run starts watching. Files to process arrive on the Queue() channel.
func (w *Watcher) Run(ctx context.Context) error {
	for _, root := range w.cfg.Roots {
		if err := w.addRecursive(root); err != nil {
			return err
		}
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	var rescan <-chan time.Time
	if w.cfg.RescanMinutes > 0 {
		rt := time.NewTicker(time.Duration(w.cfg.RescanMinutes) * time.Minute)
		defer rt.Stop()
		rescan = rt.C
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case ev, ok := <-w.fsw.Events:
			if !ok {
				return nil
			}
			w.handleEvent(ev)

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return nil
			}
			log.Printf("watch error: %v", err)

		case <-ticker.C:
			w.checkStable()

		case <-rescan:
			// fsnotify can miss events; the periodic full scan is the safety net
			w.fullRescan()
		}
	}
}

func (w *Watcher) Queue() <-chan string { return w.queue }

func (w *Watcher) handleEvent(ev fsnotify.Event) {
	// start watching newly created directories too
	if ev.Has(fsnotify.Create) {
		if st, err := os.Stat(ev.Name); err == nil && st.IsDir() {
			_ = w.addRecursive(ev.Name)
			return
		}
	}
	if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
		w.mu.Lock()
		delete(w.pending, ev.Name)
		w.mu.Unlock()
		return
	}
	if !ev.Has(fsnotify.Create) && !ev.Has(fsnotify.Write) {
		return
	}
	w.consider(ev.Name)
}

func (w *Watcher) consider(path string) {
	if !videoExts[strings.ToLower(filepath.Ext(path))] || isExcluded(w.cfg, path) {
		return
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() || st.Size() == 0 {
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.queued[path] {
		return
	}
	prev, seen := w.pending[path]
	now := time.Now()
	if !seen || prev.size != st.Size() {
		// still being written: reset the timer
		w.pending[path] = pendingFile{size: st.Size(), lastSeen: now, stableAt: now}
		return
	}
	prev.lastSeen = now
	w.pending[path] = prev
}

// checkStable queues files whose size has been steady long enough.
func (w *Watcher) checkStable() {
	stable := time.Duration(w.cfg.StableSeconds) * time.Second
	now := time.Now()

	w.mu.Lock()
	var ready []string
	for p, pf := range w.pending {
		st, err := os.Stat(p)
		if err != nil {
			delete(w.pending, p)
			continue
		}
		if st.Size() != pf.size {
			w.pending[p] = pendingFile{size: st.Size(), lastSeen: now, stableAt: now}
			continue
		}
		if now.Sub(pf.stableAt) >= stable {
			ready = append(ready, p)
			delete(w.pending, p)
			w.queued[p] = true
		}
	}
	w.mu.Unlock()

	for _, p := range ready {
		select {
		case w.queue <- p:
		default:
			log.Printf("queue full, skipped: %s", p)
			w.mu.Lock()
			delete(w.queued, p)
			w.mu.Unlock()
		}
	}
}

// fullRescan walks the whole tree to catch files that were missed.
func (w *Watcher) fullRescan() {
	files, err := WalkLibrary(w.cfg)
	if err != nil {
		log.Printf("rescan error: %v", err)
		return
	}
	for _, f := range files {
		w.consider(f)
	}
}

// Forget takes a file off the "processed" list so it can be handled again if it changes.
func (w *Watcher) Forget(path string) {
	w.mu.Lock()
	delete(w.queued, path)
	w.mu.Unlock()
}
