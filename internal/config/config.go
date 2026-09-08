// Package config loads runtime settings from environment variables.
//
// Everything that could plausibly need tuning on a specific NAS / player
// combination is an env var. In particular the codec whitelists: what a given
// Infuse tier can actually play is not cleanly documented, so the decision of
// "does this file need converting" is data, not code.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	MediaDir string // read-only source tree
	CacheDir string // converted output lives here
	Listen   string

	User string // optional basic auth
	Pass string

	FFmpegBin  string
	FFprobeBin string

	// Codecs the player can handle as-is. Anything outside these lists
	// triggers a conversion.
	VideoOK []string
	AudioOK []string

	// Audio re-encode settings (the common case).
	AudioCodec    string
	AudioBitrate  string
	AudioChannels int // 0 = keep source channel layout

	// Video re-encode settings (the expensive fallback).
	VideoCodec  string
	VideoPreset string
	VideoCRF    string

	CacheMaxBytes int64
	ProbeWorkers  int
	TranscodeJobs int

	// PrefetchOnList starts conversions as soon as a directory is browsed,
	// so playback usually starts against a finished file.
	PrefetchOnList bool

	// WaitForComplete blocks a GET until conversion finishes. This gives an
	// exact Content-Length and full seeking. Falls back to progressive
	// streaming if the wait times out.
	WaitForComplete bool
	WaitTimeout     time.Duration

	ProbeTimeout time.Duration
}

func Load() *Config {
	c := &Config{
		MediaDir:   env("NVT_MEDIA_DIR", "/media"),
		CacheDir:   env("NVT_CACHE_DIR", "/cache"),
		Listen:     env("NVT_LISTEN", ":8080"),
		User:       env("NVT_USER", ""),
		Pass:       env("NVT_PASS", ""),
		FFmpegBin:  env("NVT_FFMPEG", "ffmpeg"),
		FFprobeBin: env("NVT_FFPROBE", "ffprobe"),

		// Defaults target free-tier Infuse on Apple TV: video codecs are
		// broadly fine, licensed surround audio is not.
		VideoOK: envList("NVT_VIDEO_OK",
			"h264,hevc,mpeg4,msmpeg4v3,mpeg2video,mpeg1video,vc1,vp8,vp9,av1,prores"),
		AudioOK: envList("NVT_AUDIO_OK",
			"aac,mp3,mp2,flac,alac,opus,vorbis,pcm_s16le,pcm_s24le,pcm_u8"),

		AudioCodec:    env("NVT_AUDIO_CODEC", "aac"),
		AudioBitrate:  env("NVT_AUDIO_BITRATE", "384k"),
		AudioChannels: envInt("NVT_AUDIO_CHANNELS", 0),

		VideoCodec:  env("NVT_VIDEO_CODEC", "libx264"),
		VideoPreset: env("NVT_VIDEO_PRESET", "veryfast"),
		VideoCRF:    env("NVT_VIDEO_CRF", "23"),

		CacheMaxBytes: int64(envInt("NVT_CACHE_MAX_GB", 100)) << 30,
		ProbeWorkers:  envInt("NVT_PROBE_WORKERS", 6),
		TranscodeJobs: envInt("NVT_TRANSCODE_JOBS", 1),

		PrefetchOnList:  envBool("NVT_PREFETCH", true),
		WaitForComplete: envBool("NVT_WAIT_COMPLETE", true),
		WaitTimeout:     time.Duration(envInt("NVT_WAIT_TIMEOUT_SEC", 150)) * time.Second,
		ProbeTimeout:    time.Duration(envInt("NVT_PROBE_TIMEOUT_SEC", 20)) * time.Second,
	}
	if c.TranscodeJobs < 1 {
		c.TranscodeJobs = 1
	}
	if c.ProbeWorkers < 1 {
		c.ProbeWorkers = 1
	}
	return c
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

func envList(k, def string) []string {
	raw := env(k, def)
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}
