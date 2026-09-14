package main

import (
	"strings"
	"testing"
)

// Covers must never be mapped as video streams: a copied single-packet cover
// made ffmpeg 9 stop the whole encode after a couple of seconds (Mad Max 2015,
// 2026-09-14). They go in as attachments, numbered after the source's own.
func TestCoversAreAttachedNotMapped(t *testing.T) {
	withTempState(t)
	resetHWDecodeCache()

	mi := videoInfo("h264")
	mi.Streams = append(mi.Streams,
		Stream{Index: 1, CodecType: "video", CodecName: "png", Disposition: map[string]int{"attached_pic": 1}},
		Stream{Index: 2, CodecType: "attachment", CodecName: "ttf"},
	)
	j := Job{Src: "in.mkv", Info: mi, TargetW: 1280, TargetH: 720, Quality: 90,
		Covers: []CoverFile{{Path: "/tmp/c/cover.png", Filename: "cover.png", MIME: "image/png"}}}

	args := buildArgs(nvencConfig(), j, "out.mkv")
	if argsContain(args, "-map", "0:1") {
		t.Errorf("the cover stream was mapped:\n%s", strings.Join(args, " "))
	}
	if argsContain(args, "-c:v:1", "copy") {
		t.Error("no second video codec should be set when the cover is not mapped")
	}
	// The font from the source is attachment 0, so the cover becomes 1.
	for _, want := range [][]string{
		{"-attach", "/tmp/c/cover.png"},
		{"-metadata:s:t:1", "mimetype=image/png"},
		{"-metadata:s:t:1", "filename=cover.png"},
	} {
		if !argsContain(args, want...) {
			t.Errorf("missing %q in\n%s", want, strings.Join(args, " "))
		}
	}
}

func TestCoverFormat(t *testing.T) {
	for codec, want := range map[string]string{"png": "image/png", "mjpeg": "image/jpeg", "webp": "image/webp"} {
		if _, mime, ok := coverFormat(codec); !ok || mime != want {
			t.Errorf("%s: got %q %v, want %q", codec, mime, ok, want)
		}
	}
	// Anything unknown is skipped rather than attached with a guessed type.
	if _, _, ok := coverFormat("h264"); ok {
		t.Error("h264 is not a cover format")
	}
}
