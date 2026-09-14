package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Which codecs the GPU can decode is a property of the machine, not of vtrans.
// An Intel iGPU from 2015 has no HEVC decoder; one from 2023 has AV1; AMD and
// the various Intel generations all differ. A list written against one machine
// is wrong on the next, and wrong in the dangerous direction: claiming hardware
// decode where there is none makes every job with that codec die at hardware
// initialisation.
//
// So nothing is assumed. Hardware decode is tried, and if ffmpeg fails to set
// it up the codec is written down as software-only and every later file with
// that codec goes straight to the software path. One quick failure per codec,
// once, and the answer is remembered across restarts.

func hwDecodePath() string { return filepath.Join(stateDir(), "hwdecode.json") }

type hwDecodeCache struct {
	mu sync.Mutex
	// SoftwareOnly holds the codecs whose hardware decode was tried and failed.
	// Absent means "not tried yet", which is treated as "try hardware".
	SoftwareOnly map[string]bool `json:"software_only"`
	loaded       bool
}

var hwDecode hwDecodeCache

// resetHWDecodeCache drops what has been learned so the next call reads the
// state directory again. Only the tests need this; the running binary learns
// once and keeps the answer.
func resetHWDecodeCache() {
	hwDecode.mu.Lock()
	defer hwDecode.mu.Unlock()
	hwDecode.loaded = false
	hwDecode.SoftwareOnly = nil
}

func (c *hwDecodeCache) load() {
	if c.loaded {
		return
	}
	c.loaded = true
	c.SoftwareOnly = map[string]bool{}

	data, err := os.ReadFile(hwDecodePath())
	if err != nil {
		return
	}
	var on struct {
		SoftwareOnly map[string]bool `json:"software_only"`
	}
	if json.Unmarshal(data, &on) == nil && on.SoftwareOnly != nil {
		c.SoftwareOnly = on.SoftwareOnly
	}
}

// NeedsSoftwareDecode reports whether a file has to be decoded on the CPU.
//
// The answer covers the whole job rather than a single stream: -hwaccel is an
// input-level setting, so one stream needing software decode puts all of them
// on that path. Software decode is slower but still ends in a hardware encode,
// because the filter chain uploads the frames to the GPU.
func NeedsSoftwareDecode(m *MediaInfo) bool {
	hwDecode.mu.Lock()
	defer hwDecode.mu.Unlock()
	hwDecode.load()

	for _, s := range m.VideoStreams() {
		if hwDecode.SoftwareOnly[s.CodecName] {
			return true
		}
	}
	return false
}

// markSoftwareOnly records that hardware decode of a codec does not work here.
// Writing the file is best effort: losing it costs one repeated attempt, not
// correctness.
func markSoftwareOnly(codec string) {
	if codec == "" {
		return
	}
	hwDecode.mu.Lock()
	defer hwDecode.mu.Unlock()
	hwDecode.load()

	if hwDecode.SoftwareOnly[codec] {
		return
	}
	hwDecode.SoftwareOnly[codec] = true

	_ = os.MkdirAll(stateDir(), 0o755)
	data, err := json.MarshalIndent(struct {
		SoftwareOnly map[string]bool `json:"software_only"`
	}{hwDecode.SoftwareOnly}, "", "  ")
	if err != nil {
		return
	}
	tmp := hwDecodePath() + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, hwDecodePath())
	}
}

// hwInitFailure recognises ffmpeg giving up on hardware decode.
//
// These are the messages seen when the GPU has no decoder for the input: the
// first two are ffmpeg refusing the profile or the hwaccel setup itself
// failing (one spelling per backend), and the last is the filter chain being
// handed software frames it expected on the GPU. Any of them means the same
// thing. The last one is also how NVDEC says no: -hwaccel cuda does not fail
// for a codec it lacks, ffmpeg quietly decodes in software and scale_cuda is
// then given frames it cannot take.
func hwInitFailure(stderr string) bool {
	for _, sig := range []string{
		"Failed setup for format vaapi",
		"Failed setup for format cuda",
		"hwaccel initialisation returned error",
		"No support for codec",
		"Impossible to convert between the formats supported by the filter",
	} {
		if strings.Contains(stderr, sig) {
			return true
		}
	}
	return false
}
