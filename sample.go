package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// cmdSample produces short clips from a single file at different q values.
// The point: do not start a 2 TB job on the strength of one guessed q.
func cmdSample(ctx context.Context, cfg *Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: vtrans sample <video-file> [q1,q2,...]")
	}
	src := args[0]
	if _, err := os.Stat(src); err != nil {
		return err
	}
	if err := preflight(cfg); err != nil {
		return err
	}

	qs := []int{110, 130, 150, 170, 190}
	if len(args) > 1 {
		qs = nil
		for _, s := range strings.Split(args[1], ",") {
			if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
				qs = append(qs, n)
			}
		}
	}

	mi, err := Probe(ctx, src)
	if err != nil {
		return err
	}
	v, ok := mi.PrimaryVideo()
	if !ok {
		return fmt.Errorf("no video stream")
	}
	tw, th := TargetDims(v.Width, v.Height, cfg.MaxWidthFor(src))

	outDir := filepath.Join(stateDir(), "samples",
		strings.TrimSuffix(filepath.Base(src), filepath.Ext(src)))
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	info("Source : %dx%d, %.0fs, %s (%.1f Mbps)", v.Width, v.Height, mi.Duration,
		kind(cfg.IsTV(src)), mi.BitrateMbps())
	info("Target : %dx%d", tw, th)
	info("Output : %s", outDir)
	fmt.Println()

	// 30 seconds from 25%/50%/75% of the film - catches dark and high-motion scenes too
	const segLen = 30.0
	offsets := []float64{mi.Duration * 0.25, mi.Duration * 0.50, mi.Duration * 0.75}
	totalSec := segLen * float64(len(offsets))

	refPath := filepath.Join(outDir, "00-SOURCE.mkv")
	if err := buildSample(ctx, cfg, src, refPath, offsets, segLen, tw, th, -1); err != nil {
		return fmt.Errorf("could not produce the reference: %w", err)
	}
	if st, err := os.Stat(refPath); err == nil {
		fmt.Printf("  %-10s %10s\n", "SOURCE", human(st.Size()))
	}

	for _, q := range qs {
		p := filepath.Join(outDir, fmt.Sprintf("q%d.mkv", q))
		if err := buildSample(ctx, cfg, src, p, offsets, segLen, tw, th, q); err != nil {
			warn("  q%d failed: %v", q, err)
			continue
		}
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		fmt.Printf("  %-10s %10s   %5.2f Mbps\n", fmt.Sprintf("q%d", q), human(st.Size()),
			float64(st.Size())*8/totalSec/1e6)
	}

	fmt.Println()
	info("Compare:")
	fmt.Printf("  mpv --no-audio %q\n", refPath)
	fmt.Printf("  mpv --no-audio %q\n", filepath.Join(outDir, fmt.Sprintf("q%d.mkv", qs[len(qs)/2])))
	fmt.Println()
	info("Write the value you like into the configuration: %s", configPath())
	return nil
}

// buildSample takes clips at the given time points and joins them into one file.
// A q below 0 produces a lossless reference (ffv1).
func buildSample(ctx context.Context, cfg *Config, src, dst string,
	offsets []float64, segLen float64, w, h, q int) error {

	tmpDir, err := os.MkdirTemp(filepath.Dir(dst), ".seg-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	var list strings.Builder
	for i, off := range offsets {
		seg := filepath.Join(tmpDir, fmt.Sprintf("%02d.mkv", i))
		var args []string
		if q < 0 {
			args = []string{
				"-hide_banner", "-loglevel", "error", "-nostdin",
				"-ss", fmt.Sprintf("%.2f", off), "-t", fmt.Sprintf("%.2f", segLen),
				"-i", src, "-map", "0:v:0",
				"-vf", fmt.Sprintf("scale=%d:%d", w, h),
				"-c:v", "ffv1", "-an", "-sn", "-y", seg,
			}
		} else {
			args = []string{
				"-hide_banner", "-loglevel", "error", "-nostdin",
				"-hwaccel", "vaapi", "-hwaccel_device", cfg.RenderDevice,
				"-hwaccel_output_format", "vaapi",
				"-ss", fmt.Sprintf("%.2f", off), "-t", fmt.Sprintf("%.2f", segLen),
				"-i", src, "-map", "0:v:0",
				"-vf", fmt.Sprintf("scale_vaapi=%d:%d", w, h),
				"-c:v", "av1_vaapi", "-rc_mode", "CQP", "-q:v", strconv.Itoa(q),
				"-an", "-sn", "-y", seg,
			}
		}
		cmd := exec.CommandContext(ctx, "ffmpeg", args...)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
		fmt.Fprintf(&list, "file '%s'\n", seg)
	}

	listPath := filepath.Join(tmpDir, "list.txt")
	if err := os.WriteFile(listPath, []byte(list.String()), 0o644); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "concat", "-safe", "0", "-i", listPath, "-c", "copy", "-y", dst)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
