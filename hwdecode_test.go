package main

import "testing"

func videoInfo(codecs ...string) *MediaInfo {
	mi := &MediaInfo{}
	for i, c := range codecs {
		mi.Streams = append(mi.Streams, Stream{Index: i, CodecType: "video", CodecName: c})
	}
	return mi
}

func TestHWDecodeAssumesNothing(t *testing.T) {
	withTempState(t)
	resetHWDecodeCache()

	// Nothing has been tried yet, so every codec gets the benefit of the doubt
	// and takes the hardware path. Guessing the other way would put files on the
	// slow path on hardware that handles them perfectly well.
	for _, c := range []string{"h264", "hevc", "mpeg4", "vc1", "av1"} {
		if NeedsSoftwareDecode(videoInfo(c)) {
			t.Errorf("%s: nothing is known yet, so hardware must be tried first", c)
		}
	}
}

func TestHWDecodeLearnsAndPersists(t *testing.T) {
	withTempState(t)
	resetHWDecodeCache()

	markSoftwareOnly("mpeg4")

	if !NeedsSoftwareDecode(videoInfo("mpeg4")) {
		t.Error("a codec marked software-only must take the software path")
	}
	if NeedsSoftwareDecode(videoInfo("h264")) {
		t.Error("one codec failing says nothing about another")
	}

	// The answer has to survive a restart, or the same failure is paid for on
	// every run of the service.
	resetHWDecodeCache()
	if !NeedsSoftwareDecode(videoInfo("mpeg4")) {
		t.Error("what was learned did not survive being reloaded from disk")
	}
}

func TestHWDecodeWholeJob(t *testing.T) {
	withTempState(t)
	resetHWDecodeCache()
	markSoftwareOnly("mpeg4")

	// -hwaccel is an input-level setting, so one stream decides for all of them.
	if !NeedsSoftwareDecode(videoInfo("h264", "mpeg4")) {
		t.Error("a single software-only stream must switch the whole job")
	}

	// Cover art is a video stream to ffprobe but is copied, never decoded, so
	// its codec must not drag the job onto the software path.
	mi := videoInfo("h264")
	mi.Streams = append(mi.Streams, Stream{
		Index: 1, CodecType: "video", CodecName: "mpeg4",
		Disposition: map[string]int{"attached_pic": 1},
	})
	if NeedsSoftwareDecode(mi) {
		t.Error("cover art must not decide the decode path")
	}
}

func TestHWInitFailure(t *testing.T) {
	real := []string{
		"[mpeg4 @ 0x1] No support for codec mpeg4 profile 15",
		"Failed setup for format vaapi: hwaccel initialisation returned error",
		"Impossible to convert between the formats supported by the filter 'graph 0 input from stream 0:0'",
	}
	for _, s := range real {
		if !hwInitFailure(s) {
			t.Errorf("this is a hardware-decode failure and was not recognised: %q", s)
		}
	}

	// Ordinary failures must not be mistaken for one, or a genuinely broken file
	// would be retried in software and fail again for the real reason.
	other := []string{
		"[matroska @ 0x1] Subtitle codec mov_text (94213) is not supported.",
		"Permission denied",
		"[h264 @ 0x1] corrupted macroblock",
		"",
	}
	for _, s := range other {
		if hwInitFailure(s) {
			t.Errorf("this is not a hardware-decode failure: %q", s)
		}
	}
}
