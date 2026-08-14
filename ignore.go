package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A file can sit in the queue forever without ever being processed: either a
// permanent failure was recorded for it, or every run decides afresh to skip it.
// Counting those in the queue makes the number look stuck - it never drops,
// however long the service runs. The ignore list is the persistent answer:
// whatever lands here is out of the queue and is not probed again.
//
// Two things end up on it. "auto" records are written by the run itself when it
// meets a file it will refuse every time (stream loss). Manual records come from
// the interface, when the reader decides a file should be left alone.

func ignoredLogPath() string { return filepath.Join(stateDir(), "ignored.jsonl") }

type IgnoreRecord struct {
	Src    string `json:"src"`
	Reason string `json:"reason"`
	// Auto marks a record vtrans wrote itself, so the interface can tell it
	// apart from a decision the reader made by hand.
	Auto bool      `json:"auto"`
	At   time.Time `json:"at"`
}

// Ignore adds a file to the list. Adding one that is already there refreshes the
// reason rather than writing a second record.
func Ignore(src, reason string, auto bool) error {
	recs := loadIgnoredRecords()
	for i, r := range recs {
		if r.Src == src {
			recs[i].Reason = reason
			recs[i].Auto = auto
			recs[i].At = time.Now()
			return rewriteIgnored(recs)
		}
	}
	// Rewritten rather than appended: the list is short, and this is a button in
	// the interface, so a failed write has to be reported rather than swallowed
	// the way appendJSONL does.
	recs = append(recs, IgnoreRecord{
		Src: src, Reason: reason, Auto: auto, At: time.Now(),
	})
	return rewriteIgnored(recs)
}

// Unignore takes a file off the list and reports whether it was on it.
func Unignore(src string) (bool, error) {
	recs := loadIgnoredRecords()
	var keep []IgnoreRecord
	for _, r := range recs {
		if r.Src != src {
			keep = append(keep, r)
		}
	}
	if len(keep) == len(recs) {
		return false, nil
	}
	return true, rewriteIgnored(keep)
}

func loadIgnoredRecords() []IgnoreRecord {
	data, err := os.ReadFile(ignoredLogPath())
	if err != nil {
		return nil
	}
	var out []IgnoreRecord
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r IgnoreRecord
		if json.Unmarshal([]byte(line), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

// LoadIgnored answers "should this file be left alone", which is all the run
// loop needs.
func LoadIgnored() map[string]bool {
	out := map[string]bool{}
	for _, r := range loadIgnoredRecords() {
		out[r.Src] = true
	}
	return out
}

// rewriteIgnored replaces the file atomically: a half-written list would hide
// files from the queue at random.
func rewriteIgnored(recs []IgnoreRecord) error {
	if len(recs) == 0 {
		err := os.Remove(ignoredLogPath())
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var b strings.Builder
	for _, r := range recs {
		data, err := json.Marshal(r)
		if err != nil {
			return err
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	tmp := ignoredLogPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, ignoredLogPath())
}
