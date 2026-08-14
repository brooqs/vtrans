package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The doctor answers "what is wrong with this file", which until now meant
// running ffprobe by hand and reading the stream list. Every rule here comes
// from a case that actually came up: subtitles ffmpeg cannot read, subtitles
// Matroska cannot store, codecs the hardware decoder refuses.
//
// It only reads. Nothing here changes a file or the queue.

// Severity says how much attention a finding deserves.
const (
	SevError = "error" // the file cannot be processed at all
	SevWarn  = "warn"  // it can, but something would be lost or fall back
	SevInfo  = "info"  // worth knowing, no action needed
)

type Finding struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	// Detail is written for a reader, not for a log: it says what was found and
	// what follows from it.
	Detail  string `json:"detail"`
	Streams []int  `json:"streams,omitempty"`
}

type FileReport struct {
	Path     string    `json:"path"`
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Duration float64   `json:"duration"`
	Streams  []StreamY `json:"streams"`
	Findings []Finding `json:"findings"`
	// Err is set when the file could not be probed at all.
	Err string    `json:"err,omitempty"`
	At  time.Time `json:"at"`
}

// StreamY is the stream summary the interface shows. MediaInfo carries more
// than a reader needs, and its shape is an ffprobe detail.
type StreamY struct {
	Index    int    `json:"index"`
	Type     string `json:"type"`
	Codec    string `json:"codec"`
	Detail   string `json:"detail,omitempty"`
	Language string `json:"language,omitempty"`
	Title    string `json:"title,omitempty"`
}

// Worst returns the most serious severity in the report.
func (r *FileReport) Worst() string {
	if r.Err != "" {
		return SevError
	}
	worst := ""
	for _, f := range r.Findings {
		switch f.Severity {
		case SevError:
			return SevError
		case SevWarn:
			worst = SevWarn
		case SevInfo:
			if worst == "" {
				worst = SevInfo
			}
		}
	}
	return worst
}

// Diagnose probes one file and reports what stands in the way of processing it.
func Diagnose(ctx context.Context, cfg *Config, path string, size int64) *FileReport {
	rep := &FileReport{Path: path, Name: filepath.Base(path), Size: size, At: time.Now()}

	mi, err := Probe(ctx, path)
	if err != nil {
		rep.Err = err.Error()
		return rep
	}
	rep.Duration = mi.Duration

	for _, s := range mi.Streams {
		y := StreamY{
			Index: s.Index, Type: s.CodecType, Codec: s.CodecName,
			Language: s.Tag("language"), Title: s.Tag("title"),
		}
		if y.Codec == "" {
			y.Codec = "unknown"
		}
		switch s.CodecType {
		case "video":
			if s.Width > 0 {
				y.Detail = fmt.Sprintf("%dx%d", s.Width, s.Height)
			}
			if mi.IsCover(s) {
				y.Detail = strings.TrimSpace(y.Detail + " cover")
			}
		case "audio":
			if s.Channels > 0 {
				y.Detail = fmt.Sprintf("%d ch", s.Channels)
			}
		}
		rep.Streams = append(rep.Streams, y)
	}

	rep.Findings = diagnoseFindings(cfg, mi)
	return rep
}

// diagnoseFindings holds the rules. Kept apart from Diagnose so it can be
// tested against a MediaInfo without running ffprobe.
func diagnoseFindings(cfg *Config, mi *MediaInfo) []Finding {
	var out []Finding

	if _, ok := mi.PrimaryVideo(); !ok {
		out = append(out, Finding{
			Kind: "no_video", Severity: SevError,
			Detail: "No video stream: there is nothing here to encode.",
		})
	}
	if mi.Duration <= 0 {
		out = append(out, Finding{
			Kind: "no_duration", Severity: SevError,
			Detail: "The duration is unknown, so neither progress nor verification can be measured. " +
				"Usually a broken or truncated file.",
		})
	}

	// Subtitles ffmpeg cannot even read. This is the Ghosts case: WebVTT inside
	// Matroska, which the demuxer only maps for WebM. Processing would drop the
	// streams without a word, so vtrans refuses the file.
	if bad := mi.UnmappableStreams(); len(bad) > 0 {
		idx := streamIndexes(bad)
		verb := "is"
		if len(bad) > 1 {
			verb = "are"
		}
		d := fmt.Sprintf("%d subtitle stream(s) %s unreadable by ffmpeg (reported as codec \"unknown\"). "+
			"They cannot be copied or converted, so encoding would lose them.", len(bad), verb)
		if cfg != nil && cfg.SkipOnStreamLoss {
			d += " The file is skipped for this reason."
		} else {
			d += " \"Skip files at risk of stream loss\" is off, so they WOULD be lost."
		}
		out = append(out, Finding{Kind: "subtitle_unreadable", Severity: SevWarn, Detail: d, Streams: idx})
	}

	// Subtitles Matroska cannot store as they are. These are converted rather
	// than lost - worth reporting, because the output carries a different
	// subtitle format from the source.
	var conv []Stream
	for _, s := range mi.MappableSubs() {
		if SubCodecFor(s) != "copy" {
			conv = append(conv, s)
		}
	}
	if len(conv) > 0 {
		names := map[string]bool{}
		for _, s := range conv {
			names[s.CodecName] = true
		}
		out = append(out, Finding{
			Kind: "subtitle_converted", Severity: SevInfo,
			Detail: fmt.Sprintf("%d subtitle stream(s) in %s, which Matroska cannot store: "+
				"they are converted to SubRip. Nothing is lost, but the format changes.",
				len(conv), strings.Join(sortedKeys(names), ", ")),
			Streams: streamIndexes(conv),
		})
	}

	// Codecs the hardware decoder refuses. mpeg4 came up on 2026-08-12: vaapi
	// initialisation fails and the file cannot be encoded at all.
	if v, ok := mi.PrimaryVideo(); ok {
		// Only reported once vtrans has actually met the limitation. Guessing
		// from the codec name would be guessing about someone else's hardware.
		if NeedsSoftwareDecode(mi) {
			out = append(out, Finding{
				Kind: "codec_no_hwdec", Severity: SevInfo,
				Detail: fmt.Sprintf("The video is %s, which this machine's GPU turned out to have no "+
					"decoder for. It is decoded on the CPU and uploaded for encoding, so it works, "+
					"but it uses more processor time than usual.", v.CodecName),
				Streams: []int{v.Index},
			})
		}
		switch v.CodecName {
		case "av1":
			out = append(out, Finding{
				Kind: "already_av1", Severity: SevInfo,
				Detail: "The video is already AV1, so there is nothing to gain by re-encoding.",
			})
		}
	}

	if pics := mi.AttachedPics(); len(pics) > 0 {
		out = append(out, Finding{
			Kind: "cover_art", Severity: SevInfo,
			Detail: fmt.Sprintf("%d cover image(s) present. They are copied through, but Matroska cannot "+
				"carry the attached_pic flag, so it does not survive.", len(pics)),
			Streams: streamIndexes(pics),
		})
	}

	if n := len(mi.VideoStreams()); n > 1 {
		out = append(out, Finding{
			Kind: "multi_video", Severity: SevInfo,
			Detail: fmt.Sprintf("%d video streams (not counting covers). Each is encoded separately, "+
				"so this takes longer than usual.", n),
		})
	}

	return out
}

func streamIndexes(ss []Stream) []int {
	out := make([]int, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.Index)
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- library scan ---

// ScanState is the progress of a library sweep. Probing 1500+ files over NFS
// takes minutes, so it runs in the background and the interface polls this.
type ScanState struct {
	Running bool      `json:"running"`
	Done    int       `json:"done"`
	Total   int       `json:"total"`
	Current string    `json:"current"`
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended,omitempty"`
	// Reports holds only the files with something to report; a clean library
	// would otherwise send 1500 empty records to the browser on every poll.
	Reports []*FileReport `json:"reports"`
	Clean   int           `json:"clean"`
	Err     string        `json:"err,omitempty"`
}

type scanner struct {
	mu     sync.Mutex
	st     ScanState
	cancel context.CancelFunc
}

// snapshot copies the state for a reader. The slice is shared rather than
// copied: entries are appended and never modified after the fact.
func (sc *scanner) snapshot() ScanState {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	out := sc.st
	out.Reports = append([]*FileReport(nil), sc.st.Reports...)
	return out
}

func (sc *scanner) stop() {
	sc.mu.Lock()
	cancel := sc.cancel
	sc.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// start begins a sweep. It reports false if one is already running.
//
// The concurrency is deliberately small: the library is on NFS and the encoder
// is usually working at the same time, so a wide fan-out of ffprobe calls would
// take bandwidth away from the job that matters.
const scanWorkers = 4

// scanTarget is one file to examine. The sweep covers the whole index rather
// than the queue: files already processed and files the rules skip can carry
// problems too, and those are exactly the ones nothing else would ever report.
type scanTarget struct {
	Path string
	Size int64
}

func (sc *scanner) start(cfg *Config, files []scanTarget) bool {
	sc.mu.Lock()
	if sc.st.Running {
		sc.mu.Unlock()
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	sc.cancel = cancel
	sc.st = ScanState{
		Running: true, Total: len(files), Started: time.Now(),
	}
	sc.mu.Unlock()

	go func() {
		defer cancel()

		jobs := make(chan scanTarget)
		var wg sync.WaitGroup
		for i := 0; i < scanWorkers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for c := range jobs {
					rep := Diagnose(ctx, cfg, c.Path, c.Size)

					// Only warnings and errors are collected. Info findings are
					// true but expected - a measured 1210 of 1572 files are
					// already AV1 and 409 carry cover art, and listing those
					// buries the eight that actually need attention. They are
					// still shown when a single file is diagnosed, where the full
					// picture is the point.
					keep := rep.Err != "" || rep.Worst() == SevError || rep.Worst() == SevWarn

					sc.mu.Lock()
					sc.st.Done++
					sc.st.Current = rep.Name
					if keep {
						sc.st.Reports = append(sc.st.Reports, rep)
					} else {
						sc.st.Clean++
					}
					sc.mu.Unlock()
				}
			}()
		}

		for _, c := range files {
			select {
			case <-ctx.Done():
			case jobs <- c:
				continue
			}
			break
		}
		close(jobs)
		wg.Wait()

		sc.mu.Lock()
		sc.st.Running = false
		sc.st.Ended = time.Now()
		sc.st.Current = ""
		if ctx.Err() != nil && sc.st.Done < sc.st.Total {
			sc.st.Err = "the scan was stopped"
		}
		// Most serious first, so the interesting rows are at the top.
		sort.SliceStable(sc.st.Reports, func(i, j int) bool {
			return sevRank(sc.st.Reports[i].Worst()) < sevRank(sc.st.Reports[j].Worst())
		})
		sc.mu.Unlock()
	}()

	return true
}

func sevRank(s string) int {
	switch s {
	case SevError:
		return 0
	case SevWarn:
		return 1
	default:
		return 2
	}
}
