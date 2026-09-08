// Package transcode runs ffmpeg jobs and lets callers wait on them.
//
// Work is scheduled, not merely rate limited. A file the viewer actually
// pressed play on must not sit behind speculative work, so playback jobs jump
// the queue and will preempt a running prefetch. Jobs are deduplicated by
// cache key, so two players hitting the same file share one ffmpeg process.
package transcode

import (
	"bytes"
	"context"
	"errors"
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

// ErrDropped is reported to anyone waiting on a queued job that was discarded
// before it ever ran, because the viewer moved on to a different directory.
var ErrDropped = errors.New("nvt: queued conversion dropped")

type Priority int

const (
	// Prefetch is speculative: nobody has asked for this file yet.
	Prefetch Priority = iota
	// Playback means a player is waiting on this file right now.
	Playback
)

func (p Priority) String() string {
	if p == Playback {
		return "playback"
	}
	return "prefetch"
}

type Job struct {
	Key  string
	Src  string
	Dir  string // virtual directory, used to drop stale prefetch work
	Plan probe.Plan

	done chan struct{}

	mu        sync.Mutex
	priority  Priority
	running   bool
	started   time.Time
	cancel    context.CancelFunc
	preempted bool
	err       error
}

func (j *Job) Done() <-chan struct{} { return j.done }

func (j *Job) Finished() bool {
	select {
	case <-j.done:
		return true
	default:
		return false
	}
}

func (j *Job) Err() error {
	select {
	case <-j.done:
		j.mu.Lock()
		defer j.mu.Unlock()
		return j.err
	default:
		return nil
	}
}

func (j *Job) Priority() Priority {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.priority
}

func (j *Job) Running() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.running
}

func (j *Job) Started() time.Time {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.started
}

// raise promotes a queued job when a viewer asks for a file that was until now
// only speculative. It never demotes.
func (j *Job) raise(p Priority) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if p <= j.priority {
		return false
	}
	j.priority = p
	return true
}

type Manager struct {
	cfg   *config.Config
	cache *cache.Cache
	slots int

	mu      sync.Mutex
	jobs    map[string]*Job // pending + running, keyed by cache key
	pending []*Job
	running map[string]*Job

	wake chan struct{}
}

func NewManager(cfg *config.Config, c *cache.Cache) *Manager {
	m := &Manager{
		cfg:     cfg,
		cache:   c,
		slots:   cfg.TranscodeJobs,
		jobs:    map[string]*Job{},
		running: map[string]*Job{},
		wake:    make(chan struct{}, 1),
	}
	go m.dispatchLoop()
	return m
}

// Start queues a conversion, or returns the job already handling this key,
// promoting it if this caller needs it more urgently. Returns nil if the
// output is already complete.
func (m *Manager) Start(key, src, dir string, pl probe.Plan, pr Priority) *Job {
	if m.cache.Complete(key) {
		return nil
	}

	m.mu.Lock()
	if j, ok := m.jobs[key]; ok {
		m.mu.Unlock()
		if j.raise(pr) {
			log.Printf("promote to %s: %s", pr, src)
			m.nudge()
		}
		return j
	}
	j := &Job{Key: key, Src: src, Dir: dir, Plan: pl, priority: pr, done: make(chan struct{})}
	m.jobs[key] = j
	m.pending = append(m.pending, j)
	m.mu.Unlock()

	m.nudge()
	return j
}

// DropPendingOutside discards queued prefetch work belonging to other
// directories. Browsing away is a strong signal that the speculation was
// wrong, and on a NAS CPU that queue would otherwise run to completion.
func (m *Manager) DropPendingOutside(dir string) int {
	m.mu.Lock()
	keep := make([]*Job, 0, len(m.pending))
	var dropped []*Job
	for _, j := range m.pending {
		if j.Priority() == Prefetch && j.Dir != dir {
			dropped = append(dropped, j)
			delete(m.jobs, j.Key)
			continue
		}
		keep = append(keep, j)
	}
	m.pending = keep
	m.mu.Unlock()

	for _, j := range dropped {
		j.mu.Lock()
		j.err = ErrDropped
		j.mu.Unlock()
		close(j.done)
	}
	if len(dropped) > 0 {
		log.Printf("dropped %d queued prefetch job(s) outside %s", len(dropped), dir)
	}
	return len(dropped)
}

// Snapshot describes one job for the status endpoint.
type Snapshot struct {
	Src      string
	Action   string
	Reason   string
	Priority string
	Running  bool
	Elapsed  time.Duration
}

func (m *Manager) Active() []Snapshot {
	m.mu.Lock()
	jobs := make([]*Job, 0, len(m.jobs))
	for _, j := range m.running {
		jobs = append(jobs, j)
	}
	jobs = append(jobs, m.pending...)
	m.mu.Unlock()

	out := make([]Snapshot, 0, len(jobs))
	for _, j := range jobs {
		s := Snapshot{
			Src:      j.Src,
			Action:   string(j.Plan.Action),
			Reason:   j.Plan.Reason,
			Priority: j.Priority().String(),
			Running:  j.Running(),
		}
		if s.Running {
			s.Elapsed = time.Since(j.Started()).Round(time.Second)
		}
		out = append(out, s)
	}
	return out
}

func (m *Manager) nudge() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) dispatchLoop() {
	for range m.wake {
		m.dispatch()
	}
}

func (m *Manager) dispatch() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for len(m.running) < m.slots {
		j := m.popBestLocked()
		if j == nil {
			break
		}
		m.startLocked(j)
	}

	// Every slot is busy. If a viewer is waiting while a guess is being
	// computed, stop the guess and let the scheduler pick the real work up.
	if len(m.running) >= m.slots && m.hasPendingLocked(Playback) {
		if victim := m.runningPrefetchLocked(); victim != nil {
			log.Printf("preempting prefetch for playback: %s", victim.Src)
			victim.mu.Lock()
			victim.preempted = true
			cancel := victim.cancel
			victim.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		}
	}
}

// popBestLocked takes the highest priority job, oldest first within a priority.
func (m *Manager) popBestLocked() *Job {
	best := -1
	for i, j := range m.pending {
		if best == -1 || j.Priority() > m.pending[best].Priority() {
			best = i
		}
	}
	if best == -1 {
		return nil
	}
	j := m.pending[best]
	m.pending = append(m.pending[:best], m.pending[best+1:]...)
	return j
}

func (m *Manager) hasPendingLocked(p Priority) bool {
	for _, j := range m.pending {
		if j.Priority() == p {
			return true
		}
	}
	return false
}

func (m *Manager) runningPrefetchLocked() *Job {
	for _, j := range m.running {
		if j.Priority() == Prefetch {
			return j
		}
	}
	return nil
}

func (m *Manager) startLocked(j *Job) {
	ctx, cancel := context.WithCancel(context.Background())
	j.mu.Lock()
	j.running = true
	j.started = time.Now()
	j.cancel = cancel
	j.preempted = false
	j.mu.Unlock()

	m.running[j.Key] = j
	go m.run(ctx, j)
}

func (m *Manager) run(ctx context.Context, j *Job) {
	// Something else may have produced this while the job sat in the queue.
	if m.cache.Complete(j.Key) {
		m.finish(j, nil)
		return
	}

	log.Printf("transcode start [%s]: %s (%s: %s)", j.Priority(), j.Src, j.Plan.Action, j.Plan.Reason)
	err := m.convert(ctx, j)

	j.mu.Lock()
	preempted := j.preempted
	j.running = false
	j.cancel = nil
	j.mu.Unlock()

	if preempted {
		// Partial output is useless without its completion marker.
		m.cache.Discard(j.Key)
		m.mu.Lock()
		delete(m.running, j.Key)
		m.pending = append(m.pending, j)
		m.mu.Unlock()
		log.Printf("prefetch preempted, requeued: %s", j.Src)
		m.nudge()
		return
	}

	if err != nil {
		m.cache.Discard(j.Key)
		log.Printf("transcode failed: %s: %v", j.Src, err)
		m.finish(j, err)
		return
	}

	if err := m.cache.MarkComplete(j.Key); err != nil {
		m.finish(j, err)
		return
	}
	log.Printf("transcode done: %s in %s (%d bytes)",
		j.Src, time.Since(j.Started()).Round(time.Second), m.cache.Size(j.Key))
	m.finish(j, nil)
	m.cache.Evict(m.cfg.CacheMaxBytes)
}

func (m *Manager) finish(j *Job, err error) {
	m.mu.Lock()
	delete(m.running, j.Key)
	delete(m.jobs, j.Key)
	m.mu.Unlock()

	j.mu.Lock()
	j.running = false
	j.err = err
	j.mu.Unlock()

	close(j.done)
	m.nudge()
}

func (m *Manager) convert(ctx context.Context, j *Job) error {
	dst := m.cache.Path(j.Key)
	err := m.exec(ctx, j, dst, true)
	if err == nil || ctx.Err() != nil {
		return err
	}
	// Subtitle streams are the usual cause of a mux failure (mov_text has no
	// Matroska mapping, for one). Retry without them before giving up.
	log.Printf("transcode retry without subtitles: %s (%v)", j.Src, err)
	return m.exec(ctx, j, dst, false)
}

func (m *Manager) exec(ctx context.Context, j *Job, dst string, withSubs bool) error {
	cmd := exec.CommandContext(ctx, m.cfg.FFmpegBin, m.args(j.Plan, j.Src, dst, withSubs)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
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
