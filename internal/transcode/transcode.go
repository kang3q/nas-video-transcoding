// Package transcode runs ffmpeg jobs and lets callers wait on them.
//
// Jobs are deduplicated by cache key: two players hitting the same file share
// one ffmpeg process. Concurrency is capped because the target hardware is a
// low-power NAS CPU.
package transcode

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"nvt/internal/cache"
	"nvt/internal/config"
	"nvt/internal/probe"
)

type Job struct {
	Key     string
	Src     string
	Plan    probe.Plan
	Started time.Time

	done chan struct{}
	err  error
}

// Done is closed when the job finishes, successfully or not.
func (j *Job) Done() <-chan struct{} { return j.done }

func (j *Job) Err() error {
	select {
	case <-j.done:
		return j.err
	default:
		return nil
	}
}

func (j *Job) Finished() bool {
	select {
	case <-j.done:
		return true
	default:
		return false
	}
}

type Manager struct {
	cfg   *config.Config
	cache *cache.Cache
	sem   chan struct{}

	mu   sync.Mutex
	jobs map[string]*Job
}

func NewManager(cfg *config.Config, c *cache.Cache) *Manager {
	return &Manager{
		cfg:   cfg,
		cache: c,
		sem:   make(chan struct{}, cfg.TranscodeJobs),
		jobs:  map[string]*Job{},
	}
}

// Start begins a conversion, or returns the already-running job for this key.
// Returns nil if the output is already complete.
func (m *Manager) Start(key, src string, pl probe.Plan) *Job {
	if m.cache.Complete(key) {
		return nil
	}

	m.mu.Lock()
	if j, ok := m.jobs[key]; ok {
		m.mu.Unlock()
		return j
	}
	j := &Job{Key: key, Src: src, Plan: pl, Started: time.Now(), done: make(chan struct{})}
	m.jobs[key] = j
	m.mu.Unlock()

	go m.run(j)
	return j
}

// Active returns a snapshot of the jobs currently known to the manager.
func (m *Manager) Active() []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, j)
	}
	return out
}

func (m *Manager) run(j *Job) {
	defer close(j.done)

	m.sem <- struct{}{}
	defer func() { <-m.sem }()

	// Another request may have finished this while we sat in the queue.
	if m.cache.Complete(j.Key) {
		return
	}

	dst := m.cache.Path(j.Key)
	log.Printf("transcode start: %s (%s: %s)", j.Src, j.Plan.Action, j.Plan.Reason)

	err := m.exec(j, dst, true)
	if err != nil {
		// Subtitle streams are the usual cause of a mux failure (mov_text has
		// no Matroska mapping, for one). Retry without them before giving up.
		log.Printf("transcode retry without subtitles: %s (%v)", j.Src, err)
		err = m.exec(j, dst, false)
	}
	if err != nil {
		j.err = err
		m.cache.Discard(j.Key)
		log.Printf("transcode failed: %s: %v", j.Src, err)
		m.forget(j.Key)
		return
	}

	if err := m.cache.MarkComplete(j.Key); err != nil {
		j.err = err
		log.Printf("transcode marker failed: %s: %v", j.Src, err)
		m.forget(j.Key)
		return
	}

	log.Printf("transcode done: %s in %s (%d bytes)",
		j.Src, time.Since(j.Started).Round(time.Second), m.cache.Size(j.Key))
	m.forget(j.Key)
	m.cache.Evict(m.cfg.CacheMaxBytes)
}

func (m *Manager) forget(key string) {
	m.mu.Lock()
	delete(m.jobs, key)
	m.mu.Unlock()
}

func (m *Manager) exec(j *Job, dst string, withSubs bool) error {
	args := m.args(j.Plan, j.Src, dst, withSubs)
	cmd := exec.CommandContext(context.Background(), m.cfg.FFmpegBin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, tail(stderr.String(), 600))
	}
	return nil
}

// args builds the ffmpeg invocation. The audio-only path is the important one:
// every stream is copied and only audio is re-encoded, so it runs at disk
// speed and the video is bit-identical to the source.
func (m *Manager) args(pl probe.Plan, src, dst string, withSubs bool) []string {
	a := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-fflags", "+genpts",
		"-i", src,
		"-map", "0:v?", "-map", "0:a?",
	}
	if withSubs {
		a = append(a, "-map", "0:s?")
	}
	a = append(a, "-map_metadata", "0", "-max_muxing_queue_size", "4096")

	switch pl.Action {
	case probe.FullTranscode:
		a = append(a,
			"-c:v", m.cfg.VideoCodec,
			"-preset", m.cfg.VideoPreset,
			"-crf", m.cfg.VideoCRF,
			"-pix_fmt", "yuv420p",
		)
	default:
		a = append(a, "-c:v", "copy")
	}

	a = append(a, "-c:a", m.cfg.AudioCodec, "-b:a", m.cfg.AudioBitrate)
	if m.cfg.AudioChannels > 0 {
		a = append(a, "-ac", strconv.Itoa(m.cfg.AudioChannels))
	}
	if withSubs {
		a = append(a, "-c:s", "copy")
	}

	return append(a, "-f", "matroska", dst)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
