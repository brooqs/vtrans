package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The trash is the only way back from replace mode, so it has to be more than
// a directory that gets emptied: what is in it, where each file came from, and
// a way to put one back. Nothing here is clever; it is the bookkeeping that
// makes "replace" a decision that can be reversed one file at a time.

// TrashEntry is one replaced original waiting in the trash.
type TrashEntry struct {
	Path     string    `json:"path"`     // inside the trash
	Original string    `json:"original"` // where it lived
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	At       time.Time `json:"at"` // when it was trashed (file mtime of the move is not it; the done record is)
	// The AV1 file that took the original's place, if it is still there.
	Output     string `json:"output"`
	OutputSize int64  `json:"output_size"`
}

// originalFor reverses trashPathFor. The layout under the trash mirrors the
// path relative to a root, so with a single root the answer is exact. With
// several roots the done log settles it; failing that, the root whose
// reconstructed path has an encoded output next to it wins, then the first.
func originalFor(cfg *Config, trashPath string, done map[string]string) string {
	if src, ok := done[trashPath]; ok {
		return src
	}
	rel, err := filepath.Rel(cfg.TrashDir, trashPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	if len(cfg.Roots) == 0 {
		return ""
	}
	for _, root := range cfg.Roots {
		cand := filepath.Join(root, rel)
		if _, err := os.Stat(DestFor(cfg, cand)); err == nil {
			return cand
		}
	}
	return filepath.Join(cfg.Roots[0], rel)
}

// ListTrash walks the trash and describes every file in it.
func ListTrash(cfg *Config) ([]TrashEntry, error) {
	if cfg.TrashDir == "" {
		return nil, nil
	}
	// trash path -> source, and source -> time, from the done log
	done := map[string]string{}
	at := map[string]time.Time{}
	if recs, err := loadDone(); err == nil {
		for _, r := range recs {
			if r.Replaced {
				done[trashPathFor(cfg, r.Src)] = r.Src
				at[r.Src] = r.At
			}
		}
	}

	var out []TrashEntry
	err := filepath.WalkDir(cfg.TrashDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		st, err := d.Info()
		if err != nil {
			return nil
		}
		e := TrashEntry{Path: p, Name: d.Name(), Size: st.Size(), At: st.ModTime()}
		e.Original = originalFor(cfg, p, done)
		if t, ok := at[e.Original]; ok {
			e.At = t
		}
		if e.Original != "" {
			if ost, err := os.Stat(DestFor(cfg, e.Original)); err == nil && !ost.IsDir() {
				e.Output = DestFor(cfg, e.Original)
				e.OutputSize = ost.Size()
			}
		}
		out = append(out, e)
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out, nil
}

// insideTrash guards every path the interface hands over: only files under the
// configured trash directory may be restored or deleted through it.
func insideTrash(cfg *Config, p string) bool {
	if cfg.TrashDir == "" || p == "" {
		return false
	}
	rel, err := filepath.Rel(cfg.TrashDir, filepath.Clean(p))
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..")
}

// RestoreFromTrash puts a replaced original back where it was.
//
// The encoded output is deleted, not kept: the whole point of restoring is
// that the original is wanted instead, and when both carry the same name the
// output has to go before the original can return anyway. The file is then
// put on the ignore list, otherwise the next scan would queue it and the
// service would replace it again ten minutes later. Un-ignore re-enables that.
func RestoreFromTrash(cfg *Config, trashPath string) (original string, err error) {
	if !insideTrash(cfg, trashPath) {
		return "", fmt.Errorf("not a file inside the trash: %s", trashPath)
	}
	if _, err := os.Stat(trashPath); err != nil {
		return "", err
	}
	entries, err := ListTrash(cfg)
	if err != nil {
		return "", err
	}
	var e *TrashEntry
	for i := range entries {
		if entries[i].Path == trashPath {
			e = &entries[i]
			break
		}
	}
	if e == nil || e.Original == "" {
		return "", fmt.Errorf("could not work out where %s came from", trashPath)
	}

	// Refuse to overwrite anything that is not our own output. A file with
	// the original's exact name that is not the encode is somebody else's.
	if st, err := os.Stat(e.Original); err == nil && !st.IsDir() && e.Output != e.Original {
		return "", fmt.Errorf("%s already exists and is not vtrans output; not overwriting it", e.Original)
	}
	if e.Output != "" {
		if err := os.Remove(e.Output); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("could not remove the encoded output: %w", err)
		}
	}
	if err := moveFile(trashPath, e.Original); err != nil {
		return "", fmt.Errorf("could not move the original back: %w", err)
	}
	pruneEmptyDirs(cfg.TrashDir, filepath.Dir(trashPath))
	if err := Ignore(e.Original, "restored from the trash; un-ignore to encode it again", false); err != nil {
		warn("%s is back but could not be added to the ignore list: %v", e.Original, err)
	}
	return e.Original, nil
}

// DeleteFromTrash removes a single file from the trash for good.
func DeleteFromTrash(cfg *Config, trashPath string) error {
	if !insideTrash(cfg, trashPath) {
		return fmt.Errorf("not a file inside the trash: %s", trashPath)
	}
	if err := os.Remove(trashPath); err != nil {
		return err
	}
	pruneEmptyDirs(cfg.TrashDir, filepath.Dir(trashPath))
	return nil
}

// pruneEmptyDirs removes now-empty directories from dir up to (not including)
// stop, so the trash does not fill with the skeleton of the library.
func pruneEmptyDirs(stop, dir string) {
	for dir != stop && strings.HasPrefix(dir, stop+string(os.PathSeparator)) {
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
