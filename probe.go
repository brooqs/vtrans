package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

type Stream struct {
	Index       int               `json:"index"`
	CodecType   string            `json:"codec_type"`
	CodecName   string            `json:"codec_name"`
	Width       int               `json:"width"`
	Height      int               `json:"height"`
	Channels    int               `json:"channels"`
	BitRateRaw  string            `json:"bit_rate"`
	Tags        map[string]string `json:"tags"`
	Disposition map[string]int    `json:"disposition"`
}

func (s Stream) BitRate() int64 {
	v, err := strconv.ParseInt(s.BitRateRaw, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// IsAttachedPic identifies cover-art streams (not real video streams).
func (s Stream) IsAttachedPic() bool { return s.Disposition["attached_pic"] == 1 }

func (s Stream) Tag(k string) string {
	if s.Tags == nil {
		return ""
	}
	return s.Tags[k]
}

type Format struct {
	Duration string `json:"duration"`
	Size     string `json:"size"`
	BitRate  string `json:"bit_rate"`
}

type MediaInfo struct {
	Path     string   `json:"-"`
	Streams  []Stream `json:"streams"`
	Format   Format   `json:"format"`
	Duration float64  `json:"-"`
	Size     int64    `json:"-"`

	// Indices of cover images embedded without the attached_pic flag.
	// Filled in by detectCovers.
	covers map[int]bool
}

// IsCover reports whether a stream is cover art. There are two paths: the
// attached_pic flag on properly marked streams, and detectCovers' content
// analysis for the unmarked ones.
func (m *MediaInfo) IsCover(s Stream) bool {
	return s.IsAttachedPic() || m.covers[s.Index]
}

// StreamsOfType returns the streams of a given type, in order.
func (m *MediaInfo) StreamsOfType(t string) []Stream {
	var out []Stream
	for _, s := range m.Streams {
		if s.CodecType == t {
			out = append(out, s)
		}
	}
	return out
}

// VideoStreams returns the real video streams (cover art excluded).
func (m *MediaInfo) VideoStreams() []Stream {
	var out []Stream
	for _, s := range m.Streams {
		if s.CodecType == "video" && !m.IsCover(s) {
			out = append(out, s)
		}
	}
	return out
}

// AttachedPics returns the cover images: both flagged and unflagged ones.
func (m *MediaInfo) AttachedPics() []Stream {
	var out []Stream
	for _, s := range m.Streams {
		if s.CodecType == "video" && m.IsCover(s) {
			out = append(out, s)
		}
	}
	return out
}

// PrimaryVideo returns the first real video stream.
func (m *MediaInfo) PrimaryVideo() (Stream, bool) {
	v := m.VideoStreams()
	if len(v) == 0 {
		return Stream{}, false
	}
	return v[0], true
}

// Mappable reports whether the stream can be copied into the target container.
// Subtitles with codec_name "unknown" cannot be written by ffmpeg.
func (s Stream) Mappable() bool {
	return s.CodecName != "" && s.CodecName != "unknown"
}

// UnmappableStreams returns streams that cannot be copied. Processing anyway
// would drop them silently.
func (m *MediaInfo) UnmappableStreams() []Stream {
	var out []Stream
	for _, s := range m.Streams {
		if s.CodecType == "subtitle" && !s.Mappable() {
			out = append(out, s)
		}
	}
	return out
}

// matroskaSubCodecs lists the subtitle codecs the Matroska muxer can store as
// they are. Anything outside this set has to be transcoded on the way in.
var matroskaSubCodecs = map[string]bool{
	"subrip": true, "srt": true,
	"ass": true, "ssa": true,
	"webvtt":             true,
	"hdmv_pgs_subtitle":  true,
	"dvd_subtitle":       true,
	"dvb_subtitle":       true,
	"hdmv_text_subtitle": true,
}

// SubCodecFor gives the -c:s value for a subtitle stream.
//
// Copying is preferred, but mp4-sourced files carry mov_text (tx3g), which
// Matroska cannot store. Passing -c:s copy there makes the muxer refuse the
// header, and ffmpeg 8.0.1 then segfaults on the way out - 12 files failed this
// way on 2026-08-14 before this existed. mov_text is plain text, so converting
// to SubRip keeps the subtitle instead of losing the whole file.
func SubCodecFor(s Stream) string {
	if matroskaSubCodecs[s.CodecName] {
		return "copy"
	}
	return "srt"
}

// MappableSubs returns the writable subtitle streams.
func (m *MediaInfo) MappableSubs() []Stream {
	var out []Stream
	for _, s := range m.StreamsOfType("subtitle") {
		if s.Mappable() {
			out = append(out, s)
		}
	}
	return out
}

// CountByType returns stream counts for verification.
func (m *MediaInfo) CountByType() map[string]int {
	c := map[string]int{}
	for _, s := range m.Streams {
		if s.CodecType == "video" && m.IsCover(s) {
			c["attached_pic"]++
			continue
		}
		c[s.CodecType]++
	}
	return c
}

// AudioBitrateTotal sums the audio streams' bitrate, estimating from the
// channel count where it is unknown.
func (m *MediaInfo) AudioBitrateTotal() int64 {
	var total int64
	for _, s := range m.StreamsOfType("audio") {
		if br := s.BitRate(); br > 0 {
			total += br
			continue
		}
		switch {
		case s.Channels >= 6:
			total += 640000
		case s.Channels >= 2:
			total += 192000
		default:
			total += 96000
		}
	}
	return total
}

// BitrateMbps is the file's overall bitrate.
func (m *MediaInfo) BitrateMbps() float64 {
	if m.Duration <= 0 {
		return 0
	}
	return float64(m.Size) * 8 / m.Duration / 1e6
}

// Probe analyses a file with ffprobe.
func Probe(ctx context.Context, path string) (*MediaInfo, error) {
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_format", "-show_streams",
		path,
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe %s: %w: %s", path, err, strings.TrimSpace(stderr.String()))
	}

	var mi MediaInfo
	if err := json.Unmarshal(out, &mi); err != nil {
		return nil, fmt.Errorf("could not parse ffprobe output (%s): %w", path, err)
	}
	mi.Path = path
	mi.Duration, _ = strconv.ParseFloat(mi.Format.Duration, 64)
	mi.Size, _ = strconv.ParseInt(mi.Format.Size, 10, 64)
	detectCovers(ctx, path, &mi)
	return &mi, nil
}

// coverWindowSec is the time window read for cover detection.
const coverWindowSec = 5

// detectCovers finds cover images embedded without the attached_pic flag.
//
// Some releases embed the poster/fanart as a plain video stream with no flag.
// Mistaking those for real video opens a separate hardware encoder for each;
// two VAAPI encoders' surface pools can exhaust GTT and drive the system into
// OOM. The distinguishing trait is that they are a single frame: a real stream
// produces dozens of packets in the first seconds, a cover at most one.
//
// The read is bounded by a time window. A packet-count limit (-read_intervals
// %+#N) cannot be used: hunting for a second packet that never arrives makes
// ffprobe scan the whole file (measured: over 40 seconds on a 2 GB file). The
// time window makes the same call in under 0.2 seconds.
func detectCovers(ctx context.Context, path string, mi *MediaInfo) {
	// No unflagged candidates, or only one video stream, means there is nothing
	// to tell apart: return without the extra read. The vast majority of the
	// library takes this path.
	var cand []Stream
	for _, s := range mi.Streams {
		if s.CodecType == "video" && !s.IsAttachedPic() {
			cand = append(cand, s)
		}
	}
	if len(cand) < 2 {
		return
	}

	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-select_streams", "v",
		"-read_intervals", fmt.Sprintf("%%+%d", coverWindowSec),
		"-show_entries", "packet=stream_index",
		"-of", "csv=p=0",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return // if detection fails, keep the previous behaviour
	}

	counts := map[int]int{}
	for _, line := range strings.Split(string(out), "\n") {
		if n, err := strconv.Atoi(strings.TrimSpace(line)); err == nil {
			counts[n]++
		}
	}

	// The stream producing the most packets always counts as real video, so an
	// unexpectedly empty measurement never makes the whole file look like cover art.
	best, bestN := -1, -1
	for _, s := range cand {
		if counts[s.Index] > bestN {
			best, bestN = s.Index, counts[s.Index]
		}
	}
	if bestN <= 1 {
		return // could not tell them apart, leave it alone
	}

	for _, s := range cand {
		if s.Index != best && counts[s.Index] <= 1 {
			if mi.covers == nil {
				mi.covers = map[int]bool{}
			}
			mi.covers[s.Index] = true
		}
	}
}

// TargetDims computes the target size preserving aspect ratio. It never
// upscales. A maxWidth of 0 keeps the source size.
func TargetDims(w, h, maxWidth int) (int, int) {
	if maxWidth <= 0 || w <= maxWidth || w <= 0 || h <= 0 {
		return even(w), even(h)
	}
	nh := h * maxWidth / w
	return even(maxWidth), even(nh)
}

// even rounds to the nearest even number (encoders reject odd dimensions).
func even(n int) int {
	if n < 2 {
		return 2
	}
	if n%2 != 0 {
		n++
	}
	return n
}
