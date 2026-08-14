package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Mode decides whether the output replaces the original or goes to a separate tree.
type Mode string

const (
	ModeReplace Mode = "replace" // replaces the original after a successful, verified encode
	ModeCopy    Mode = "copy"    // leaves the original alone, writes under DestRoot
)

type Config struct {
	// Library roots. There may be more than one.
	Roots []string `json:"roots"`

	Mode Mode `json:"mode"`

	// Output root for ModeCopy.
	DestRoot string `json:"dest_root"`

	// In ModeReplace, replaced originals are moved here (deleted outright if empty).
	// Space is only reclaimed once the trash is emptied - a deliberate safety tradeoff.
	TrashDir string `json:"trash_dir"`

	// AV1 quality (global_quality, 0-255). Higher value = smaller file.
	QMovie int `json:"q_movie"`
	QTV    int `json:"q_tv"`

	// TV shows are scaled down to this width (aspect ratio preserved). 0 = no downscale.
	TVMaxWidth int `json:"tv_max_width"`
	// Upper bound for movies. 0 = keep source resolution.
	MovieMaxWidth int `json:"movie_max_width"`

	// "copy" | "opus" | "none"
	AudioMode string `json:"audio_mode"`
	// Opus stereo bitrate (kbps). 5.1 tracks get twice this.
	OpusStereoKbps int `json:"opus_stereo_kbps"`

	// These codecs are already considered efficient and are skipped.
	SkipCodecs []string `json:"skip_codecs"`
	// Files below this Mbps are skipped.
	MinBitrateMbps float64 `json:"min_bitrate_mbps"`
	// The output is rejected unless it is at least this much smaller than the source.
	// (Replacing the original is pointless without a real gain.)
	MinSavingRatio float64 `json:"min_saving_ratio"`

	RenderDevice string `json:"render_device"`

	// Skip the file entirely if any stream cannot be copied (e.g. codec_name=unknown
	// subtitles). Prevents silent stream loss in replace mode; set false to drop the
	// stream and carry on.
	SkipOnStreamLoss bool `json:"skip_on_stream_loss"`

	// "sample" (start/middle/end are decoded) or "full" (everything is decoded)
	VerifyMode string `json:"verify_mode"`
	// Allowed duration difference (seconds).
	DurationToleranceSec float64 `json:"duration_tolerance_sec"`

	// watch: a file counts as "finished writing" once its size holds steady this long.
	StableSeconds int `json:"stable_seconds"`
	// watch: periodic full scan to catch events fsnotify missed (minutes). 0 = off.
	RescanMinutes int `json:"rescan_minutes"`

	// TV vs movie: a path containing any of these fragments counts as TV.
	TVPathMarkers []string `json:"tv_path_markers"`
}

func DefaultConfig() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		Roots:                []string{filepath.Join(home, "videos")},
		Mode:                 ModeReplace,
		DestRoot:             filepath.Join(home, "videos", "transcoded"),
		TrashDir:             filepath.Join(home, "videos", ".vtrans-trash"),
		QMovie:               130,
		QTV:                  150,
		TVMaxWidth:           1280,
		MovieMaxWidth:        0,
		AudioMode:            "opus",
		OpusStereoKbps:       128,
		SkipCodecs:           []string{"av1"},
		MinBitrateMbps:       1.5,
		MinSavingRatio:       0.25,
		RenderDevice:         "/dev/dri/renderD128",
		SkipOnStreamLoss:     true,
		VerifyMode:           "sample",
		DurationToleranceSec: 2.0,
		StableSeconds:        30,
		RescanMinutes:        60,
		TVPathMarkers:        []string{"/tv/", "/TV/", "/series/", "/diziler/"},
	}
}

func configPath() string {
	if p := os.Getenv("VTRANS_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "vtrans", "config.json")
}

func stateDir() string {
	if p := os.Getenv("VTRANS_STATE"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "vtrans")
}

// LoadConfig reads the file; if it does not exist, writes and returns defaults.
func LoadConfig() (*Config, error) {
	cfg := DefaultConfig()
	p := configPath()

	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		if err := SaveConfig(cfg); err != nil {
			return nil, fmt.Errorf("could not write default configuration: %w", err)
		}
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("could not read %s: %w", p, err)
	}
	return cfg, nil
}

func SaveConfig(cfg *Config) error {
	p := configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(data, '\n'), 0o644)
}

// IsTV reports whether the path belongs to a TV show.
func (c *Config) IsTV(path string) bool {
	for _, m := range c.TVPathMarkers {
		if containsFold(path, m) {
			return true
		}
	}
	return false
}

// QualityFor returns the q value to apply to a file.
func (c *Config) QualityFor(path string) int {
	if c.IsTV(path) {
		return c.QTV
	}
	return c.QMovie
}

// MaxWidthFor returns the target width cap (0 = unlimited).
func (c *Config) MaxWidthFor(path string) int {
	if c.IsTV(path) {
		return c.TVMaxWidth
	}
	return c.MovieMaxWidth
}
