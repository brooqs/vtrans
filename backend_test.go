package main

import (
	"strings"
	"testing"
)

func nvencConfig() *Config {
	cfg := DefaultConfig()
	cfg.Backend = BackendNVENC
	return cfg
}

func vaapiConfig() *Config {
	cfg := DefaultConfig()
	cfg.Backend = BackendVAAPI
	return cfg
}

func argsContain(args []string, seq ...string) bool {
	joined := " " + strings.Join(args, " ") + " "
	return strings.Contains(joined, " "+strings.Join(seq, " ")+" ")
}

func TestBuildArgsPerBackend(t *testing.T) {
	withTempState(t)
	resetHWDecodeCache()

	j := Job{Src: "in.mkv", Info: videoInfo("h264"), TargetW: 1280, TargetH: 720, Quality: 140}

	// The two backends must disagree on every hardware-facing flag and on
	// nothing else: the encoder, the scaler, the hwaccel and the q option.
	nv := buildArgs(nvencConfig(), j, "out.mkv")
	for _, want := range [][]string{
		{"-hwaccel", "cuda", "-hwaccel_device", "0", "-hwaccel_output_format", "cuda"},
		{"-filter_complex", "[0:0]scale_cuda=1280:720[v0]"},
		{"-c:v:0", "av1_nvenc"},
		{"-rc:v:0", "constqp", "-qp:v:0", "140"},
	} {
		if !argsContain(nv, want...) {
			t.Errorf("nvenc: missing %q in\n%s", want, strings.Join(nv, " "))
		}
	}
	for _, bad := range []string{"vaapi", "-rc_mode", "-q:v:0"} {
		if argsContain(nv, bad) {
			t.Errorf("nvenc: a vaapi flag leaked in: %q", bad)
		}
	}

	va := buildArgs(vaapiConfig(), j, "out.mkv")
	for _, want := range [][]string{
		{"-hwaccel", "vaapi", "-hwaccel_device", "/dev/dri/renderD128", "-hwaccel_output_format", "vaapi"},
		{"-filter_complex", "[0:0]scale_vaapi=1280:720[v0]"},
		{"-c:v:0", "av1_vaapi", "-rc_mode:v:0", "CQP", "-q:v:0", "140"},
	} {
		if !argsContain(va, want...) {
			t.Errorf("vaapi: missing %q in\n%s", want, strings.Join(va, " "))
		}
	}
	for _, bad := range []string{"cuda", "nvenc", "-qp:v:0"} {
		if argsContain(va, bad) {
			t.Errorf("vaapi: an nvenc flag leaked in: %q", bad)
		}
	}
}

func TestBuildArgsSoftwareDecodeStillEncodesOnGPU(t *testing.T) {
	withTempState(t)
	resetHWDecodeCache()
	markSoftwareOnly("mpeg4")

	j := Job{Src: "in.avi", Info: videoInfo("mpeg4"), TargetW: 640, TargetH: 480, Quality: 150}

	nv := buildArgs(nvencConfig(), j, "out.mkv")
	if argsContain(nv, "-hwaccel") {
		t.Error("nvenc software path must not ask for hardware decode")
	}
	if !argsContain(nv, "-init_hw_device", "cuda=gpu:0", "-filter_hw_device", "gpu") {
		t.Error("nvenc software path must still open the device for the filter chain")
	}
	if !argsContain(nv, "-filter_complex", "[0:0]format=nv12,hwupload,scale_cuda=640:480[v0]") {
		t.Errorf("nvenc software path must upload before scaling:\n%s", strings.Join(nv, " "))
	}
	if !argsContain(nv, "-c:v:0", "av1_nvenc") {
		t.Error("software decode must still end in a hardware encode")
	}

	va := buildArgs(vaapiConfig(), j, "out.mkv")
	if !argsContain(va, "-vaapi_device", "/dev/dri/renderD128") ||
		!argsContain(va, "-filter_complex", "[0:0]format=nv12,hwupload,scale_vaapi=640:480[v0]") {
		t.Errorf("vaapi software path changed:\n%s", strings.Join(va, " "))
	}
}

func TestResolveBackendHonoursExplicitChoice(t *testing.T) {
	// Whatever the machine has, an explicit choice wins. Auto-detection is a
	// convenience for a fresh install, not something that overrides a person.
	if got := vaapiConfig().ResolveBackend(); got != BackendVAAPI {
		t.Errorf("explicit vaapi resolved to %q", got)
	}
	if got := nvencConfig().ResolveBackend(); got != BackendNVENC {
		t.Errorf("explicit nvenc resolved to %q", got)
	}
	// Auto resolves to one of the two, never stays empty.
	if got := DefaultConfig().ResolveBackend(); got != BackendVAAPI && got != BackendNVENC {
		t.Errorf("auto resolved to %q", got)
	}
}

func TestNvencDefaultsFillIn(t *testing.T) {
	// A configuration written before the NVENC fields existed has them empty;
	// that must not turn into "-hwaccel_device ''" on the command line.
	cfg := nvencConfig()
	cfg.CudaDevice, cfg.NvencPreset = "", ""
	hw := cfg.HW()
	if !argsContain(hw.decodeArgs(), "-hwaccel_device", "0") {
		t.Errorf("empty cuda device must fall back to card 0: %v", hw.decodeArgs())
	}
	if !argsContain(hw.codecArgs(0, 130), "-preset:v:0", "p6") {
		t.Errorf("empty preset must fall back to p6: %v", hw.codecArgs(0, 130))
	}
}

func TestHWInitFailureCUDA(t *testing.T) {
	if !hwInitFailure("[h264 @ 0x1] Failed setup for format cuda: hwaccel initialisation returned error.") {
		t.Error("the cuda spelling of a hwaccel setup failure was not recognised")
	}
}

func TestQualityIsPerBackend(t *testing.T) {
	cfg := vaapiConfig()
	cfg.QMovie, cfg.QTV, cfg.QMovieNvenc, cfg.QTVNvenc = 130, 150, 90, 100
	if got := cfg.QualityFor("/movies/a.mkv"); got != 130 {
		t.Errorf("vaapi movie q = %d, want 130", got)
	}
	if got := cfg.QualityFor("/tv/a.mkv"); got != 150 {
		t.Errorf("vaapi tv q = %d, want 150", got)
	}
	cfg.Backend = BackendNVENC
	if got := cfg.QualityFor("/movies/a.mkv"); got != 90 {
		t.Errorf("nvenc movie q = %d, want 90", got)
	}
	if got := cfg.QualityFor("/tv/a.mkv"); got != 100 {
		t.Errorf("nvenc tv q = %d, want 100", got)
	}

	// An old configuration has no NVENC values. Using the VAAPI ones would
	// encode at q130 on NVENC, which measured at VMAF 85 against 94: a
	// silent quality drop across the whole library. The defaults apply.
	cfg.QMovieNvenc, cfg.QTVNvenc = 0, 0
	if got := cfg.QualityFor("/movies/a.mkv"); got != 90 {
		t.Errorf("nvenc movie q with empty field = %d, want the default 90", got)
	}
}
