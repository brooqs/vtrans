package main

import (
	"strings"
	"testing"
)

// kinds collects the finding kinds so a test can assert on them without caring
// about wording.
func kinds(fs []Finding) map[string]Finding {
	out := map[string]Finding{}
	for _, f := range fs {
		out[f.Kind] = f
	}
	return out
}

func TestDiagnoseUnreadableSubtitles(t *testing.T) {
	// The Ghosts case: WebVTT in a Matroska file, which ffmpeg reports as an
	// unknown codec and can neither copy nor decode.
	mi := &MediaInfo{
		Duration: 1800,
		Streams: []Stream{
			{Index: 0, CodecType: "video", CodecName: "h264", Width: 1920, Height: 1080},
			{Index: 1, CodecType: "audio", CodecName: "aac"},
			{Index: 2, CodecType: "subtitle", CodecName: "unknown"},
			{Index: 3, CodecType: "subtitle", CodecName: "unknown"},
		},
	}

	got := kinds(diagnoseFindings(&Config{SkipOnStreamLoss: true}, mi))
	f, ok := got["subtitle_unreadable"]
	if !ok {
		t.Fatalf("unreadable subtitles were not reported: %v", got)
	}
	if f.Severity != SevWarn {
		t.Errorf("severity = %q, want %q", f.Severity, SevWarn)
	}
	if len(f.Streams) != 2 {
		t.Errorf("streams = %v, want the two subtitle indexes", f.Streams)
	}
	if !strings.Contains(f.Detail, "skipped") {
		t.Errorf("with the guard on, the detail should say the file is skipped: %q", f.Detail)
	}

	// With the guard off the same file would be processed and the subtitles
	// really would be lost - the wording has to change with it.
	got = kinds(diagnoseFindings(&Config{SkipOnStreamLoss: false}, mi))
	if d := got["subtitle_unreadable"].Detail; !strings.Contains(d, "WOULD be lost") {
		t.Errorf("with the guard off the warning should be stronger: %q", d)
	}
}

func TestDiagnoseConvertedSubtitles(t *testing.T) {
	// mov_text is readable but Matroska cannot store it, so it is converted.
	// That is worth reporting, but it is not a problem.
	mi := &MediaInfo{
		Duration: 1800,
		Streams: []Stream{
			{Index: 0, CodecType: "video", CodecName: "h264", Width: 1920, Height: 1080},
			{Index: 1, CodecType: "subtitle", CodecName: "mov_text"},
			{Index: 2, CodecType: "subtitle", CodecName: "subrip"},
		},
	}
	got := kinds(diagnoseFindings(&Config{SkipOnStreamLoss: true}, mi))

	f, ok := got["subtitle_converted"]
	if !ok {
		t.Fatalf("the converted subtitle was not reported: %v", got)
	}
	if f.Severity != SevInfo {
		t.Errorf("a conversion loses nothing, so it is info, not %q", f.Severity)
	}
	// Only mov_text is converted; subrip is copied and must not be counted.
	if len(f.Streams) != 1 || f.Streams[0] != 1 {
		t.Errorf("streams = %v, want just the mov_text stream", f.Streams)
	}
	if _, bad := got["subtitle_unreadable"]; bad {
		t.Error("a readable subtitle must not be reported as unreadable")
	}
}

func TestDiagnoseBrokenFile(t *testing.T) {
	mi := &MediaInfo{
		Duration: 0,
		Streams:  []Stream{{Index: 0, CodecType: "audio", CodecName: "aac"}},
	}
	got := kinds(diagnoseFindings(&Config{}, mi))
	if _, ok := got["no_video"]; !ok {
		t.Error("a file with no video stream must be reported")
	}
	if _, ok := got["no_duration"]; !ok {
		t.Error("an unknown duration must be reported")
	}
}

func TestDiagnoseCleanFile(t *testing.T) {
	mi := &MediaInfo{
		Duration: 1800,
		Streams: []Stream{
			{Index: 0, CodecType: "video", CodecName: "h264", Width: 1920, Height: 1080},
			{Index: 1, CodecType: "audio", CodecName: "aac"},
			{Index: 2, CodecType: "subtitle", CodecName: "subrip"},
		},
	}
	if f := diagnoseFindings(&Config{SkipOnStreamLoss: true}, mi); len(f) != 0 {
		t.Errorf("an ordinary file should produce no findings, got %v", f)
	}
}

func TestReportWorst(t *testing.T) {
	cases := []struct {
		name string
		rep  FileReport
		want string
	}{
		{"probe failure outranks everything", FileReport{Err: "no such file"}, SevError},
		{"nothing found", FileReport{}, ""},
		{
			"warn beats info regardless of order",
			FileReport{Findings: []Finding{{Severity: SevInfo}, {Severity: SevWarn}, {Severity: SevInfo}}},
			SevWarn,
		},
		{
			"error beats warn",
			FileReport{Findings: []Finding{{Severity: SevWarn}, {Severity: SevError}}},
			SevError,
		},
	}
	for _, c := range cases {
		if got := c.rep.Worst(); got != c.want {
			t.Errorf("%s: Worst() = %q, want %q", c.name, got, c.want)
		}
	}
}
