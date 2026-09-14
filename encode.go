package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Job struct {
	Src      string
	Info     *MediaInfo
	TargetW  int
	TargetH  int
	Quality  int
	IsTV     bool
	DestPath string // final output path
	// Cover images extracted from the source, attached to the output. Filled
	// in by Encode; see cover.go for why they do not travel as streams.
	Covers []CoverFile
}

type Result struct {
	Job
	OutSize  int64
	Elapsed  time.Duration
	Replaced bool
	Err      error
}

// Progress reports encoding progress.
type Progress struct {
	OutTimeSec float64
	TotalSec   float64
	FPS        float64
	Speed      float64
}

// buildArgs produces the verified ffmpeg command line.
//
// Important behaviours:
//   - Every real video stream is kept and encoded (alternate cuts are not dropped).
//   - Cover-art streams are copied as-is.
//   - Every audio and subtitle stream is kept (dual/multi language stays intact).
//   - filter_complex outputs carry no stream tags, so title/language are rewritten by hand.
func buildArgs(cfg *Config, j Job, outPath string) []string {
	// Codecs the GPU cannot decode take the software route: ffmpeg decodes on
	// the CPU and the filter chain uploads the frames, so the encode still
	// happens in hardware. See NeedsSoftwareDecode.
	soft := NeedsSoftwareDecode(j.Info)
	hw := cfg.HW()

	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-progress", "pipe:1", "-nostats",
	}
	if soft {
		// No -hwaccel: the device is opened for the filter chain alone, so
		// hwupload and the scaler have somewhere to put the frames.
		args = append(args, hw.filterDeviceArgs()...)
	} else {
		args = append(args, hw.decodeArgs()...)
	}
	args = append(args, "-i", j.Src)

	vids := j.Info.VideoStreams()

	// scaling chains
	//
	// The primary stream uses the target the caller computed; secondary streams
	// are scaled to their own aspect ratio. Giving them all the same size would
	// squash streams with a different ratio (e.g. a vertical bonus clip).
	maxW := cfg.MaxWidthFor(j.Src)
	var chains []string
	for i, s := range vids {
		w, h := j.TargetW, j.TargetH
		if i > 0 {
			w, h = TargetDims(s.Width, s.Height, maxW)
		}
		// With hardware decode the frames are already on the GPU. With software
		// decode they are in main memory, so they are converted to a format the
		// encoder accepts and uploaded before scaling.
		pre := ""
		if soft {
			pre = hw.uploadPrefix()
		}
		chains = append(chains, fmt.Sprintf("[0:%d]%s%s[v%d]", s.Index, pre, hw.scaleFilter(w, h), i))
	}
	args = append(args, "-filter_complex", strings.Join(chains, ";"))

	for i := range vids {
		args = append(args, "-map", fmt.Sprintf("[v%d]", i))
	}
	args = append(args, "-map", "0:a?")
	// Subtitles are mapped one by one so an unwritable stream (codec_name=unknown)
	// does not take the whole job down. Verify catches whichever were dropped.
	for _, s := range j.Info.MappableSubs() {
		args = append(args, "-map", fmt.Sprintf("0:%d", s.Index))
	}
	args = append(args, "-map", "0:t?")

	// video codecs
	for i := range vids {
		args = append(args, hw.codecArgs(i, j.Quality)...)
	}
	// Cover images go in as attachments, never as mapped streams: cover.go.
	args = append(args, attachArgs(j.Info, j.Covers)...)

	// audio
	args = append(args, audioArgs(cfg, j.Info)...)

	// Subtitle codecs are chosen per stream rather than with a blanket -c:s copy:
	// see SubCodecFor. The index here is the per-type one, and subtitles are
	// mapped in MappableSubs order, so the two line up.
	for i, s := range j.Info.MappableSubs() {
		args = append(args, fmt.Sprintf("-c:s:%d", i), SubCodecFor(s))
	}
	args = append(args, "-map_metadata", "0", "-map_chapters", "0")

	// rewrite the video stream tags
	for i, s := range vids {
		if t := s.Tag("title"); t != "" {
			args = append(args, fmt.Sprintf("-metadata:s:v:%d", i), "title="+t)
		}
		if l := s.Tag("language"); l != "" {
			args = append(args, fmt.Sprintf("-metadata:s:v:%d", i), "language="+l)
		}
	}

	args = append(args, "-y", outPath)
	return args
}

// audioArgs gives each audio stream a setting matched to its channel count.
func audioArgs(cfg *Config, mi *MediaInfo) []string {
	switch cfg.AudioMode {
	case "copy", "":
		return []string{"-c:a", "copy"}
	case "none":
		return []string{"-an"}
	}

	args := []string{"-c:a", "libopus"}
	stereo := cfg.OpusStereoKbps
	if stereo <= 0 {
		stereo = 128
	}
	for i, s := range mi.StreamsOfType("audio") {
		switch {
		case s.Channels >= 6:
			args = append(args,
				fmt.Sprintf("-b:a:%d", i), fmt.Sprintf("%dk", stereo*2),
				// libopus does not take layouts above 5.1 directly; downmix to 5.1
				fmt.Sprintf("-filter:a:%d", i), "aformat=channel_layouts=5.1")
		case s.Channels >= 2:
			args = append(args, fmt.Sprintf("-b:a:%d", i), fmt.Sprintf("%dk", stereo))
		default:
			args = append(args, fmt.Sprintf("-b:a:%d", i), fmt.Sprintf("%dk", stereo/2))
		}
	}
	return args
}

// Encode encodes the file into a temporary output and returns its path.
// The temporary file stays in the same directory as the final target so the
// rename is atomic.
func Encode(ctx context.Context, cfg *Config, j Job, onProgress func(Progress)) (string, error) {
	dir := filepath.Dir(j.DestPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, ".vtrans-"+filepath.Base(j.DestPath)+".part.mkv")
	_ = os.Remove(tmp)

	if len(j.Info.AttachedPics()) > 0 {
		coverDir, err := os.MkdirTemp("", "vtrans-cover-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(coverDir)
		j.Covers = extractCovers(ctx, j.Src, j.Info, coverDir)
	}

	out, err := runEncode(ctx, cfg, j, tmp, onProgress)
	if err == nil {
		return out, nil
	}

	// Hardware decode failing is not the file's fault and not a reason to give
	// up: it means this machine's GPU has no decoder for that codec. Note it
	// down so no later file with the same codec pays for the discovery, and run
	// this one again on the software path.
	var hw hwDecodeError
	if errors.As(err, &hw) {
		codec := ""
		if v, ok := j.Info.PrimaryVideo(); ok {
			codec = v.CodecName
		}
		markSoftwareOnly(codec)
		warn("%s cannot be decoded by the GPU here; switching to software decode", codec)
		return runEncode(ctx, cfg, j, tmp, onProgress)
	}
	return "", err
}

// hwDecodeError marks the one failure that is worth retrying differently.
type hwDecodeError struct{ msg string }

func (e hwDecodeError) Error() string { return e.msg }

func runEncode(ctx context.Context, cfg *Config, j Job, tmp string, onProgress func(Progress)) (string, error) {
	_ = os.Remove(tmp)
	args := buildArgs(cfg, j, tmp)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return "", err
	}

	go parseProgress(stdout, j.Info.Duration, onProgress)

	if err := cmd.Wait(); err != nil {
		_ = os.Remove(tmp)
		// Same rule as decodeCheck: classify on the whole output, shorten only the
		// text that ends up in the message. A "Permission denied" past the 600th
		// character would otherwise be invisible and the failure would be filed as
		// permanent instead of transient.
		fullErr := strings.TrimSpace(stderr.String())
		msg := fullErr
		if len(msg) > 600 {
			msg = msg[:600] + "..."
		}
		// SIGSEGV comes from inside ffmpeg, so it says nothing about the machine.
		// Measured on 2026-08-14: ffmpeg 8.0.1 crashes while tearing down after a
		// muxer refuses the header (mov_text into matroska). The stderr it managed
		// to write before dying is the only useful part, so keep it and call the
		// failure permanent - retrying a deterministic crash just burns the queue.
		if sig, ok := killedBySignal(err); ok {
			if sig == syscall.SIGSEGV {
				return "", fmt.Errorf("ffmpeg crashed (%v): %s", sig, msg)
			}
			// Any other signal means something outside killed the process (OOM
			// killer, a stop request). That is not the file's fault, so it stays
			// transient and gets another try later.
			return "", fmt.Errorf("%w: ffmpeg was killed with %v", errTransient, sig)
		}
		// Hardware decode could not be set up. Reported separately so Encode can
		// learn from it and retry in software, rather than filing a permanent
		// failure for a file that is perfectly fine.
		if !NeedsSoftwareDecode(j.Info) && hwInitFailure(fullErr) {
			return "", hwDecodeError{msg: msg}
		}
		// Transient EACCES shows up on NFS under load.
		if strings.Contains(fullErr, "Permission denied") {
			return "", fmt.Errorf("%w: could not open the output file (permission denied): %s", errTransient, msg)
		}
		return "", fmt.Errorf("ffmpeg: %w: %s", err, msg)
	}

	st, err := os.Stat(tmp)
	if err != nil || st.Size() == 0 {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("no output was produced")
	}
	return tmp, nil
}

func parseProgress(r io.Reader, total float64, cb func(Progress)) {
	if cb == nil {
		_, _ = io.Copy(io.Discard, r)
		return
	}
	sc := bufio.NewScanner(r)
	var p Progress
	p.TotalSec = total
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		switch k {
		case "out_time_ms", "out_time_us":
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				p.OutTimeSec = float64(n) / 1e6
			}
		case "fps":
			p.FPS, _ = strconv.ParseFloat(v, 64)
		case "speed":
			p.Speed, _ = strconv.ParseFloat(strings.TrimSuffix(v, "x"), 64)
		case "progress":
			cb(p)
		}
	}
}

// Verify compares the output against the source. These are the checks that must
// pass before the original is replaced.
func Verify(ctx context.Context, cfg *Config, src *MediaInfo, outPath string) error {
	out, err := Probe(ctx, outPath)
	if err != nil {
		return fmt.Errorf("could not analyse the output: %w", err)
	}

	// 1. Duration must match the source
	if src.Duration > 0 {
		diff := src.Duration - out.Duration
		if diff < 0 {
			diff = -diff
		}
		if diff > cfg.DurationToleranceSec {
			return fmt.Errorf("duration mismatch: source %.1fs, output %.1fs", src.Duration, out.Duration)
		}
	}

	// 2. Stream counts must hold. For subtitles the expected measure is the number
	//    of writable streams: uncopyable ones are filtered out up front and
	//    reported separately.
	sc, oc := src.CountByType(), out.CountByType()
	for _, t := range []string{"video", "audio"} {
		if oc[t] < sc[t] {
			return fmt.Errorf("lost %s stream: source %d, output %d", t, sc[t], oc[t])
		}
	}
	if want := len(src.MappableSubs()); oc["subtitle"] < want {
		return fmt.Errorf("lost subtitle stream: expected %d, output %d", want, oc["subtitle"])
	}

	// 3. The video must really be AV1
	if v, ok := out.PrimaryVideo(); !ok || v.CodecName != "av1" {
		return fmt.Errorf("the output video stream is not av1")
	}

	// 4. Without a worthwhile gain there is no point replacing anything
	if src.Size > 0 {
		saving := 1 - float64(out.Size)/float64(src.Size)
		if saving < cfg.MinSavingRatio {
			return fmt.Errorf("insufficient saving: %.0f%% (threshold %.0f%%)", saving*100, cfg.MinSavingRatio*100)
		}
	}

	// 5. Decodability
	return decodeCheck(ctx, outPath, out.Duration, cfg.VerifyMode)
}

// benignDecodeMsgs are messages produced while decoding that do not invalidate
// the output. ffmpeg writes them at error level but still exits zero.
var benignDecodeMsgs = []string{
	// Produced for the first packet of each stream on multichannel tracks encoded
	// with libopus; the audio decodes in full (measured: 20 s source -> 20.00 s output).
	"Error parsing Opus packet header",

	// A muxer warning, not a decode error: seeking into the middle of a file with
	// -ss makes ffmpeg rewind to the nearest keyframe and the first packets' stamps
	// collide. The null muxer reports it, but nothing is wrong with the file.
	//
	// Measured (The Owl House S01E01, 22 min): produced only on sample mode's last
	// segment (-ss duration-15), absent from the first two, and absent from a FULL
	// decode. The output matches the source exactly: 1326.62s -> 1326.63s,
	// 1 video/1 audio/1 subtitle, all 31806 frames decoded without error. Treating
	// it as a permanent failure was needlessly eliminating 39 files in the library.
	"non monotonically increasing dts to muxer",
}

// onlyBenignDecodeMsgs reports whether every line of the stderr output is one of
// the known harmless messages. A single unrecognised line makes it false - an
// unknown warning may be a sign of corruption.
func onlyBenignDecodeMsgs(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		known := false
		for _, b := range benignDecodeMsgs {
			if strings.Contains(line, b) {
				known = true
				break
			}
		}
		if !known {
			return false
		}
	}
	return true
}

// decodeCheck actually decodes the output to confirm there are no broken frames.
// "sample" mode decodes the start/middle/end, "full" mode decodes everything.
func decodeCheck(ctx context.Context, path string, dur float64, mode string) error {
	run := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "ffmpeg", append([]string{"-v", "error", "-nostdin"}, args...)...)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		err := cmd.Run()
		// Judge the whole output and shorten only what goes into the message.
		// Truncating first cuts the last line in half, and a half line matches no
		// benign pattern - which is how 39 sound files were rejected over
		// "non monotonica..." between 2026-08-11 and 08-12, after the message was
		// already on the benign list.
		full := strings.TrimSpace(stderr.String())
		s := full
		if len(s) > 300 {
			s = s[:300] + "..."
		}
		if err != nil {
			return fmt.Errorf("decode failed: %w: %s", err, s)
		}
		// If ffmpeg exited zero the decode succeeded. Remaining stderr lines may
		// still indicate corruption, but known harmless warnings are no reason to
		// reject the output.
		if full != "" && !onlyBenignDecodeMsgs(full) {
			return fmt.Errorf("error while decoding: %s", s)
		}
		return nil
	}

	if mode == "full" || dur <= 60 {
		return run("-i", path, "-f", "null", "-")
	}

	for _, ss := range []float64{0, dur / 2, dur - 15} {
		if ss < 0 {
			ss = 0
		}
		if err := run("-ss", fmt.Sprintf("%.2f", ss), "-t", "10", "-i", path, "-f", "null", "-"); err != nil {
			return err
		}
	}
	return nil
}
