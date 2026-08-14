package main

import (
	"strings"
	"testing"
)

// The benign check has to see the whole ffmpeg output. decodeCheck used to
// shorten stderr to 300 characters before classifying it, which cut the last
// line in half; half a line matches no pattern, so a clean file was rejected.
// Between 2026-08-11 and 08-12 this eliminated 39 files whose only complaint
// was the dts warning that was already on the benign list.
func TestBenignCheckNeedsWholeOutput(t *testing.T) {
	line := "[null @ 0x58844205e600] Application provided invalid, non monotonically increasing dts to muxer in stream 0: 2 >= 2"
	full := strings.Join([]string{line, line, line}, "\n")
	if len(full) <= 300 {
		t.Fatalf("test input must exceed the truncation limit, got %d", len(full))
	}

	if !onlyBenignDecodeMsgs(full) {
		t.Error("the whole output is benign and must be accepted")
	}
	// Shortened first, the tail is "...non monotonica..." and matches nothing.
	// This asserts the trap is real, so nobody reintroduces the truncation.
	if onlyBenignDecodeMsgs(full[:300] + "...") {
		t.Error("a truncated tail must not be treated as benign; classify before shortening")
	}
}

func TestOnlyBenignDecodeMsgs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", true},
		{"single opus warning", "[opus @ 0x1] Error parsing Opus packet header.", true},
		{"two opus warnings",
			"[opus @ 0x1] Error parsing Opus packet header.\n[opus @ 0x2] Error parsing Opus packet header.", true},
		{"real corruption", "[h264 @ 0x1] corrupted macroblock 12 4", false},
		{"mixed: one unrecognised",
			"[opus @ 0x1] Error parsing Opus packet header.\n[h264 @ 0x2] decode_slice_header error", false},
		{"seek-induced dts warning",
			"[null @ 0x1] Application provided invalid, non monotonically increasing dts to muxer in stream 0: 1 >= 1", true},
		{"dts and opus together",
			"[null @ 0x1] Application provided invalid, non monotonically increasing dts to muxer in stream 0: 5 >= 5\n" +
				"[opus @ 0x2] Error parsing Opus packet header.", true},
		{"real corruption alongside dts",
			"[null @ 0x1] Application provided invalid, non monotonically increasing dts to muxer in stream 0: 1 >= 1\n" +
				"[av1 @ 0x2] Failed to get pixel format", false},
	}
	for _, c := range cases {
		if got := onlyBenignDecodeMsgs(c.in); got != c.want {
			t.Errorf("%s: onlyBenignDecodeMsgs(%q) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}
