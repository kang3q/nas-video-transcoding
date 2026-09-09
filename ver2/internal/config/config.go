// Package config loads v2's settings from the environment.
//
// The variables are prefixed NVT2_ so v1 and v2 can be described in one compose
// file without colliding. There are far fewer of them than v1 had: most of v1's
// knobs tuned the machinery that guessed what a player wanted, and v2 does not
// guess.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	SourceDir string // read-only library
	OutputDir string // converted MP4s, mirroring SourceDir's shape
	StateDir  string // probe cache, live HLS segments, queue state
	Listen    string

	User string // optional basic auth
	Pass string

	FFmpegBin  string
	FFprobeBin string

	// Workers is how many ffmpeg jobs run at once. One by default: an H.264
	// encode saturates the machine, and running two only makes both late.
	Workers int
	// Threads is ffmpeg's -threads. Left below the core count so the web UI
	// stays responsive while an encode runs.
	Threads int

	Preset        string
	VideoBitrate  string
	AudioBitrate  string
	AudioChannels int
	AudioRate     int

	// CheckpointPercent is how far into the first file of a batch to get
	// before offering it up for inspection. Far enough past an opening
	// sequence that burned-in subtitles are actually on screen.
	CheckpointPercent int

	// Live writes an HLS rendition alongside the MP4 so a job can be watched
	// before it finishes. One encode feeds both, so the cost is disk, not CPU.
	Live        bool
	SegmentSecs int

	TelegramToken string
	TelegramChat  string
	PublicBaseURL string // used in notifications, e.g. http://nas.local:8080
	ProbeTimeout  time.Duration
	LogRequests   bool

	// OutputRel is set when OutputDir lives inside SourceDir: the library must
	// then hide it, or the service offers to convert its own output.
	OutputRel string
}

func Load() (*Config, error) {
	c := &Config{
		SourceDir: env("NVT2_SOURCE_DIR", "/media"),
		OutputDir: env("NVT2_OUTPUT_DIR", "/output"),
		StateDir:  env("NVT2_STATE_DIR", "/state"),
		Listen:    env("NVT2_LISTEN", ":8080"),
		User:      env("NVT2_USER", ""),
		Pass:      env("NVT2_PASS", ""),

		FFmpegBin:  env("NVT2_FFMPEG", "ffmpeg"),
		FFprobeBin: env("NVT2_FFPROBE", "ffprobe"),

		Workers: envInt("NVT2_WORKERS", 1),
		Threads: envInt("NVT2_THREADS", 3),

		Preset:        env("NVT2_PRESET", "superfast"),
		VideoBitrate:  env("NVT2_VIDEO_BITRATE", "2600k"),
		AudioBitrate:  env("NVT2_AUDIO_BITRATE", "320k"),
		AudioChannels: envInt("NVT2_AUDIO_CHANNELS", 2),
		AudioRate:     envInt("NVT2_AUDIO_RATE", 48000),

		CheckpointPercent: envInt("NVT2_CHECKPOINT_PERCENT", 10),
		Live:              envBool("NVT2_LIVE", true),
		SegmentSecs:       envInt("NVT2_SEGMENT_SEC", 4),

		TelegramToken: env("NVT2_TELEGRAM_TOKEN", ""),
		TelegramChat:  env("NVT2_TELEGRAM_CHAT_ID", ""),
		PublicBaseURL: strings.TrimRight(env("NVT2_PUBLIC_URL", ""), "/"),

		ProbeTimeout: time.Duration(envInt("NVT2_PROBE_TIMEOUT_SEC", 30)) * time.Second,
		LogRequests:  envBool("NVT2_LOG_REQUESTS", true),
	}

	if c.Workers < 1 {
		c.Workers = 1
	}
	if c.Threads < 0 {
		c.Threads = 0
	}
	if c.CheckpointPercent < 1 || c.CheckpointPercent > 99 {
		c.CheckpointPercent = 10
	}
	if c.SegmentSecs < 1 || c.SegmentSecs > 30 {
		c.SegmentSecs = 4
	}

	if err := c.resolveDirs(); err != nil {
		return nil, err
	}
	return c, nil
}

// resolveDirs makes the three directories absolute and settles how the source
// and output trees relate. Nesting the output inside the source is allowed —
// it is the natural place for it on a NAS share — but it has to be recorded so
// the library can hide it.
func (c *Config) resolveDirs() error {
	src, err := resolveDir(c.SourceDir, false)
	if err != nil {
		return fmt.Errorf("NVT2_SOURCE_DIR: %w", err)
	}
	out, err := resolveDir(c.OutputDir, true)
	if err != nil {
		return fmt.Errorf("NVT2_OUTPUT_DIR: %w", err)
	}
	state, err := resolveDir(c.StateDir, true)
	if err != nil {
		return fmt.Errorf("NVT2_STATE_DIR: %w", err)
	}
	c.SourceDir, c.OutputDir, c.StateDir = src, out, state

	if src == out {
		return fmt.Errorf("source and output directories are the same: %s", src)
	}
	if within(src, out) {
		// The source inside the output would mean converting into the tree we
		// read from, with the output of one job as the input of the next.
		return fmt.Errorf("source directory %s is inside output directory %s", src, out)
	}
	if within(out, src) {
		rel, err := filepath.Rel(src, out)
		if err != nil {
			return err
		}
		c.OutputRel = filepath.ToSlash(rel)
	}
	return nil
}

func resolveDir(dir string, create bool) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("not set")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if create {
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return "", err
		}
	}
	// Resolve symlinks so the containment checks below compare real paths.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", resolved)
	}
	return resolved, nil
}

// within reports whether child sits under parent. Both must already be
// absolute and symlink-resolved.
func within(child, parent string) bool {
	if child == parent {
		return false
	}
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b
		}
	}
	return def
}
