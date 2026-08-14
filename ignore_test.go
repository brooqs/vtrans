package main

import (
	"os"
	"testing"
)

// The ignore list lives next to the other state, so the tests point stateDir at
// a temporary directory through the same environment variable the binary reads.
func withTempState(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("VTRANS_STATE", dir)
	if stateDir() != dir {
		t.Fatalf("stateDir() = %q, want %q", stateDir(), dir)
	}
}

func TestIgnoreRoundTrip(t *testing.T) {
	withTempState(t)

	if len(LoadIgnored()) != 0 {
		t.Fatal("a fresh list must be empty")
	}

	if err := Ignore("/a/one.mkv", "streams cannot be written", true); err != nil {
		t.Fatalf("Ignore: %v", err)
	}
	if err := Ignore("/a/two.mkv", "ignored by hand", false); err != nil {
		t.Fatalf("Ignore: %v", err)
	}

	ign := LoadIgnored()
	if !ign["/a/one.mkv"] || !ign["/a/two.mkv"] || len(ign) != 2 {
		t.Fatalf("LoadIgnored() = %v", ign)
	}

	recs := loadIgnoredRecords()
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	if !recs[0].Auto || recs[1].Auto {
		t.Errorf("the auto flag did not survive: %+v", recs)
	}

	// Ignoring the same file again must update it, not add a duplicate -
	// otherwise a stream-loss file would gain a record on every single run.
	if err := Ignore("/a/one.mkv", "a different reason", false); err != nil {
		t.Fatalf("re-Ignore: %v", err)
	}
	recs = loadIgnoredRecords()
	if len(recs) != 2 {
		t.Fatalf("re-ignoring duplicated the record: %d", len(recs))
	}
	for _, r := range recs {
		if r.Src == "/a/one.mkv" {
			if r.Reason != "a different reason" || r.Auto {
				t.Errorf("the record was not updated: %+v", r)
			}
		}
	}
}

func TestUnignore(t *testing.T) {
	withTempState(t)

	if err := Ignore("/a/one.mkv", "x", false); err != nil {
		t.Fatal(err)
	}

	found, err := Unignore("/a/nothing.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Error("a file that was never on the list must report not-found")
	}

	found, err = Unignore("/a/one.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Error("removing a listed file must report found")
	}
	if len(LoadIgnored()) != 0 {
		t.Error("the list should be empty now")
	}

	// The last record going away removes the file rather than leaving an empty
	// one behind.
	if _, err := os.Stat(ignoredLogPath()); !os.IsNotExist(err) {
		t.Errorf("the empty list file was left behind: %v", err)
	}
}
