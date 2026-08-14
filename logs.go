package main

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The working side runs under systemd, so its output only exists in the
// journal. Reading it back with journalctl keeps vtrans free of a log file of
// its own - one copy of the truth, rotated by the system.

// ansiRE strips the escapes the terminal output carries - colour (\x1b[36m) and
// erase-line (\x1b[K) alike. Left in, they show up as "[36m" in the browser.
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// LogLine is one journal entry.
type LogLine struct {
	At   string `json:"at"`
	Text string `json:"text"`
}

// readJournal returns the last n lines of a unit's log, oldest first.
func readJournal(ctx context.Context, unit string, n int) ([]LogLine, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "journalctl",
		"-u", unit,
		"-n", strconv.Itoa(n),
		"--no-pager",
		"-o", "short-iso",
		// Without --all, journalctl hides any message holding control characters
		// behind "[3.4K blob data]". The progress line is written with \r, so
		// every encode result ends up inside such a message - which is exactly
		// what the reader wants to see.
		"--all",
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var lines []LogLine
	for _, raw := range strings.Split(string(out), "\n") {
		if raw == "" || strings.HasPrefix(raw, "-- ") {
			continue
		}
		l := splitStamp(raw)
		l.Text = lastOverwrite(l.Text)
		if l.Text == "" {
			continue
		}
		lines = append(lines, l)
	}
	return lines, nil
}

// lastOverwrite reduces a carriage-return progress line to what a terminal
// would actually show. The encoder redraws one line about once a second with
// \r, so a single journal message can hold a hundred updates followed by the
// result. Only the final segment is meaningful; the rest is superseded.
func lastOverwrite(s string) string {
	if i := strings.LastIndex(s, "\r"); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(ansiRE.ReplaceAllString(s, ""))
}

// splitStamp separates the timestamp from the message. short-iso format is
// "2026-08-14T10:14:19+00:00 host unit[pid]: text"; the host and unit prefix is
// noise in the interface, since the reader already knows which unit they asked
// for.
func splitStamp(raw string) LogLine {
	f := strings.SplitN(raw, " ", 2)
	if len(f) != 2 {
		return LogLine{Text: raw}
	}
	stamp, rest := f[0], f[1]
	// Drop everything up to the first "]: " or ": " that follows the unit name.
	if i := strings.Index(rest, "]: "); i >= 0 {
		rest = rest[i+3:]
	} else if i := strings.Index(rest, ": "); i >= 0 {
		rest = rest[i+2:]
	}
	return LogLine{At: stamp, Text: strings.TrimSpace(rest)}
}

// ServiceState is what systemd reports about a unit.
type ServiceState struct {
	Unit   string `json:"unit"`
	Active string `json:"active"` // active | activating | inactive | failed
	Sub    string `json:"sub"`    // running | auto-restart | dead ...
	Known  bool   `json:"known"`  // false when systemd knows no such unit
}

// Waiting reports whether the unit finished its cycle and is counting down to
// the next one. This is the state that reads as "it stopped" in the interface:
// the queue drained, the process exited cleanly and systemd is sitting on
// RestartSec before starting it again.
func (s ServiceState) Waiting() bool {
	return s.Active == "activating" && s.Sub == "auto-restart"
}

// readServiceState asks systemd about a unit. An error means the answer is
// unknown, which is reported rather than guessed.
func readServiceState(ctx context.Context, unit string) ServiceState {
	st := ServiceState{Unit: unit}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "systemctl", "show", unit,
		"-p", "ActiveState", "-p", "SubState", "-p", "LoadState").Output()
	if err != nil {
		return st
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "ActiveState":
			st.Active = v
		case "SubState":
			st.Sub = v
		case "LoadState":
			st.Known = v == "loaded"
		}
	}
	return st
}
