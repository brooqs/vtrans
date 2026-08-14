package main

import "testing"

func TestSubCodecFor(t *testing.T) {
	cases := []struct {
		codec string
		want  string
	}{
		// Matroska stores these as they are.
		{"subrip", "copy"},
		{"ass", "copy"},
		{"hdmv_pgs_subtitle", "copy"},
		{"dvd_subtitle", "copy"},
		{"webvtt", "copy"},

		// mov_text is the one that used to crash ffmpeg: it must be converted,
		// never copied.
		{"mov_text", "srt"},

		// Anything else unknown to the muxer takes the same route.
		{"eia_608", "srt"},
	}
	for _, c := range cases {
		if got := SubCodecFor(Stream{CodecName: c.codec}); got != c.want {
			t.Errorf("SubCodecFor(%q) = %q, want %q", c.codec, got, c.want)
		}
	}
}
