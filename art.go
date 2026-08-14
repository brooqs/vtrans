package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Artwork already on disk is used as it is; nothing is fetched from the
// network and nothing is written into the library. Coverage measured on
// 2026-08-14: 42% of 1571 files have some image, the rest fall back to a
// generic icon drawn by the interface.
//
// The files themselves are too big to serve directly - folder.jpg runs to
// 2.7 MB and a screen of cards would be a hundred megabytes - so they are
// scaled down once and kept in the state directory.

// artNames are the image names media managers leave beside a film or series,
// in the order they are preferred. A poster shaped image comes first; the wide
// ones are a fallback.
var artNames = []string{
	"folder.jpg", "poster.jpg", "cover.jpg", "folder.png", "poster.png",
	"landscape.jpg", "backdrop.jpg", "fanart.jpg",
}

// artSuffixes are images named after the video file itself, which is how
// per-episode thumbnails are stored.
var artSuffixes = []string{"-thumb.jpg", "-poster.jpg", "-fanart.jpg", ".jpg", ".png"}

// FindArt returns the image that best represents a video file, or "" when
// there is none.
//
// The search widens outwards: the file's own thumbnail first, then its
// directory, then the parent. The parent matters for series, where the episode
// sits in "Season 1" and the poster belongs to the show.
func FindArt(videoPath string) string {
	base := strings.TrimSuffix(videoPath, filepath.Ext(videoPath))
	for _, suf := range artSuffixes {
		if p := base + suf; fileExists(p) {
			return p
		}
	}

	dir := filepath.Dir(videoPath)
	for _, cand := range []string{dir, filepath.Dir(dir)} {
		for _, n := range artNames {
			if p := filepath.Join(cand, n); fileExists(p) {
				return p
			}
		}
		// Season posters are named after the season, so they cannot be listed
		// ahead of time.
		if p := findSeasonPoster(cand); p != "" {
			return p
		}
	}
	return ""
}

func findSeasonPoster(dir string) string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range ents {
		n := e.Name()
		if strings.HasSuffix(n, "-poster.jpg") && strings.HasPrefix(n, "season") {
			return filepath.Join(dir, n)
		}
	}
	return ""
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func artCacheDir() string { return filepath.Join(stateDir(), "art") }

// artThumb returns a small JPEG for the given image, generating and caching it
// on first use. The cache key includes the source's size and modification time,
// so replacing the artwork replaces the thumbnail too.
func artThumb(ctx context.Context, src string) (string, error) {
	st, err := os.Stat(src)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d", src, st.Size(), st.ModTime().UnixNano())))
	out := filepath.Join(artCacheDir(), hex.EncodeToString(sum[:8])+".jpg")

	if fileExists(out) {
		return out, nil
	}
	if err := os.MkdirAll(artCacheDir(), 0o755); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	tmp := out + ".tmp"
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-v", "error", "-nostdin",
		"-i", src,
		"-vf", "scale=400:-2",
		// mjpeg refuses non-full-range YUV unless the pixel format is spelled
		// out; without this it fails to open the encoder at all.
		"-pix_fmt", "yuvj420p",
		"-frames:v", "1", "-q:v", "4",
		// The temporary name ends in .tmp, so ffmpeg cannot guess the format from
		// the extension and has to be told.
		"-f", "image2",
		"-y", tmp,
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("could not make a thumbnail: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if err := os.Rename(tmp, out); err != nil {
		return "", err
	}
	return out, nil
}
