// Package probe inspects media files with ffprobe and decides what has to
// happen to make them playable.
//
// Probing is the slow part of browsing a large library, so results are cached
// on disk keyed by path+size+mtime and reused across restarts.
package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"nvt/ver1/internal/config"
)

type Action string

const (
	// Passthrough: the player can handle the file as it is.
	Passthrough Action = "passthrough"
	// AudioOnly: copy every stream, re-encode audio. Cheap, runs at disk speed.
	AudioOnly Action = "audio"
	// FullTranscode: video must be re-encoded. Expensive.
	FullTranscode Action = "video"
)

type Plan struct {
	Action   Action  `json:"action"`
	Reason   string  `json:"reason"`
	Duration float64 `json:"duration"`
	SrcSize  int64   `json:"src_size"`
	SrcMod   int64   `json:"src_mod"`
}

// NeedsWork reports whether the file has to be run through ffmpeg at all.
func (p Plan) NeedsWork() bool { return p.Action != Passthrough }

type ffStream struct {
	Index     int    `json:"index"`
	CodecName string `json:"codec_name"`
	CodecType string `json:"codec_type"`
	Channels  int    `json:"channels"`
}

type ffFormat struct {
	FormatName string `json:"format_name"`
	Duration   string `json:"duration"`
}

type ffOutput struct {
	Streams []ffStream `json:"streams"`
	Format  ffFormat   `json:"format"`
}

type Prober struct {
	cfg *config.Config

	sem chan struct{}

	mu      sync.Mutex
	entries map[string]Plan // key -> plan
	dirty   bool
	file    string
}

func New(cfg *config.Config) (*Prober, error) {
	p := &Prober{
		cfg:     cfg,
		sem:     make(chan struct{}, cfg.ProbeWorkers),
		entries: map[string]Plan{},
		file:    filepath.Join(cfg.CacheDir, "probe-cache.json"),
	}
	p.load()
	go p.flushLoop()
	return p, nil
}

func cacheKey(path string, size int64, mod time.Time) string {
	return fmt.Sprintf("%s|%d|%d", path, size, mod.Unix())
}

// Plan returns the conversion plan for a file, probing it if necessary.
func (p *Prober) Plan(ctx context.Context, path string, fi os.FileInfo) (Plan, error) {
	key := cacheKey(path, fi.Size(), fi.ModTime())

	p.mu.Lock()
	if pl, ok := p.entries[key]; ok {
		p.mu.Unlock()
		return pl, nil
	}
	p.mu.Unlock()

	pl, err := p.run(ctx, path, fi)
	if err != nil {
		return Plan{}, err
	}

	p.mu.Lock()
	p.entries[key] = pl
	p.dirty = true
	p.mu.Unlock()
	return pl, nil
}

// Cached returns a plan only if it is already known, without running ffprobe.
func (p *Prober) Cached(path string, fi os.FileInfo) (Plan, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pl, ok := p.entries[cacheKey(path, fi.Size(), fi.ModTime())]
	return pl, ok
}

func (p *Prober) run(ctx context.Context, path string, fi os.FileInfo) (Plan, error) {
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-ctx.Done():
		return Plan{}, ctx.Err()
	}

	ctx, cancel := context.WithTimeout(ctx, p.cfg.ProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.cfg.FFprobeBin,
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return Plan{}, fmt.Errorf("ffprobe %s: %w", filepath.Base(path), err)
	}

	var ff ffOutput
	if err := json.Unmarshal(out, &ff); err != nil {
		return Plan{}, fmt.Errorf("ffprobe json %s: %w", filepath.Base(path), err)
	}

	dur, _ := strconv.ParseFloat(strings.TrimSpace(ff.Format.Duration), 64)
	pl := decide(ff, p.cfg)
	pl.Duration = dur
	pl.SrcSize = fi.Size()
	pl.SrcMod = fi.ModTime().Unix()
	return pl, nil
}

func decide(ff ffOutput, cfg *config.Config) Plan {
	videoOK := set(cfg.VideoOK)
	audioOK := set(cfg.AudioOK)

	var badVideo, badAudio []string
	hasVideo := false

	for _, s := range ff.Streams {
		name := strings.ToLower(s.CodecName)
		switch s.CodecType {
		case "video":
			// Cover art and thumbnails show up as video streams; they are
			// harmless and must not trigger a re-encode.
			if isCoverArt(name) {
				continue
			}
			hasVideo = true
			if !videoOK[name] {
				badVideo = append(badVideo, name)
			}
		case "audio":
			if !audioOK[name] {
				badAudio = append(badAudio, name)
			}
		}
	}

	switch {
	case len(badVideo) > 0:
		reason := "video codec " + strings.Join(uniq(badVideo), "/")
		if len(badAudio) > 0 {
			reason += ", audio codec " + strings.Join(uniq(badAudio), "/")
		}
		return Plan{Action: FullTranscode, Reason: reason}
	case len(badAudio) > 0:
		return Plan{Action: AudioOnly, Reason: "audio codec " + strings.Join(uniq(badAudio), "/")}
	case !hasVideo:
		return Plan{Action: Passthrough, Reason: "no video stream"}
	default:
		return Plan{Action: Passthrough, Reason: "compatible"}
	}
}

func isCoverArt(codec string) bool {
	switch codec {
	case "mjpeg", "png", "bmp", "gif", "webp":
		return true
	}
	return false
}

func set(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[strings.ToLower(x)] = true
	}
	return m
}

func uniq(xs []string) []string {
	seen := map[string]bool{}
	out := xs[:0:0]
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

func (p *Prober) load() {
	b, err := os.ReadFile(p.file)
	if err != nil {
		return
	}
	var m map[string]Plan
	if err := json.Unmarshal(b, &m); err == nil {
		p.entries = m
	}
}

func (p *Prober) flushLoop() {
	for range time.Tick(30 * time.Second) {
		p.Flush()
	}
}

func (p *Prober) Flush() {
	p.mu.Lock()
	if !p.dirty {
		p.mu.Unlock()
		return
	}
	b, err := json.Marshal(p.entries)
	p.dirty = false
	p.mu.Unlock()
	if err != nil {
		return
	}
	tmp := p.file + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, p.file)
	}
}
