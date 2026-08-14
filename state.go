package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// RunState is the externally observable snapshot of a working vtrans process.
//
// Why a file and not a socket: the interface (vtrans serve) is a separate
// process and the working side usually runs under systemd. A shared file is
// the least moving parts - no port to listen on, no connection handling. If
// the process crashes a stale record is left behind, and the Updated stamp
// gives that away.
type RunState struct {
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
	Updated time.Time `json:"updated"`

	// scan | encode | verify | commit | idle
	Phase   string `json:"phase"`
	Current string `json:"current"` // full path of the file being processed
	Index   int    `json:"index"`   // position in the queue (1-based)
	Total   int    `json:"total"`

	Pct     float64 `json:"pct"`
	FPS     float64 `json:"fps"`
	Speed   float64 `json:"speed"`
	ETASec  float64 `json:"eta_sec"`
	SrcSize int64   `json:"src_size"`

	// Counters for this session
	OK    int   `json:"ok"`
	Skip  int   `json:"skip"`
	Fail  int   `json:"fail"`
	Saved int64 `json:"saved"`
}

func runStatePath() string { return filepath.Join(stateDir(), "run.json") }

// staleAfter is the age past which a record counts as "not alive". ffmpeg emits
// progress lines several times a second, so Updated is always fresh in a healthy
// run; a generous margin would leave a record from a SIGKILLed process looking
// alive for a long time.
const staleAfter = 30 * time.Second

// Alive reports whether the record really belongs to a running process.
//
// Two tests are used together: stamp freshness and the pid being alive. The pid
// alone is not enough - numbers wrap around and may land on another process.
// The stamp alone is not enough either: a SIGKILLed process cannot delete the
// file, but its pid disappears immediately.
func (s *RunState) Alive() bool {
	if s == nil || time.Since(s.Updated) > staleAfter {
		return false
	}
	if s.PID <= 0 {
		return false
	}
	// Signal 0 does not affect the process, it only tests for its existence.
	return syscall.Kill(s.PID, 0) == nil
}

// LoadRunState reads the last written state. Returns nil when the file is
// absent (never ran, or shut down cleanly).
func LoadRunState() *RunState {
	data, err := os.ReadFile(runStatePath())
	if err != nil {
		return nil
	}
	var s RunState
	if json.Unmarshal(data, &s) != nil {
		return nil
	}
	return &s
}

// RunReporter holds the state record and writes it to disk.
//
// Writes are throttled: ffmpeg emits several progress lines per second and
// flushing each one is wasteful. Meaningful transitions such as a phase change
// are written immediately, so the interface never misses short steps like
// "verifying".
type RunReporter struct {
	mu    sync.Mutex
	st    RunState
	last  time.Time
	every time.Duration
}

func NewRunReporter(total int) *RunReporter {
	now := time.Now()
	return &RunReporter{
		st:    RunState{PID: os.Getpid(), Started: now, Total: total, Phase: "idle"},
		every: time.Second,
	}
}

// SetFile announces the file that is next to be processed.
func (r *RunReporter) SetFile(index int, path string, size int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.st.Index, r.st.Current, r.st.SrcSize = index, path, size
	r.st.Pct, r.st.FPS, r.st.Speed, r.st.ETASec = 0, 0, 0, 0
	r.mu.Unlock()
	r.flush(true)
}

func (r *RunReporter) SetPhase(phase string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.st.Phase = phase
	r.mu.Unlock()
	r.flush(true)
}

// SetTotal updates the queue size once it becomes known.
func (r *RunReporter) SetTotal(total int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.st.Total = total
	r.mu.Unlock()
	r.flush(true)
}

func (r *RunReporter) Progress(p Progress) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.st.Phase = "encode"
	r.st.FPS, r.st.Speed = p.FPS, p.Speed
	if p.TotalSec > 0 {
		pct := p.OutTimeSec / p.TotalSec * 100
		if pct > 100 {
			pct = 100
		}
		r.st.Pct = pct
	}
	if p.Speed > 0 && p.TotalSec > p.OutTimeSec {
		r.st.ETASec = (p.TotalSec - p.OutTimeSec) / p.Speed
	} else {
		r.st.ETASec = 0
	}
	r.mu.Unlock()
	r.flush(false)
}

// Tally updates the session counters when a file finishes.
func (r *RunReporter) Tally(ok, skip, fail int, saved int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.st.OK, r.st.Skip, r.st.Fail, r.st.Saved = ok, skip, fail, saved
	r.mu.Unlock()
	r.flush(true)
}

// Close removes the record. A cleanly finished run leaves no state file behind,
// and the interface reads that as "not running".
func (r *RunReporter) Close() {
	if r == nil {
		return
	}
	_ = os.Remove(runStatePath())
}

func (r *RunReporter) flush(force bool) {
	r.mu.Lock()
	if !force && time.Since(r.last) < r.every {
		r.mu.Unlock()
		return
	}
	r.last = time.Now()
	r.st.Updated = r.last
	data, err := json.Marshal(r.st)
	r.mu.Unlock()
	if err != nil {
		return
	}

	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return
	}
	// Atomic write: the interface must never see a half-written file.
	tmp := runStatePath() + ".tmp"
	if os.WriteFile(tmp, data, 0o644) != nil {
		return
	}
	_ = os.Rename(tmp, runStatePath())
}
