package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

var videoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".m4v": true,
	".ts": true, ".mov": true, ".wmv": true, ".mpg": true, ".mpeg": true,
}

// Entry is a single file record in the index.
type Entry struct {
	Path      string  `json:"path"`
	Size      int64   `json:"size"`
	ModTimeNS int64   `json:"mtime"`
	Codec     string  `json:"codec"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	Duration  float64 `json:"duration"`
	AudioBR   int64   `json:"audio_br"`
	NumVideo  int     `json:"num_video"`
}

type Index struct {
	Entries map[string]Entry `json:"entries"`
	mu      sync.Mutex
}

func indexPath() string { return filepath.Join(stateDir(), "index.json") }

func LoadIndex() *Index {
	idx := &Index{Entries: map[string]Entry{}}
	data, err := os.ReadFile(indexPath())
	if err != nil {
		return idx
	}
	_ = json.Unmarshal(data, idx)
	if idx.Entries == nil {
		idx.Entries = map[string]Entry{}
	}
	return idx
}

func (ix *Index) Save() error {
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return err
	}
	ix.mu.Lock()
	data, err := json.MarshalIndent(ix, "", "  ")
	ix.mu.Unlock()
	if err != nil {
		return err
	}
	tmp := indexPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, indexPath())
}

func (ix *Index) Get(p string) (Entry, bool) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	e, ok := ix.Entries[p]
	return e, ok
}

func (ix *Index) Put(e Entry) {
	ix.mu.Lock()
	ix.Entries[e.Path] = e
	ix.mu.Unlock()
}

func (ix *Index) Delete(p string) {
	ix.mu.Lock()
	delete(ix.Entries, p)
	ix.mu.Unlock()
}

// isExcluded decides which paths stay out of the scan.
func isExcluded(cfg *Config, path string) bool {
	if strings.Contains(path, "/.vtrans-trash/") || strings.HasPrefix(filepath.Base(path), ".vtrans-") {
		return true
	}
	if cfg.Mode == ModeCopy && cfg.DestRoot != "" && strings.HasPrefix(path, cfg.DestRoot+string(os.PathSeparator)) {
		return true
	}
	return false
}

// WalkLibrary lists every video file under the roots.
func WalkLibrary(cfg *Config) ([]string, error) {
	var files []string
	seen := map[string]bool{}

	for _, root := range cfg.Roots {
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // skip what cannot be read, do not abort the scan
			}
			if d.IsDir() {
				if strings.HasPrefix(d.Name(), ".vtrans-") {
					return filepath.SkipDir
				}
				if cfg.Mode == ModeCopy && cfg.DestRoot != "" && p == cfg.DestRoot {
					return filepath.SkipDir
				}
				return nil
			}
			if !videoExts[strings.ToLower(filepath.Ext(p))] || isExcluded(cfg, p) {
				return nil
			}
			if !seen[p] {
				seen[p] = true
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(files)
	return files, nil
}

// Scan walks the library, reusing cached entries for unchanged files.
func Scan(ctx context.Context, cfg *Config, ix *Index, progress func(done, total int)) error {
	files, err := WalkLibrary(cfg)
	if err != nil {
		return err
	}

	// drop records for files that no longer exist
	live := map[string]bool{}
	for _, f := range files {
		live[f] = true
	}
	ix.mu.Lock()
	for p := range ix.Entries {
		if !live[p] {
			delete(ix.Entries, p)
		}
	}
	ix.mu.Unlock()

	work := make(chan string)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0

	workers := runtime.NumCPU()
	if workers > 8 {
		workers = 8
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range work {
				if e, err := probeToEntry(ctx, p, ix); err == nil {
					ix.Put(e)
				}
				mu.Lock()
				done++
				if progress != nil && done%25 == 0 {
					progress(done, len(files))
				}
				mu.Unlock()
			}
		}()
	}

	for _, f := range files {
		select {
		case <-ctx.Done():
			close(work)
			wg.Wait()
			return ctx.Err()
		case work <- f:
		}
	}
	close(work)
	wg.Wait()

	if progress != nil {
		progress(len(files), len(files))
	}
	return ix.Save()
}

// probeToEntry checks the cache and runs ffprobe only when needed.
func probeToEntry(ctx context.Context, p string, ix *Index) (Entry, error) {
	st, err := os.Stat(p)
	if err != nil {
		return Entry{}, err
	}
	if e, ok := ix.Get(p); ok && e.Size == st.Size() && e.ModTimeNS == st.ModTime().UnixNano() {
		return e, nil // unchanged, no need to analyse again
	}

	mi, err := Probe(ctx, p)
	if err != nil {
		return Entry{}, err
	}
	v, ok := mi.PrimaryVideo()
	if !ok {
		return Entry{}, fmt.Errorf("no video stream")
	}
	return Entry{
		Path:      p,
		Size:      st.Size(),
		ModTimeNS: st.ModTime().UnixNano(),
		Codec:     v.CodecName,
		Width:     v.Width,
		Height:    v.Height,
		Duration:  mi.Duration,
		AudioBR:   mi.AudioBitrateTotal(),
		NumVideo:  len(mi.VideoStreams()),
	}, nil
}

// Candidate is a file queued for processing.
type Candidate struct {
	Entry
	IsTV     bool
	TargetW  int
	TargetH  int
	Quality  int
	DestPath string
}

// shouldProcess decides whether a file gets processed; returns the skip reason.
func shouldProcess(cfg *Config, e Entry) (bool, string) {
	if e.Duration <= 0 {
		return false, "unknown duration"
	}
	for _, c := range cfg.SkipCodecs {
		if strings.EqualFold(e.Codec, c) {
			return false, "already " + e.Codec
		}
	}
	mbps := float64(e.Size) * 8 / e.Duration / 1e6
	if mbps < cfg.MinBitrateMbps {
		return false, fmt.Sprintf("bitrate too low (%.1f Mbps)", mbps)
	}
	return true, ""
}

// DestFor determines the final output path.
func DestFor(cfg *Config, src string) string {
	base := strings.TrimSuffix(src, filepath.Ext(src)) + ".mkv"
	if cfg.Mode == ModeReplace {
		return base // next to the original, same name (with .mkv extension)
	}
	// copy mode: mirror the path relative to the root under DestRoot
	for _, root := range cfg.Roots {
		if strings.HasPrefix(src, root+string(os.PathSeparator)) {
			rel, err := filepath.Rel(root, base)
			if err == nil {
				return filepath.Join(cfg.DestRoot, rel)
			}
		}
	}
	return filepath.Join(cfg.DestRoot, filepath.Base(base))
}

// BuildCandidates produces the list of files to process from the index.
func BuildCandidates(cfg *Config, ix *Index) []Candidate {
	var out []Candidate
	ix.mu.Lock()
	entries := make([]Entry, 0, len(ix.Entries))
	for _, e := range ix.Entries {
		entries = append(entries, e)
	}
	ix.mu.Unlock()

	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

	for _, e := range entries {
		if ok, _ := shouldProcess(cfg, e); !ok {
			continue
		}
		isTV := cfg.IsTV(e.Path)
		w, h := TargetDims(e.Width, e.Height, cfg.MaxWidthFor(e.Path))
		out = append(out, Candidate{
			Entry:    e,
			IsTV:     isTV,
			TargetW:  w,
			TargetH:  h,
			Quality:  cfg.QualityFor(e.Path),
			DestPath: DestFor(cfg, e.Path),
		})
	}
	return out
}
