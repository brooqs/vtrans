package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Cover art travels as a Matroska attachment, not as a video stream.
//
// It used to be mapped and copied like any other stream. That broke on
// 2026-09-14 (Mad Max 2015, a PNG poster embedded as a flagless video
// stream): the copied cover is a single packet with no timestamps, and once it
// reached the muxer ffmpeg treated the other streams as finished too. The
// output held 38 frames of a two-hour film, and verification caught it as a
// duration mismatch. The cut-off point varied between 0.9 s and 3.6 s across
// runs and encoders (av1_nvenc, hevc_nvenc, libsvtav1 all affected, libx264
// oddly not), so it is a race inside ffmpeg 9's muxing queue rather than
// anything about the file.
//
// -attach sidesteps it entirely: the image is written as an attachment with
// the conventional cover filename and its MIME type, which is how Matroska
// stores covers in the first place. On the way back in, ffmpeg exposes an
// image attachment as a video stream carrying the attached_pic flag, so the
// output classifies correctly on the next scan - better than before, when the
// flag was lost in the copy and detectCovers had to recognise the cover by
// content.

// CoverFile is a cover image extracted from the source, ready to attach.
type CoverFile struct {
	Path     string
	Filename string // name inside the container: cover.png, cover1.jpg, ...
	MIME     string
}

// coverFormat maps a cover stream's codec to the file extension and MIME type
// the attachment carries. Codecs not listed here are not attached; they are
// rare enough that losing the cover is preferable to guessing a MIME type the
// muxer then writes into every copy.
func coverFormat(codec string) (ext, mime string, ok bool) {
	switch codec {
	case "png":
		return "png", "image/png", true
	case "mjpeg":
		return "jpg", "image/jpeg", true
	case "webp":
		return "webp", "image/webp", true
	case "bmp":
		return "bmp", "image/bmp", true
	case "gif":
		return "gif", "image/gif", true
	}
	return "", "", false
}

// extractCovers writes each cover stream of the source to dir and returns the
// attachments. A cover ffmpeg cannot extract is skipped with a warning rather
// than failing the job: the film matters more than its poster.
func extractCovers(ctx context.Context, src string, mi *MediaInfo, dir string) []CoverFile {
	var out []CoverFile
	for i, s := range mi.AttachedPics() {
		ext, mime, ok := coverFormat(s.CodecName)
		if !ok {
			warn("cover stream %d is %s, which is not carried over as an attachment", s.Index, s.CodecName)
			continue
		}
		name := "cover." + ext
		if i > 0 {
			name = fmt.Sprintf("cover%d.%s", i, ext)
		}
		path := filepath.Join(dir, name)
		cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin",
			"-i", src, "-map", fmt.Sprintf("0:%d", s.Index), "-c", "copy", "-frames:v", "1",
			"-f", "image2", "-y", path)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			warn("could not extract cover stream %d: %s", s.Index, strings.TrimSpace(stderr.String()))
			continue
		}
		if st, err := os.Stat(path); err != nil || st.Size() == 0 {
			warn("cover stream %d extracted to an empty file; skipped", s.Index)
			continue
		}
		out = append(out, CoverFile{Path: path, Filename: name, MIME: mime})
	}
	return out
}

// attachArgs adds the covers to the output. Attachments mapped from the source
// (-map 0:t?) come first in the output's attachment order, so the new ones are
// numbered after them; the metadata indices must line up with that.
func attachArgs(mi *MediaInfo, covers []CoverFile) []string {
	var args []string
	base := len(mi.StreamsOfType("attachment"))
	for i, c := range covers {
		args = append(args, "-attach", c.Path,
			fmt.Sprintf("-metadata:s:t:%d", base+i), "mimetype="+c.MIME,
			fmt.Sprintf("-metadata:s:t:%d", base+i), "filename="+c.Filename,
		)
	}
	return args
}
