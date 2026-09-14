package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func trashFixture(t *testing.T) (*Config, string) {
	t.Helper()
	withTempState(t)
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Roots = []string{root}
	cfg.Mode = ModeReplace
	cfg.TrashDir = filepath.Join(root, ".trash")
	return cfg, root
}

func touch(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreSameNameOutput(t *testing.T) {
	cfg, root := trashFixture(t)
	orig := filepath.Join(root, "movies", "A", "A.mkv")
	trashed := trashPathFor(cfg, orig)
	touch(t, trashed, "original")
	touch(t, orig, "av1") // the encode took the original's exact name

	entries, err := ListTrash(cfg)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ListTrash: %v, %d entries", err, len(entries))
	}
	if entries[0].Original != orig || entries[0].Output != orig {
		t.Fatalf("entry = %+v", entries[0])
	}

	got, err := RestoreFromTrash(cfg, trashed)
	if err != nil || got != orig {
		t.Fatalf("restore: %v, %q", err, got)
	}
	if b, _ := os.ReadFile(orig); string(b) != "original" {
		t.Errorf("the original was not put back: %q", b)
	}
	if _, err := os.Stat(trashed); !os.IsNotExist(err) {
		t.Error("the trash copy should be gone")
	}
	// The empty movies/A skeleton must not linger in the trash.
	if _, err := os.Stat(filepath.Join(cfg.TrashDir, "movies")); !os.IsNotExist(err) {
		t.Error("empty directories were left in the trash")
	}
	// And the file must not be queued and replaced again ten minutes later.
	ignored := false
	for _, r := range loadIgnoredRecords() {
		ignored = ignored || r.Src == orig
	}
	if !ignored {
		t.Error("a restored file must be on the ignore list")
	}
}

func TestRestoreDifferentExtension(t *testing.T) {
	cfg, root := trashFixture(t)
	orig := filepath.Join(root, "movies", "B.mp4")
	out := filepath.Join(root, "movies", "B.mkv")
	touch(t, trashPathFor(cfg, orig), "original")
	touch(t, out, "av1")

	if _, err := RestoreFromTrash(cfg, trashPathFor(cfg, orig)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Error("the AV1 output must be removed on restore")
	}
	if _, err := os.Stat(orig); err != nil {
		t.Error("the mp4 original must be back")
	}
}

func TestRestoreRefusesForeignFileAndOutsidePaths(t *testing.T) {
	cfg, root := trashFixture(t)
	orig := filepath.Join(root, "movies", "C.mp4")
	touch(t, trashPathFor(cfg, orig), "original")
	touch(t, orig, "someone else's file") // same name, not our output

	if _, err := RestoreFromTrash(cfg, trashPathFor(cfg, orig)); err == nil {
		t.Error("must not overwrite a file that is not vtrans output")
	}
	if _, err := RestoreFromTrash(cfg, orig); err == nil {
		t.Error("a path outside the trash must be refused")
	}
	if err := DeleteFromTrash(cfg, orig); err == nil {
		t.Error("deleting outside the trash must be refused")
	}
	if err := DeleteFromTrash(cfg, cfg.TrashDir); err == nil {
		t.Error("the trash directory itself is not a file to delete")
	}
}

// Restore must work while the service runs: the service holds the run lock
// for the whole batch, so gating on the lock would mean never. Only the file
// the worker is on right now is off limits.
func TestTrashBusyOnlyForTheCurrentFile(t *testing.T) {
	cfg, root := trashFixture(t)
	cur := filepath.Join(root, "movies", "Now.mkv")
	other := filepath.Join(root, "movies", "Other.mkv")
	st := &RunState{PID: os.Getpid(), Updated: time.Now(), Phase: "encode", Current: cur}
	data, _ := json.Marshal(st)
	if err := os.WriteFile(runStatePath(), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, busy := trashBusy(cfg, trashPathFor(cfg, cur)); !busy {
		t.Error("the file being encoded must be reported busy")
	}
	if _, busy := trashBusy(cfg, trashPathFor(cfg, other)); busy {
		t.Error("a file the worker is not touching must not be busy")
	}
	// A stale record (worker gone) blocks nothing.
	st.Updated = time.Now().Add(-time.Hour)
	data, _ = json.Marshal(st)
	_ = os.WriteFile(runStatePath(), data, 0o644)
	if _, busy := trashBusy(cfg, trashPathFor(cfg, cur)); busy {
		t.Error("a stale run record must not block restores")
	}
}
