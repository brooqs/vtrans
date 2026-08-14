package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// pendingOp is the record written for recovery in case of a crash while the
// original is being replaced. Step order: write journal -> move original to
// trash -> put the temporary file in its place -> delete journal.
type pendingOp struct {
	Src     string    `json:"src"`
	Tmp     string    `json:"tmp"`
	Dest    string    `json:"dest"`
	Trashed string    `json:"trashed"`
	Started time.Time `json:"started"`
}

func journalPath() string { return filepath.Join(stateDir(), "pending.json") }

// cleanStaleParts deletes leftover partial encode files.
//
// Called after the lock is taken: at that point no other vtrans is running, so
// every stray .part file is garbage. Such files are left behind when vtrans dies
// by SIGKILL (e.g. the OOM killer); deleting is safe even if an orphaned ffmpeg
// is still writing, since the file is unlinked and the space comes back when
// ffmpeg exits.
func cleanStaleParts(cfg *Config) (n int, freed int64) {
	for _, root := range cfg.Roots {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			base := filepath.Base(p)
			if !strings.HasPrefix(base, ".vtrans-") || !strings.HasSuffix(base, ".part.mkv") {
				return nil
			}
			if st, err := os.Stat(p); err == nil {
				freed += st.Size()
			}
			if os.Remove(p) == nil {
				n++
			}
			return nil
		})
	}
	return n, freed
}

func writeJournal(op *pendingOp) error {
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(op, "", "  ")
	return os.WriteFile(journalPath(), data, 0o644)
}

func clearJournal() { _ = os.Remove(journalPath()) }

// RecoverJournal reports a half-finished replacement left by a previous run.
func RecoverJournal() (*pendingOp, error) {
	data, err := os.ReadFile(journalPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var op pendingOp
	if err := json.Unmarshal(data, &op); err != nil {
		return nil, err
	}
	return &op, nil
}

// trashPathFor produces the original's destination inside the trash (the path
// structure is preserved).
func trashPathFor(cfg *Config, src string) string {
	rel := src
	for _, root := range cfg.Roots {
		if strings.HasPrefix(src, root+string(os.PathSeparator)) {
			if r, err := filepath.Rel(root, src); err == nil {
				rel = r
				break
			}
		}
	}
	return filepath.Join(cfg.TrashDir, rel)
}

// moveFile renames within a filesystem, and falls back to copy-and-delete across them.
func moveFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return os.Remove(src)
}

// Commit makes the verified temporary file permanent.
// In ModeReplace the original is moved to the trash first, then the new file
// takes its place.
func Commit(cfg *Config, j Job, tmp string) (replaced bool, err error) {
	if cfg.Mode != ModeReplace {
		if err := os.MkdirAll(filepath.Dir(j.DestPath), 0o755); err != nil {
			return false, err
		}
		return false, os.Rename(tmp, j.DestPath)
	}

	op := &pendingOp{Src: j.Src, Tmp: tmp, Dest: j.DestPath, Started: time.Now()}

	if cfg.TrashDir != "" {
		op.Trashed = trashPathFor(cfg, j.Src)
	}
	if err := writeJournal(op); err != nil {
		return false, fmt.Errorf("could not write the recovery record: %w", err)
	}
	defer clearJournal()

	// 1) give up the original
	if op.Trashed != "" {
		if err := moveFile(j.Src, op.Trashed); err != nil {
			_ = os.Remove(tmp)
			return false, fmt.Errorf("could not move the original to the trash: %w", err)
		}
	} else {
		if err := os.Remove(j.Src); err != nil {
			_ = os.Remove(tmp)
			return false, fmt.Errorf("could not delete the original: %w", err)
		}
	}

	// 2) put the new file in its place
	if err := os.Rename(tmp, j.DestPath); err != nil {
		// roll back: restore the original
		if op.Trashed != "" {
			_ = moveFile(op.Trashed, j.Src)
		}
		return false, fmt.Errorf("could not put the output in place: %w", err)
	}
	return true, nil
}

// Hooks is used to report outwards during processing.
//
// Progress and phase are kept apart because they flow at very different rates:
// progress several times a second, phase a few times per file. The verification
// phase also produces no progress at all yet can run for minutes - an interface
// watching only progress would appear frozen there.
type Hooks struct {
	Progress func(Progress)
	Phase    func(string)
}

func (h Hooks) phase(s string) {
	if h.Phase != nil {
		h.Phase(s)
	}
}

// ProcessOne handles a single file end to end.
func ProcessOne(ctx context.Context, cfg *Config, c Candidate, h Hooks) Result {
	start := time.Now()
	res := Result{Job: Job{Src: c.Path, TargetW: c.TargetW, TargetH: c.TargetH,
		Quality: c.Quality, IsTV: c.IsTV, DestPath: c.DestPath}}

	h.phase("probe")
	mi, err := Probe(ctx, c.Path)
	if err != nil {
		res.Err = err
		return res
	}
	res.Job.Info = mi

	// An uncopyable stream in replace mode would mean unacceptable silent loss.
	if bad := mi.UnmappableStreams(); len(bad) > 0 && cfg.SkipOnStreamLoss {
		res.Err = fmt.Errorf("%w: %d subtitle stream(s) cannot be copied (unrecognised codec)",
			errStreamLoss, len(bad))
		return res
	}

	// in copy mode, skip if the output already exists
	if cfg.Mode == ModeCopy {
		if st, err := os.Stat(c.DestPath); err == nil && st.Size() > 0 {
			res.Err = errAlreadyDone
			return res
		}
	}

	h.phase("encode")
	tmp, err := Encode(ctx, cfg, res.Job, h.Progress)
	if err != nil {
		res.Err = err
		return res
	}

	h.phase("verify")
	if err := Verify(ctx, cfg, mi, tmp); err != nil {
		_ = os.Remove(tmp)
		res.Err = fmt.Errorf("verification failed: %w", err)
		return res
	}

	st, _ := os.Stat(tmp)
	res.OutSize = st.Size()

	h.phase("commit")
	replaced, err := Commit(cfg, res.Job, tmp)
	if err != nil {
		res.Err = err
		return res
	}
	res.Replaced = replaced
	res.Elapsed = time.Since(start)
	return res
}

var (
	errAlreadyDone = fmt.Errorf("already processed")
	errStreamLoss  = fmt.Errorf("risk of stream loss")
	// errTransient marks failures that are about the environment rather than the
	// file, and may pass on a retry. These are not written to the permanent record.
	errTransient = fmt.Errorf("transient error")
)

// killedBySignal reports whether the process was terminated by an outside signal
// (such as the OOM killer) rather than failing on its own.
func killedBySignal(err error) (syscall.Signal, bool) {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return 0, false
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return 0, false
	}
	return ws.Signal(), true
}

// --- permanent records ---

type DoneRecord struct {
	Src      string    `json:"src"`
	Dest     string    `json:"dest"`
	SrcSize  int64     `json:"src_size"`
	DestSize int64     `json:"dest_size"`
	Replaced bool      `json:"replaced"`
	At       time.Time `json:"at"`
}

func doneLogPath() string   { return filepath.Join(stateDir(), "done.jsonl") }
func failedLogPath() string { return filepath.Join(stateDir(), "failed.jsonl") }

func appendJSONL(path string, v any) {
	_ = os.MkdirAll(stateDir(), 0o755)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	data, _ := json.Marshal(v)
	_, _ = f.Write(append(data, '\n'))
}

func RecordDone(r Result) {
	appendJSONL(doneLogPath(), DoneRecord{
		Src: r.Src, Dest: r.DestPath, SrcSize: r.Info.Size,
		DestSize: r.OutSize, Replaced: r.Replaced, At: time.Now(),
	})
}

type FailRecord struct {
	Src    string    `json:"src"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

func RecordFail(src string, err error) {
	appendJSONL(failedLogPath(), FailRecord{Src: src, Reason: err.Error(), At: time.Now()})
}

// loadFailedRecords returns the failure records as they are.
// LoadFailed only answers "was this tried"; the interface also shows the reason
// and the time, so it needs the full record.
func loadFailedRecords() []FailRecord {
	data, err := os.ReadFile(failedLogPath())
	if err != nil {
		return nil
	}
	var out []FailRecord
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r FailRecord
		if json.Unmarshal([]byte(line), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

// rewriteFailed rewrites the failure record from scratch (to take a file off the
// list). It writes to a temporary file and renames; a half-written record file
// would silently skip files on the next run.
func rewriteFailed(recs []FailRecord) error {
	if len(recs) == 0 {
		err := os.Remove(failedLogPath())
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
	tmp := failedLogPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, failedLogPath())
}

// LoadFailed returns the files that failed before (so they are not retried).
func LoadFailed() map[string]bool {
	out := map[string]bool{}
	data, err := os.ReadFile(failedLogPath())
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r FailRecord
		if json.Unmarshal([]byte(line), &r) == nil {
			out[r.Src] = true
		}
	}
	return out
}
