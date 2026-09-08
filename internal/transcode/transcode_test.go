package transcode

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nvt/internal/cache"
	"nvt/internal/config"
	"nvt/internal/probe"
)

// fakeFFmpeg writes a script that stays busy long enough to be caught in the
// act, so scheduling decisions can be observed rather than raced against.
func fakeFFmpeg(t *testing.T, seconds string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ffmpeg")
	body := "#!/bin/sh\nfor a in \"$@\"; do out=\"$a\"; done\n: > \"$out\"\nsleep " + seconds + "\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func newManager(t *testing.T, slots int, seconds string) (*Manager, *cache.Cache) {
	t.Helper()
	c, err := cache.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		FFmpegBin:     fakeFFmpeg(t, seconds),
		TranscodeJobs: slots,
		AudioCodec:    "aac",
		AudioBitrate:  "384k",
		CacheMaxBytes: 1 << 40,
	}
	return NewManager(cfg, c), c
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (m *Manager) find(src string) (Snapshot, bool) {
	for _, s := range m.Active() {
		if s.Src == src {
			return s, true
		}
	}
	return Snapshot{}, false
}

var audioPlan = probe.Plan{Action: probe.AudioOnly, Reason: "audio codec ac3"}

// A viewer pressing play must not wait behind a guess. With a single slot the
// only way to honour that is to stop the guess.
func TestPlaybackPreemptsRunningPrefetch(t *testing.T) {
	m, _ := newManager(t, 1, "10")

	m.Start("guess", "/media/guess.avi", "/dir", audioPlan, Prefetch)
	waitFor(t, "prefetch to start", func() bool {
		s, ok := m.find("/media/guess.avi")
		return ok && s.Running
	})

	m.Start("wanted", "/media/wanted.avi", "/dir", audioPlan, Playback)

	waitFor(t, "playback to take the slot", func() bool {
		s, ok := m.find("/media/wanted.avi")
		return ok && s.Running
	})

	s, ok := m.find("/media/guess.avi")
	if !ok {
		t.Fatal("preempted prefetch was discarded instead of requeued")
	}
	if s.Running {
		t.Error("preempted prefetch is still running")
	}
	if s.Priority != "prefetch" {
		t.Errorf("requeued job priority = %q, want prefetch", s.Priority)
	}
}

// Browsing away is a strong signal the speculation was wrong. Queued guesses
// for other directories should not keep the NAS busy.
func TestDropPendingOutsideDirectory(t *testing.T) {
	m, _ := newManager(t, 1, "10")

	m.Start("a1", "/media/A/1.avi", "/A", audioPlan, Prefetch)
	waitFor(t, "first job to start", func() bool {
		s, ok := m.find("/media/A/1.avi")
		return ok && s.Running
	})
	queued := m.Start("a2", "/media/A/2.avi", "/A", audioPlan, Prefetch)
	m.Start("b1", "/media/B/1.avi", "/B", audioPlan, Prefetch)

	if n := m.DropPendingOutside("/B"); n != 1 {
		t.Fatalf("dropped %d jobs, want 1", n)
	}

	select {
	case <-queued.Done():
		if !errors.Is(queued.Err(), ErrDropped) {
			t.Errorf("dropped job err = %v, want ErrDropped", queued.Err())
		}
	case <-time.After(time.Second):
		t.Error("dropped job never finished")
	}

	// The running job belongs to another directory but must survive: only
	// queued work is speculative enough to throw away.
	if s, ok := m.find("/media/A/1.avi"); !ok || !s.Running {
		t.Error("running job was dropped")
	}
	if _, ok := m.find("/media/B/1.avi"); !ok {
		t.Error("job for the current directory was dropped")
	}
}

// The same file requested twice is one conversion, and asking for it as
// playback upgrades the guess already in flight.
func TestStartDeduplicatesAndPromotes(t *testing.T) {
	m, _ := newManager(t, 1, "10")

	first := m.Start("k", "/media/x.avi", "/dir", audioPlan, Prefetch)
	second := m.Start("k", "/media/x.avi", "/dir", audioPlan, Playback)
	if first != second {
		t.Fatal("the same key produced two jobs")
	}
	if got := first.Priority(); got != Playback {
		t.Errorf("priority = %v, want Playback", got)
	}
	if n := len(m.Active()); n != 1 {
		t.Errorf("active jobs = %d, want 1", n)
	}
}

// With every slot busy, the queue must be ordered by priority, not arrival.
func TestQueueOrdersPlaybackFirst(t *testing.T) {
	m, _ := newManager(t, 1, "10")

	m.Start("busy", "/media/busy.avi", "/dir", audioPlan, Prefetch)
	waitFor(t, "slot to fill", func() bool {
		s, ok := m.find("/media/busy.avi")
		return ok && s.Running
	})
	m.Start("later-prefetch", "/media/p.avi", "/dir", audioPlan, Prefetch)
	m.Start("later-playback", "/media/q.avi", "/dir", audioPlan, Playback)

	// The playback job preempts, so it should be the one running shortly.
	waitFor(t, "playback to run before the queued prefetch", func() bool {
		s, ok := m.find("/media/q.avi")
		return ok && s.Running
	})
	if s, ok := m.find("/media/p.avi"); !ok || s.Running {
		t.Error("a queued prefetch ran ahead of a playback request")
	}
}

func TestCompletedOutputIsNotRequeued(t *testing.T) {
	m, c := newManager(t, 1, "0")

	job := m.Start("done", "/media/done.avi", "/dir", audioPlan, Playback)
	select {
	case <-job.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("job never finished")
	}
	if err := job.Err(); err != nil {
		t.Fatalf("job failed: %v", err)
	}
	if !c.Complete("done") {
		t.Fatal("cache entry not marked complete")
	}
	if m.Start("done", "/media/done.avi", "/dir", audioPlan, Playback) != nil {
		t.Error("a completed conversion was started again")
	}
}

func argsFor(t *testing.T, codec, bitrate string, action probe.Action) []string {
	t.Helper()
	m := &Manager{cfg: &config.Config{
		AudioCodec: codec, AudioBitrate: bitrate,
		VideoCodec: "libx264", VideoPreset: "veryfast", VideoCRF: "23",
	}}
	return m.args(probe.Plan{Action: action}, "/in.avi", "/out.mkv", true)
}

func joined(a []string) string { return " " + strings.Join(a, " ") + " " }

// A lossless target ignores a bitrate, and ffmpeg should not be handed one.
func TestArgsOmitBitrateForLosslessAudio(t *testing.T) {
	for _, codec := range []string{"flac", "alac", "pcm_s16le", "pcm_s24le"} {
		t.Run(codec, func(t *testing.T) {
			got := joined(argsFor(t, codec, "384k", probe.AudioOnly))
			if strings.Contains(got, " -b:a ") {
				t.Errorf("args carry a bitrate for lossless codec %s: %s", codec, got)
			}
			if !strings.Contains(got, " -c:a "+codec+" ") {
				t.Errorf("codec %s missing from args: %s", codec, got)
			}
		})
	}
}

func TestArgsKeepBitrateForLossyAudio(t *testing.T) {
	got := joined(argsFor(t, "aac", "192k", probe.AudioOnly))
	if !strings.Contains(got, " -b:a 192k ") {
		t.Errorf("lossy codec lost its bitrate: %s", got)
	}
}

// The audio-only plan must never re-encode video; that is the whole point of it.
func TestArgsCopyVideoForAudioPlan(t *testing.T) {
	got := joined(argsFor(t, "flac", "", probe.AudioOnly))
	if !strings.Contains(got, " -c:v copy ") {
		t.Errorf("audio plan is not copying video: %s", got)
	}
	if strings.Contains(got, "libx264") {
		t.Errorf("audio plan is invoking a video encoder: %s", got)
	}
}

func TestArgsEncodeVideoForVideoPlan(t *testing.T) {
	got := joined(argsFor(t, "flac", "", probe.FullTranscode))
	if !strings.Contains(got, " -c:v libx264 ") {
		t.Errorf("video plan is not encoding video: %s", got)
	}
}
