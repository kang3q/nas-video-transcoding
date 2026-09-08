// Package mediainfo reads what is actually inside a file, so the converter can
// decide how much work it needs rather than guessing from the extension.
//
// The difference is not small. A .mkv already holding H.264 and AAC only needs
// its container swapped, which takes seconds; re-encoding it because of the
// extension would take forty minutes on the hardware this targets. The reverse
// trap is just as real: a .mp4 holding HEVC and AC3 looks converted and is not.
package mediainfo

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Stream is one track inside a file.
type Stream struct {
	Index    int    `json:"index"`
	Type     string `json:"type"` // video, audio, subtitle
	Codec    string `json:"codec"`
	Lang     string `json:"lang,omitempty"`
	Title    string `json:"title,omitempty"`
	Default  bool   `json:"default,omitempty"`
	Channels int    `json:"channels,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
}

// Bitmap reports whether a subtitle track is a picture rather than text.
// Those cannot be turned into a sidecar file or a WebVTT track; the only way
// to show them is to draw them into the video.
func (s Stream) Bitmap() bool {
	switch s.Codec {
	case "dvd_subtitle", "hdmv_pgs_subtitle", "dvb_subtitle", "xsub":
		return true
	}
	return false
}

// Info is everything worth remembering about a source file.
type Info struct {
	Duration  float64  `json:"duration"` // seconds, 0 when unknown
	Container string   `json:"container"`
	Streams   []Stream `json:"streams"`
	SrcSize   int64    `json:"src_size"`
	SrcMod    int64    `json:"src_mod"`
}

func (i Info) byType(t string) []Stream {
	var out []Stream
	for _, s := range i.Streams {
		if s.Type == t {
			out = append(out, s)
		}
	}
	return out
}

func (i Info) Audio() []Stream     { return i.byType("audio") }
func (i Info) Subtitles() []Stream { return i.byType("subtitle") }

// Video returns the first real video track. Cover art and thumbnails are
// stored as video streams too, and picking one of those would convert a
// still image instead of the film.
func (i Info) Video() (Stream, bool) {
	for _, s := range i.byType("video") {
		if !coverArt(s.Codec) {
			return s, true
		}
	}
	return Stream{}, false
}

func coverArt(codec string) bool {
	switch codec {
	case "mjpeg", "png", "bmp", "gif", "webp":
		return true
	}
	return false
}

// RemuxOnly reports whether the file already holds what a browser and an
// Apple TV both play, so only the container has to change.
func (i Info) RemuxOnly() bool {
	v, ok := i.Video()
	if !ok || v.Codec != "h264" {
		return false
	}
	audio := i.Audio()
	if len(audio) == 0 {
		return true // video-only is fine to remux
	}
	switch audio[0].Codec {
	case "aac", "mp3":
		return true
	}
	return false
}

// --- ffprobe ---

type ffStream struct {
	Index       int               `json:"index"`
	CodecName   string            `json:"codec_name"`
	CodecType   string            `json:"codec_type"`
	Channels    int               `json:"channels"`
	Width       int               `json:"width"`
	Height      int               `json:"height"`
	Tags        map[string]string `json:"tags"`
	Disposition map[string]int    `json:"disposition"`
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
	bin     string
	timeout time.Duration
	sem     chan struct{}

	mu      sync.Mutex
	entries map[string]Info
	dirty   bool
	file    string
}

// NewProber caches results on disk, keyed by path, size and modification time,
// so browsing a library a second time costs nothing.
func NewProber(bin string, timeout time.Duration, workers int, stateDir string) *Prober {
	if workers < 1 {
		workers = 1
	}
	p := &Prober{
		bin:     bin,
		timeout: timeout,
		sem:     make(chan struct{}, workers),
		entries: map[string]Info{},
		file:    filepath.Join(stateDir, "mediainfo.json"),
	}
	p.load()
	return p
}

func key(path string, size int64, modUnix int64) string {
	return fmt.Sprintf("%s|%d|%d", path, size, modUnix)
}

// Probe returns the file's contents, running ffprobe only if this exact
// version of the file has not been seen.
func (p *Prober) Probe(ctx context.Context, path string, fi os.FileInfo) (Info, error) {
	k := key(path, fi.Size(), fi.ModTime().Unix())

	p.mu.Lock()
	if info, ok := p.entries[k]; ok {
		p.mu.Unlock()
		return info, nil
	}
	p.mu.Unlock()

	info, err := p.run(ctx, path)
	if err != nil {
		return Info{}, err
	}
	info.SrcSize = fi.Size()
	info.SrcMod = fi.ModTime().Unix()

	p.mu.Lock()
	p.entries[k] = info
	p.dirty = true
	p.mu.Unlock()
	return info, nil
}

// Cached returns a previous result without running ffprobe, for callers that
// would rather show nothing than block.
func (p *Prober) Cached(path string, fi os.FileInfo) (Info, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	info, ok := p.entries[key(path, fi.Size(), fi.ModTime().Unix())]
	return info, ok
}

func (p *Prober) run(ctx context.Context, path string) (Info, error) {
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-ctx.Done():
		return Info{}, ctx.Err()
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, p.bin,
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		path,
	).Output()
	if err != nil {
		return Info{}, fmt.Errorf("ffprobe %s: %w", filepath.Base(path), err)
	}
	return parse(out)
}

func parse(raw []byte) (Info, error) {
	var ff ffOutput
	if err := json.Unmarshal(raw, &ff); err != nil {
		return Info{}, fmt.Errorf("ffprobe json: %w", err)
	}

	dur, _ := strconv.ParseFloat(strings.TrimSpace(ff.Format.Duration), 64)
	info := Info{
		Duration:  dur,
		Container: ff.Format.FormatName,
	}
	for _, s := range ff.Streams {
		if s.CodecType != "video" && s.CodecType != "audio" && s.CodecType != "subtitle" {
			continue // data, attachments
		}
		info.Streams = append(info.Streams, Stream{
			Index:    s.Index,
			Type:     s.CodecType,
			Codec:    strings.ToLower(s.CodecName),
			Lang:     strings.ToLower(s.Tags["language"]),
			Title:    s.Tags["title"],
			Default:  s.Disposition["default"] == 1,
			Channels: s.Channels,
			Width:    s.Width,
			Height:   s.Height,
		})
	}
	return info, nil
}

// --- disk cache ---

func (p *Prober) load() {
	b, err := os.ReadFile(p.file)
	if err != nil {
		return
	}
	var m map[string]Info
	if json.Unmarshal(b, &m) == nil {
		p.entries = m
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
