package main

import (
	"fmt"
	"os"
	"strconv"
)

// Backend names the GPU family ffmpeg is driven through.
//
// vtrans was written against VAAPI (AMD, Intel) and later gained NVENC
// (NVIDIA). The two differ in every hardware-facing ffmpeg flag - the hwaccel
// name, the device syntax, the scaler, the encoder and its quality option - but
// in nothing else: the same streams are mapped, the same verification runs,
// the same q scale applies. So the differences live here and nowhere else.
type Backend string

const (
	// BackendAuto picks NVENC when an NVIDIA driver is loaded and VAAPI
	// otherwise. It is the zero value so a configuration written before the
	// field existed keeps working on the hardware it was written for.
	BackendAuto  Backend = ""
	BackendVAAPI Backend = "vaapi"
	BackendNVENC Backend = "nvenc"
)

// nvidiaControlDevice exists as soon as the proprietary driver is loaded,
// whether or not any /dev/dri node belongs to the card. Its presence is the
// cheapest honest answer to "is there an NVIDIA GPU here".
const nvidiaControlDevice = "/dev/nvidiactl"

// ResolveBackend turns the configured value into a concrete backend.
func (c *Config) ResolveBackend() Backend {
	switch c.Backend {
	case BackendVAAPI, BackendNVENC:
		return c.Backend
	}
	if _, err := os.Stat(nvidiaControlDevice); err == nil {
		return BackendNVENC
	}
	return BackendVAAPI
}

// hwBackend carries the ffmpeg vocabulary for one GPU family.
type hwBackend struct {
	Name    Backend
	Encoder string // ffmpeg encoder name, also what preflight looks for
	device  string // -hwaccel_device / -init_hw_device argument
	preset  string // nvenc only
}

// HW returns the backend the configuration resolves to, ready to build
// ffmpeg arguments.
func (c *Config) HW() hwBackend {
	switch c.ResolveBackend() {
	case BackendNVENC:
		dev := c.CudaDevice
		if dev == "" {
			dev = "0"
		}
		preset := c.NvencPreset
		if preset == "" {
			preset = "p6"
		}
		return hwBackend{Name: BackendNVENC, Encoder: "av1_nvenc", device: dev, preset: preset}
	default:
		return hwBackend{Name: BackendVAAPI, Encoder: "av1_vaapi", device: c.RenderDevice}
	}
}

// decodeArgs puts the decoder on the GPU and keeps the frames there. These go
// before -i.
func (b hwBackend) decodeArgs() []string {
	switch b.Name {
	case BackendNVENC:
		return []string{
			"-hwaccel", "cuda",
			"-hwaccel_device", b.device,
			"-hwaccel_output_format", "cuda",
		}
	default:
		return []string{
			"-hwaccel", "vaapi",
			"-hwaccel_device", b.device,
			"-hwaccel_output_format", "vaapi",
		}
	}
}

// filterDeviceArgs opens the device for the filter chain alone, for the
// software-decode path: no -hwaccel, so ffmpeg decodes on the CPU, but hwupload
// and the scaler still have somewhere to put the frames. These go before -i.
func (b hwBackend) filterDeviceArgs() []string {
	switch b.Name {
	case BackendNVENC:
		return []string{"-init_hw_device", "cuda=gpu:" + b.device, "-filter_hw_device", "gpu"}
	default:
		return []string{"-vaapi_device", b.device}
	}
}

// uploadPrefix converts software frames to a format the encoder accepts and
// uploads them. hwupload is generic: it targets whatever -filter_hw_device or
// -vaapi_device opened.
func (b hwBackend) uploadPrefix() string { return "format=nv12,hwupload," }

// scaleFilter resizes on the GPU.
func (b hwBackend) scaleFilter(w, h int) string {
	switch b.Name {
	case BackendNVENC:
		return fmt.Sprintf("scale_cuda=%d:%d", w, h)
	default:
		return fmt.Sprintf("scale_vaapi=%d:%d", w, h)
	}
}

// codecArgs selects the encoder for video output stream i at quality q.
//
// q is the AV1 quantiser index (0-255) on both backends: VAAPI takes it as
// -q:v under CQP rate control, NVENC as -qp under constqp. Same scale, but
// not the same result, which is why the configuration carries one pair of
// values per backend. Measured 2026-09-14, 60 s clips, VMAF against the
// source, RTX 4060 vs Radeon 890M:
//
//	Hansel & Gretel 1080p   vaapi q130: 1938 kbps VMAF 94.0
//	                        nvenc  q90: 1663 kbps VMAF 94.0
//	                        nvenc q130:  805 kbps VMAF 85.0
//	Star Trek SNW -> 720p   vaapi q150:  543 kbps VMAF 89.9
//	                        nvenc  q90:  460 kbps VMAF 89.7
//
// NVENC's VBR "constant quality" mode (-cq, 0-63, with lookahead and
// adaptive quantisation) was tried too and gave no better VMAF per bit than
// constqp on this content, with a coarser scale; constqp stays.
func (b hwBackend) codecArgs(i, q int) []string {
	qs := strconv.Itoa(q)
	switch b.Name {
	case BackendNVENC:
		return []string{
			fmt.Sprintf("-c:v:%d", i), b.Encoder,
			fmt.Sprintf("-preset:v:%d", i), b.preset,
			fmt.Sprintf("-tune:v:%d", i), "hq",
			fmt.Sprintf("-rc:v:%d", i), "constqp",
			fmt.Sprintf("-qp:v:%d", i), qs,
		}
	default:
		return []string{
			fmt.Sprintf("-c:v:%d", i), b.Encoder,
			fmt.Sprintf("-rc_mode:v:%d", i), "CQP",
			fmt.Sprintf("-q:v:%d", i), qs,
		}
	}
}

// checkDevice reports whether the device the backend needs can be opened by
// this process. The message says what to do about it, since "permission
// denied" on a device node almost always means a missing group.
func (b hwBackend) checkDevice() error {
	switch b.Name {
	case BackendNVENC:
		f, err := os.OpenFile(nvidiaControlDevice, os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("could not open %s: %w (is the NVIDIA driver loaded?)", nvidiaControlDevice, err)
		}
		_ = f.Close()
		if _, err := strconv.Atoi(b.device); err != nil {
			return fmt.Errorf("cuda device must be a card index such as 0, got %q", b.device)
		}
		return nil
	default:
		f, err := os.OpenFile(b.device, os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("could not open %s: %w (are you in the render group?)", b.device, err)
		}
		_ = f.Close()
		return nil
	}
}
