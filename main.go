package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Build information, filled in at link time by GoReleaser. The defaults are
// what a plain "go build" produces, which is the honest answer for a binary
// built outside a release.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const usage = `vtrans - hardware-accelerated (VAAPI / NVENC) AV1 video archiving

  vtrans scan              Scan the library / refresh the index
  vtrans plan              Show what would happen (writes nothing)
  vtrans sample <file>     Produce sample clips to compare quality by eye
  vtrans run               Batch conversion
  vtrans watch             Watch directories, process new files automatically
  vtrans status            Progress and savings
  vtrans serve             Web interface (all interfaces, :7654)
  vtrans trash             Trash status (--empty to empty it)
  vtrans config            Show / edit the configuration
  vtrans version           Build version

Configuration: ~/.config/vtrans/config.json
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}

	cfg, err := LoadConfig()
	if err != nil {
		fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "scan":
		err = cmdScan(ctx, cfg)
	case "plan":
		err = cmdPlan(ctx, cfg)
	case "sample":
		err = cmdSample(ctx, cfg, args)
	case "run":
		err = cmdRun(ctx, cfg, args)
	case "watch":
		err = cmdWatch(ctx, cfg)
	case "status":
		err = cmdStatus()
	case "serve":
		err = cmdServe(ctx, cfg, args)
	case "trash":
		err = cmdTrash(cfg, args)
	case "config":
		err = cmdConfig(cfg, args)
	case "version", "--version", "-v":
		fmt.Printf("vtrans %s (%s, built %s)\n", version, commit, date)
		return
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Print(usage)
		os.Exit(2)
	}

	if err != nil && !errors.Is(err, context.Canceled) {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "%svtrans: %v%s\n", cRed, err, cOff)
	os.Exit(1)
}

// --- preflight checks ---

func preflight(cfg *Config) error {
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("%s not found (sudo apt install ffmpeg)", bin)
		}
	}
	hw := cfg.HW()
	if err := hw.checkDevice(); err != nil {
		return err
	}
	out, err := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
	if err != nil || !strings.Contains(string(out), hw.Encoder) {
		return fmt.Errorf("this ffmpeg has no %s encoder (backend %s)", hw.Encoder, hw.Name)
	}
	return nil
}

// --- commands ---

func cmdScan(ctx context.Context, cfg *Config) error {
	if err := preflight(cfg); err != nil {
		return err
	}
	ix := LoadIndex()
	info("Scanning: %s", strings.Join(cfg.Roots, ", "))
	err := Scan(ctx, cfg, ix, func(done, total int) {
		fmt.Printf("\r  %d/%d", done, total)
	})
	fmt.Println()
	if err != nil {
		return err
	}
	info("%d files indexed", len(ix.Entries))
	return nil
}

func cmdPlan(ctx context.Context, cfg *Config) error {
	ix := LoadIndex()
	if len(ix.Entries) == 0 {
		return fmt.Errorf("the index is empty, run 'vtrans scan' first")
	}
	cands := BuildCandidates(cfg, ix)
	if len(cands) == 0 {
		warn("Nothing to process.")
		return nil
	}

	type group struct {
		n        int
		src, est int64
	}
	groups := map[string]*group{}
	var totalSrc, totalEst int64

	for _, c := range cands {
		base := estimatedVideoMbps(cfg, c.Quality)
		px := float64(c.TargetW * c.TargetH)
		estVideo := base * (px / (1920 * 1080)) * 1e6 * c.Duration / 8
		estAudio := estimatedAudio(cfg, c) * c.Duration / 8
		est := int64(estVideo + estAudio)

		key := kind(c.IsTV) + "/" + resLabel(c.Width)
		g := groups[key]
		if g == nil {
			g = &group{}
			groups[key] = g
		}
		g.n++
		g.src += c.Size
		g.est += est
		totalSrc += c.Size
		totalEst += est
	}

	fmt.Println()
	fmt.Printf("%s%-16s %7s %12s %12s %10s%s\n", cDim, "GROUP", "COUNT", "SOURCE", "ESTIMATE", "SAVING", cOff)
	fmt.Println(strings.Repeat("-", 62))

	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		g := groups[k]
		fmt.Printf("%-16s %7d %12s %12s %10s\n", k, g.n, human(g.src), human(g.est), human(g.src-g.est))
	}
	fmt.Println(strings.Repeat("-", 62))
	fmt.Printf("%s%-16s %7d %12s %12s %10s%s\n", cGrn, "TOTAL", len(cands),
		human(totalSrc), human(totalEst), human(totalSrc-totalEst), cOff)

	fmt.Println()
	if cfg.Mode == ModeReplace {
		warn("MODE: replace - the original of every verified file will be replaced.")
		if cfg.TrashDir != "" {
			warn("Originals will be moved under %s; space is only freed after 'vtrans trash --empty'.", cfg.TrashDir)
		} else {
			warn("TrashDir is empty: originals will be DELETED OUTRIGHT. There is no way back.")
		}
	} else {
		info("MODE: copy - originals are kept, output is written under %s.", cfg.DestRoot)
	}
	fmt.Printf("%sEstimates are rough. Check them with 'vtrans sample <file>'.%s\n", cDim, cOff)
	return nil
}

// estimatedVideoMbps is the AV1 bitrate estimate for 1920x1080 live action on
// the active backend.
//
// VAAPI: an exponential model fitted to values measured on the Radeon 890M
// (District 9, 4 scenes):
//
//	q110 2.79 | q130 2.02 | q150 1.46 | q170 1.04 | q190 0.74 Mbps
//
// The ratio per 20 q steps comes out constant at ~0.718; the model matches the
// measurements to within 2%.
//
// NVENC: fitted on the RTX 4060 (Hansel & Gretel, 60 s at 30 min, 1920x800
// normalised to 1080p): the ratio per 20 q steps is ~0.70 and the curve sits
// well below VAAPI's at the same q. The reference bitrate is set so the two
// curves cross at the measured equivalence (NVENC q90 = VAAPI q130); it is
// rougher than the VAAPI figure since it rests on one film.
//
// Live action is the reference: animation compresses about twice as well, so the
// estimate makes animated files look larger - deliberately, to avoid overstating
// the savings.
func estimatedVideoMbps(cfg *Config, q int) float64 {
	refQ, refMbps, ratioPer20 := 110.0, 2.79, 0.718
	if cfg.ResolveBackend() == BackendNVENC {
		refQ, refMbps, ratioPer20 = 110.0, 1.30, 0.70
	}
	return refMbps * math.Pow(ratioPer20, (float64(q)-refQ)/20)
}

func estimatedAudio(cfg *Config, c Candidate) float64 {
	if cfg.AudioMode == "copy" {
		return float64(c.AudioBR)
	}
	if cfg.AudioMode == "none" {
		return 0
	}
	// opus: the index has no channel information, so estimate roughly assuming
	// most streams are 5.1
	if c.AudioBR <= 0 {
		return 256000
	}
	est := float64(c.AudioBR) * 0.4
	if est < 128000 {
		est = 128000
	}
	return est
}

func cmdRun(ctx context.Context, cfg *Config, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "only print what would happen")
	limit := fs.Int("limit", 0, "process at most this many files (0 = unlimited)")
	retry := fs.Bool("retry-failed", false, "retry files that failed before")
	_ = fs.Parse(args)

	if err := preflight(cfg); err != nil {
		return err
	}
	if err := checkRecovery(); err != nil {
		return err
	}
	release, err := acquireLock()
	if err != nil {
		return err
	}
	defer release()

	if n, freed := cleanStaleParts(cfg); n > 0 {
		info("deleted %d leftover partial file(s) (%s reclaimed)", n, human(freed))
	}

	ix := LoadIndex()
	if len(ix.Entries) == 0 {
		return fmt.Errorf("the index is empty, run 'vtrans scan' first")
	}
	cands := BuildCandidates(cfg, ix)
	failed := LoadFailed()
	ignored := LoadIgnored()

	info("%d candidate files | mode=%s | audio=%s", len(cands), cfg.Mode, cfg.AudioMode)
	if cfg.Mode == ModeReplace {
		if cfg.TrashDir != "" {
			info("Originals will be moved to the trash: %s", cfg.TrashDir)
		} else {
			warn("Originals will be DELETED OUTRIGHT.")
		}
	}
	fmt.Println()

	var nOK, nSkip, nFail, nListed int
	var saved int64
	start := time.Now()

	// The state record is only kept for a real run: a dry run processes nothing,
	// so there is no point in looking busy to the interface.
	var rep *RunReporter
	if !*dry {
		rep = NewRunReporter(len(cands))
		defer rep.Close()
	}

	for i, c := range cands {
		if ctx.Err() != nil {
			break
		}
		// a dry run does no encoding, so the limit counts listed files instead
		if *limit > 0 && ((*dry && nListed >= *limit) || (!*dry && nOK >= *limit)) {
			break
		}
		if failed[c.Path] && !*retry {
			nSkip++
			continue
		}
		// An ignored file is left alone even with -retry: -retry is for reopening
		// failures, while ignoring is a decision that was made deliberately.
		if ignored[c.Path] {
			nSkip++
			continue
		}

		fmt.Printf("%s[%d/%d]%s %s\n", cDim, i+1, len(cands), cOff, filepath.Base(c.Path))
		fmt.Printf("   %s%s %dx%d -> AV1 %dx%d q%d%s\n", cDim, c.Codec, c.Width, c.Height,
			c.TargetW, c.TargetH, c.Quality, cOff)

		nListed++
		if *dry {
			continue
		}

		rep.SetFile(i+1, c.Path, c.Size)
		pp := progressPrinter()
		res := ProcessOne(ctx, cfg, c, Hooks{
			Progress: func(p Progress) { pp(p); rep.Progress(p) },
			Phase:    rep.SetPhase,
		})
		fmt.Print("\r\033[K")

		switch {
		case errors.Is(res.Err, errAlreadyDone):
			nSkip++
		case errors.Is(res.Err, errStreamLoss):
			nSkip++
			fmt.Printf("   %sSKIPPED: %v%s\n", cYlw, res.Err, cOff)
			// This verdict does not change between runs: the file has streams that
			// cannot be written, and every cycle would probe it again and reach the
			// same conclusion. Recording it keeps it out of the queue count, which
			// otherwise never drops. It stays visible under Ignored and can be put
			// back by hand.
			if !*dry {
				if err := Ignore(c.Path, res.Err.Error(), true); err != nil {
					warn("could not add to the ignore list: %v", err)
				}
			}
		case res.Err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			nFail++
			fmt.Printf("   %sFAILED: %v%s\n", cRed, res.Err, cOff)
			// Transient errors are not written to the permanent record; otherwise
			// the file would never be tried again.
			if !errors.Is(res.Err, errTransient) {
				RecordFail(c.Path, res.Err)
			}
		default:
			nOK++
			d := res.Info.Size - res.OutSize
			saved += d
			verb := "written"
			if res.Replaced {
				verb = "replaced"
			}
			fmt.Printf("   %s%s -> %s (%s saved, %s) %s%s\n", cGrn,
				human(res.Info.Size), human(res.OutSize), human(d),
				res.Elapsed.Round(time.Second), verb, cOff)
			RecordDone(res)
			ix.Delete(c.Path)
		}
		rep.Tally(nOK, nSkip, nFail, saved)
	}

	_ = ix.Save()
	fmt.Println()
	info("Done. %d processed, %d skipped, %d failed, took %s",
		nOK, nSkip, nFail, time.Since(start).Round(time.Second))
	info("Total saved: %s", human(saved))
	if nFail > 0 {
		warn("Failures: %s", failedLogPath())
	}
	return nil
}

func cmdWatch(ctx context.Context, cfg *Config) error {
	if err := preflight(cfg); err != nil {
		return err
	}
	if err := checkRecovery(); err != nil {
		return err
	}
	release, err := acquireLock()
	if err != nil {
		return err
	}
	defer release()

	w, err := NewWatcher(cfg)
	if err != nil {
		return err
	}
	defer w.Close()

	info("Watching: %s", strings.Join(cfg.Roots, ", "))
	info("A new file is processed once its size holds steady for %d s. Ctrl-C to quit.", cfg.StableSeconds)
	if cfg.Mode == ModeReplace {
		warn("MODE: replace - the original of every verified file will be replaced.")
	}
	fmt.Println()

	// In watch mode the queue is not known in advance; the total counts as 1 per file.
	rep := NewRunReporter(1)
	defer rep.Close()

	errc := make(chan error, 1)
	go func() { errc <- w.Run(ctx) }()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errc:
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		case path := <-w.Queue():
			handleWatched(ctx, cfg, w, rep, path)
			rep.SetPhase("idle")
		}
	}
}

func handleWatched(ctx context.Context, cfg *Config, w *Watcher, rep *RunReporter, path string) {
	mi, err := Probe(ctx, path)
	if err != nil {
		fmt.Printf("%s? %s: could not be analysed%s\n", cDim, filepath.Base(path), cOff)
		w.Forget(path)
		return
	}
	v, ok := mi.PrimaryVideo()
	if !ok {
		w.Forget(path)
		return
	}

	st, err := os.Stat(path)
	if err != nil {
		w.Forget(path)
		return
	}
	e := Entry{Path: path, Size: st.Size(), ModTimeNS: st.ModTime().UnixNano(),
		Codec: v.CodecName, Width: v.Width, Height: v.Height,
		Duration: mi.Duration, AudioBR: mi.AudioBitrateTotal(), NumVideo: len(mi.VideoStreams())}

	if ok, reason := shouldProcess(cfg, e); !ok {
		fmt.Printf("%s- %s (%s)%s\n", cDim, filepath.Base(path), reason, cOff)
		return
	}

	tw, th := TargetDims(e.Width, e.Height, cfg.MaxWidthFor(path))
	c := Candidate{Entry: e, IsTV: cfg.IsTV(path), TargetW: tw, TargetH: th,
		Quality: cfg.QualityFor(path), DestPath: DestFor(cfg, path)}

	fmt.Printf("%s%s%s\n", cCyn, filepath.Base(path), cOff)
	fmt.Printf("   %s%s %dx%d -> AV1 %dx%d q%d%s\n", cDim, e.Codec, e.Width, e.Height, tw, th, c.Quality, cOff)

	pp := progressPrinter()
	rep.SetFile(1, path, e.Size)
	res := ProcessOne(ctx, cfg, c, Hooks{
		Progress: func(p Progress) { pp(p); rep.Progress(p) },
		Phase:    rep.SetPhase,
	})
	fmt.Print("\r\033[K")

	if res.Err != nil {
		if ctx.Err() != nil {
			return
		}
		fmt.Printf("   %sFAILED: %v%s\n", cRed, res.Err, cOff)
		if !errors.Is(res.Err, errTransient) {
			RecordFail(path, res.Err)
		}
		return
	}
	fmt.Printf("   %s%s -> %s (%s saved, %s)%s\n", cGrn,
		human(res.Info.Size), human(res.OutSize), human(res.Info.Size-res.OutSize),
		res.Elapsed.Round(time.Second), cOff)
	RecordDone(res)
}

func checkRecovery() error {
	op, err := RecoverJournal()
	if err != nil || op == nil {
		return nil
	}
	warn("A previous run was left half-finished:")
	warn("  source    : %s", op.Src)
	warn("  temporary : %s", op.Tmp)
	if op.Trashed != "" {
		warn("  the original may have been moved to the trash: %s", op.Trashed)
	}
	return fmt.Errorf("check by hand first, then delete %s", journalPath())
}

func cmdStatus() error {
	recs, err := loadDone()
	if err != nil || len(recs) == 0 {
		warn("No files completed yet.")
		return nil
	}
	var src, dst int64
	var replaced int
	for _, r := range recs {
		src += r.SrcSize
		dst += r.DestSize
		if r.Replaced {
			replaced++
		}
	}
	fmt.Printf("  completed : %d files (%d replaced the original)\n", len(recs), replaced)
	fmt.Printf("  source    : %s\n", human(src))
	fmt.Printf("  output    : %s\n", human(dst))
	if src > 0 {
		fmt.Printf("  saved     : %s (%.0f%%)\n", human(src-dst), float64(src-dst)*100/float64(src))
	}
	if f := LoadFailed(); len(f) > 0 {
		fmt.Printf("  failed    : %d files (%s)\n", len(f), failedLogPath())
	}
	return nil
}

func cmdTrash(cfg *Config, args []string) error {
	fs := flag.NewFlagSet("trash", flag.ExitOnError)
	empty := fs.Bool("empty", false, "delete the trash directory permanently")
	_ = fs.Parse(args)

	if cfg.TrashDir == "" {
		warn("TrashDir is not configured (originals are deleted outright).")
		return nil
	}
	var total int64
	var n int
	_ = filepath.WalkDir(cfg.TrashDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if st, err := d.Info(); err == nil {
			total += st.Size()
			n++
		}
		return nil
	})

	fmt.Printf("  trash: %s\n  %d files, %s\n", cfg.TrashDir, n, human(total))
	if !*empty {
		if n > 0 {
			fmt.Printf("\n  To delete permanently: vtrans trash --empty\n")
		}
		return nil
	}
	if n == 0 {
		return nil
	}
	if err := os.RemoveAll(cfg.TrashDir); err != nil {
		return err
	}
	info("Trash emptied, %s reclaimed.", human(total))
	return nil
}

func cmdConfig(cfg *Config, args []string) error {
	if len(args) > 0 && args[0] == "path" {
		fmt.Println(configPath())
		return nil
	}
	fmt.Printf("%s%s%s\n\n", cDim, configPath(), cOff)
	data, _ := os.ReadFile(configPath())
	if len(data) == 0 {
		_ = SaveConfig(cfg)
		data, _ = os.ReadFile(configPath())
	}
	fmt.Print(string(data))
	return nil
}
