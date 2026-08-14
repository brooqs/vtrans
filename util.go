package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	cRed = "\033[31m"
	cGrn = "\033[32m"
	cYlw = "\033[33m"
	cCyn = "\033[36m"
	cDim = "\033[2m"
	cOff = "\033[0m"
)

func info(format string, a ...any) {
	fmt.Printf("%s%s%s\n", cCyn, fmt.Sprintf(format, a...), cOff)
}

func warn(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "%s%s%s\n", cYlw, fmt.Sprintf(format, a...), cOff)
}

func human(b int64) string {
	neg := ""
	if b < 0 {
		neg = "-"
		b = -b
	}
	f := float64(b)
	switch {
	case f >= 1<<40:
		return fmt.Sprintf("%s%.2f TB", neg, f/(1<<40))
	case f >= 1<<30:
		return fmt.Sprintf("%s%.1f GB", neg, f/(1<<30))
	case f >= 1<<20:
		return fmt.Sprintf("%s%.0f MB", neg, f/(1<<20))
	default:
		return fmt.Sprintf("%s%d B", neg, b)
	}
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func kind(isTV bool) string {
	if isTV {
		return "tv"
	}
	return "movie"
}

func resLabel(w int) string {
	switch {
	case w >= 3000:
		return "4K"
	case w >= 1900:
		return "1080p"
	case w >= 1200:
		return "720p"
	default:
		return "SD"
	}
}

// progressPrinter draws a single self-updating progress line.
func progressPrinter() func(Progress) {
	last := time.Now()
	return func(p Progress) {
		if time.Since(last) < 500*time.Millisecond {
			return
		}
		last = time.Now()
		pct := 0.0
		if p.TotalSec > 0 {
			pct = p.OutTimeSec / p.TotalSec * 100
			if pct > 100 {
				pct = 100
			}
		}
		eta := ""
		if p.Speed > 0 && p.TotalSec > p.OutTimeSec {
			remain := time.Duration((p.TotalSec-p.OutTimeSec)/p.Speed) * time.Second
			eta = fmt.Sprintf("  eta %s", remain.Round(time.Second))
		}
		fmt.Printf("\r\033[K   %s%.1f%%  %.0f fps  %.1fx%s%s", cDim, pct, p.FPS, p.Speed, eta, cOff)
	}
}

// runQuiet runs a command discarding its output; returns an error if it is missing.
func runQuiet(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = nil, nil
	return cmd.Run()
}

func loadDone() ([]DoneRecord, error) {
	data, err := os.ReadFile(doneLogPath())
	if err != nil {
		return nil, err
	}
	var out []DoneRecord
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r DoneRecord
		if json.Unmarshal([]byte(line), &r) == nil {
			out = append(out, r)
		}
	}
	return out, nil
}
