package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nvt/ver2/internal/ffmpeg"
	"nvt/ver2/internal/library"
	"nvt/ver2/internal/mediainfo"
	"nvt/ver2/internal/outpath"
)

// --- fakes ---

type fakeProber struct {
	info mediainfo.Info
	err  error
	// uncached makes the dispatcher unable to tell a copy from an encode,
	// which is what it faces for a file nothing has looked at yet.
	uncached bool
}

func (f fakeProber) Probe(context.Context, string, os.FileInfo) (mediainfo.Info, error) {
	return f.info, f.err
}

func (f fakeProber) Cached(string, os.FileInfo) (mediainfo.Info, bool) {
	if f.uncached || f.err != nil {
		return mediainfo.Info{}, false
	}
	return f.info, true
}

func h264Info(dur float64) mediainfo.Info {
	return mediainfo.Info{
		Duration: dur,
		Streams: []mediainfo.Stream{
			{Index: 0, Type: "video", Codec: "hevc"},
			{Index: 1, Type: "audio", Codec: "aac"},
		},
	}
}

type fakeRunner struct {
	fn func(ctx context.Context, spec ffmpeg.Spec, set ffmpeg.Settings, onProgress func(ffmpeg.Progress)) error
}

func (f fakeRunner) Run(ctx context.Context, spec ffmpeg.Spec, set ffmpeg.Settings, onProgress func(ffmpeg.Progress)) error {
	return f.fn(ctx, spec, set, onProgress)
}

// writeOutput is the minimum a runner must do to look successful.
func writeOutput(spec ffmpeg.Spec) error {
	return os.WriteFile(spec.Dst, []byte("converted"), 0o644)
}

// --- harness ---

type harness struct {
	q   *Queue
	m   *outpath.Mapper
	lib *library.Library
	src string
	out string
}

func newHarness(t *testing.T, runner ffmpeg.Runner, workers int, files ...string) *harness {
	t.Helper()
	base := t.TempDir()
	src := filepath.Join(base, "media")
	out := filepath.Join(base, "out")
	live := filepath.Join(base, "live")
	for _, d := range []string{src, out, live} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range files {
		p := filepath.Join(src, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("source"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m, err := outpath.NewMapper(src, out, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })

	q := NewQueue(Deps{
		Mapper: m, Prober: fakeProber{info: h264Info(100)}, Runner: runner,
		Workers: workers, LiveRoot: live, CheckpointAt: 0.10,
	})
	return &harness{q: q, m: m, lib: library.New(m), src: src, out: out}
}

func (h *harness) rel(t *testing.T, p string) outpath.Rel {
	t.Helper()
	r, err := h.m.ParseRel(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (h *harness) states() map[string]State {
	out := map[string]State{}
	for _, v := range h.q.Snapshot() {
		out[v.Name] = v.State
	}
	return out
}

// --- tests ---

func TestConvertsInTheGivenOrder(t *testing.T) {
	var mu sync.Mutex
	var order []string
	runner := fakeRunner{fn: func(_ context.Context, spec ffmpeg.Spec, _ ffmpeg.Settings, _ func(ffmpeg.Progress)) error {
		mu.Lock()
		order = append(order, filepath.Base(spec.Src))
		mu.Unlock()
		return writeOutput(spec)
	}}
	h := newHarness(t, runner, 1, "S/ep1.mkv", "S/ep2.mkv", "S/ep3.mkv")

	// Picking ep2 with "whole folder" should convert 2, 3, then 1.
	picked := h.rel(t, "S/ep2.mkv")
	if _, err := h.q.EnqueueDir(h.lib, picked, library.ScopeFolder, Options{}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "all three to finish", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == 3
	})
	want := []string{"ep2.mkv", "ep3.mkv", "ep1.mkv"}
	mu.Lock()
	defer mu.Unlock()
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("converted %v, want %v", order, want)
		}
	}
}

// The file exists only once it is complete: the rename is the commit.
func TestRenamesPartIntoPlaceOnlyOnSuccess(t *testing.T) {
	runner := fakeRunner{fn: func(_ context.Context, spec ffmpeg.Spec, _ ffmpeg.Settings, _ func(ffmpeg.Progress)) error {
		if !strings.HasSuffix(spec.Dst, ".part") {
			t.Errorf("ffmpeg was told to write %q directly", spec.Dst)
		}
		return writeOutput(spec)
	}}
	h := newHarness(t, runner, 1, "a.mkv")

	if _, err := h.q.Enqueue(outpath.Root(), []outpath.Rel{h.rel(t, "a.mkv")}, Options{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to finish", func() bool { return h.states()["a.mkv"] == Done })

	if _, err := os.Stat(filepath.Join(h.out, "a.mp4")); err != nil {
		t.Errorf("final output missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.out, "a.mp4.part")); !os.IsNotExist(err) {
		t.Error("the .part file survived a successful job")
	}
}

func TestFailureLeavesNoPartialOutput(t *testing.T) {
	runner := fakeRunner{fn: func(_ context.Context, spec ffmpeg.Spec, _ ffmpeg.Settings, _ func(ffmpeg.Progress)) error {
		os.WriteFile(spec.Dst, []byte("half"), 0o644)
		return errors.New("encoder exploded")
	}}
	h := newHarness(t, runner, 1, "a.mkv")

	if _, err := h.q.Enqueue(outpath.Root(), []outpath.Rel{h.rel(t, "a.mkv")}, Options{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to fail", func() bool { return h.states()["a.mkv"] == Failed })

	if _, err := os.Stat(filepath.Join(h.out, "a.mp4.part")); !os.IsNotExist(err) {
		t.Error("a .part was left behind after a failure")
	}
	if _, err := os.Stat(filepath.Join(h.out, "a.mp4")); !os.IsNotExist(err) {
		t.Error("a failed job produced an output file")
	}
	var found bool
	for _, v := range h.q.Snapshot() {
		if v.Name == "a.mkv" && strings.Contains(v.Error, "exploded") {
			found = true
		}
	}
	if !found {
		t.Error("the failure reason was not recorded")
	}
}

func TestSkipsFilesAlreadyConverted(t *testing.T) {
	var ran int
	var mu sync.Mutex
	runner := fakeRunner{fn: func(_ context.Context, spec ffmpeg.Spec, _ ffmpeg.Settings, _ func(ffmpeg.Progress)) error {
		mu.Lock()
		ran++
		mu.Unlock()
		return writeOutput(spec)
	}}
	h := newHarness(t, runner, 1, "S/ep1.mkv", "S/ep2.mkv")

	// ep1 is already there from a previous run.
	os.MkdirAll(filepath.Join(h.out, "S"), 0o755)
	os.WriteFile(filepath.Join(h.out, "S", "ep1.mp4"), []byte("old"), 0o644)

	b, err := h.q.EnqueueDir(h.lib, h.rel(t, "S/ep1.mkv"), library.ScopeFolder, Options{})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the batch to finish", func() bool {
		v, _ := h.q.BatchView(b.ID)
		return !v.Active
	})
	mu.Lock()
	defer mu.Unlock()
	if ran != 1 {
		t.Errorf("ran %d conversions, want 1: ep1 was already converted", ran)
	}
}

// Two sources in one directory can map to the same .mp4. Letting one overwrite
// the other silently is worse than refusing.
func TestRefusesAnOutputNameCollision(t *testing.T) {
	h := newHarness(t, fakeRunner{fn: func(_ context.Context, s ffmpeg.Spec, _ ffmpeg.Settings, _ func(ffmpeg.Progress)) error {
		return writeOutput(s)
	}}, 1, "S/ep1.mkv", "S/ep1.avi")

	_, err := h.q.EnqueueDir(h.lib, h.rel(t, "S/ep1.avi"), library.ScopeFolder, Options{})
	if !errors.Is(err, ErrOutputClash) {
		t.Fatalf("err = %v, want ErrOutputClash", err)
	}
	if got := len(h.q.Snapshot()); got != 0 {
		t.Errorf("%d jobs were created despite the clash", got)
	}
}

func TestEnqueueWithNothingToDo(t *testing.T) {
	h := newHarness(t, fakeRunner{}, 1, "a.mkv")
	os.WriteFile(filepath.Join(h.out, "a.mp4"), []byte("x"), 0o644)

	_, err := h.q.Enqueue(outpath.Root(), []outpath.Rel{h.rel(t, "a.mkv")}, Options{})
	if !errors.Is(err, ErrNothingToDo) {
		t.Errorf("err = %v, want ErrNothingToDo", err)
	}
}

func TestCancelAQueuedJob(t *testing.T) {
	block := make(chan struct{})
	runner := fakeRunner{fn: func(ctx context.Context, spec ffmpeg.Spec, _ ffmpeg.Settings, _ func(ffmpeg.Progress)) error {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
		return writeOutput(spec)
	}}
	h := newHarness(t, runner, 1, "S/ep1.mkv", "S/ep2.mkv")
	// Release the running job and let it settle before the test returns, or
	// its output write races the temporary directory being removed.
	defer func() {
		close(block)
		waitFor(t, "the running job to settle", func() bool { return h.states()["ep1.mkv"].Terminal() })
	}()

	b, err := h.q.EnqueueDir(h.lib, h.rel(t, "S/ep1.mkv"), library.ScopeFolder, Options{})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first job to start", func() bool { return h.states()["ep1.mkv"] == Running })

	v, _ := h.q.BatchView(b.ID)
	var second string
	for _, jv := range v.Jobs {
		if jv.Name == "ep2.mkv" {
			second = jv.ID
		}
	}
	if !h.q.Cancel(second) {
		t.Fatal("cancelling a queued job reported failure")
	}
	waitFor(t, "the second job to be cancelled", func() bool { return h.states()["ep2.mkv"] == Canceled })
	if h.states()["ep1.mkv"] != Running {
		t.Error("cancelling a queued job disturbed the running one")
	}
}

// The running encode is interrupted, not killed, so ffmpeg can close its files.
func TestCancelBatchStopsEverythingOutstanding(t *testing.T) {
	started := make(chan struct{}, 4)
	runner := fakeRunner{fn: func(ctx context.Context, spec ffmpeg.Spec, _ ffmpeg.Settings, _ func(ffmpeg.Progress)) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}}
	h := newHarness(t, runner, 1, "S/ep1.mkv", "S/ep2.mkv", "S/ep3.mkv")

	b, err := h.q.EnqueueDir(h.lib, h.rel(t, "S/ep1.mkv"), library.ScopeFolder, Options{})
	if err != nil {
		t.Fatal(err)
	}
	<-started

	if n := h.q.CancelBatch(b.ID); n != 3 {
		t.Errorf("cancelled %d jobs, want 3", n)
	}
	waitFor(t, "the batch to go quiet", func() bool {
		v, _ := h.q.BatchView(b.ID)
		return !v.Active
	})
	for _, v := range h.q.Snapshot() {
		if v.State != Canceled {
			t.Errorf("%s ended as %s, want canceled", v.Name, v.State)
		}
	}
}

// The point of the checkpoint is to offer a look at the first result before
// the remaining hours are spent — once, and only for the file being waited on.
func TestCheckpointFiresOnceOnTheFirstJob(t *testing.T) {
	release := make(chan struct{})
	runner := fakeRunner{fn: func(ctx context.Context, spec ffmpeg.Spec, _ ffmpeg.Settings, onProgress func(ffmpeg.Progress)) error {
		for _, sec := range []int{5, 15, 30, 60} {
			onProgress(ffmpeg.Progress{OutTime: time.Duration(sec) * time.Second})
		}
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return writeOutput(spec)
	}}
	h := newHarness(t, runner, 1, "S/ep1.mkv", "S/ep2.mkv")

	events, stop := h.q.Subscribe()
	defer stop()

	var mu sync.Mutex
	var checkpoints []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range events {
			if ev.Kind == "checkpoint" {
				mu.Lock()
				checkpoints = append(checkpoints, ev.Job.Name)
				mu.Unlock()
			}
		}
	}()

	if _, err := h.q.EnqueueDir(h.lib, h.rel(t, "S/ep1.mkv"), library.ScopeFolder, Options{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the checkpoint", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(checkpoints) > 0
	})

	close(release)
	waitFor(t, "both jobs to finish", func() bool {
		s := h.states()
		return s["ep1.mkv"] == Done && s["ep2.mkv"] == Done
	})
	stop()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(checkpoints) != 1 {
		t.Fatalf("checkpoints = %v, want exactly one", checkpoints)
	}
	if checkpoints[0] != "ep1.mkv" {
		t.Errorf("checkpoint fired on %q, want the first file of the batch", checkpoints[0])
	}
}

func TestWorkerLimit(t *testing.T) {
	var mu sync.Mutex
	var now, peak int
	block := make(chan struct{})
	runner := fakeRunner{fn: func(_ context.Context, spec ffmpeg.Spec, _ ffmpeg.Settings, _ func(ffmpeg.Progress)) error {
		mu.Lock()
		now++
		if now > peak {
			peak = now
		}
		mu.Unlock()
		<-block
		mu.Lock()
		now--
		mu.Unlock()
		return writeOutput(spec)
	}}
	h := newHarness(t, runner, 2, "S/a.mkv", "S/b.mkv", "S/c.mkv", "S/d.mkv")

	if _, err := h.q.EnqueueDir(h.lib, h.rel(t, "S/a.mkv"), library.ScopeFolder, Options{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both workers to be busy", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return now == 2
	})
	time.Sleep(50 * time.Millisecond)
	close(block)
	waitFor(t, "everything to finish", func() bool {
		for _, v := range h.q.Snapshot() {
			if !v.State.Terminal() {
				return false
			}
		}
		return true
	})

	mu.Lock()
	defer mu.Unlock()
	if peak > 2 {
		t.Errorf("ran %d jobs at once, want at most 2", peak)
	}
}

// A file that already holds H.264 and AAC only needs its container changed.
func TestRemuxIsChosenFromTheProbeNotTheExtension(t *testing.T) {
	var gotRemux bool
	runner := fakeRunner{fn: func(_ context.Context, spec ffmpeg.Spec, _ ffmpeg.Settings, _ func(ffmpeg.Progress)) error {
		gotRemux = spec.Remux
		return writeOutput(spec)
	}}
	h := newHarness(t, runner, 1, "a.mkv")
	h.q.prober = fakeProber{info: mediainfo.Info{
		Duration: 60,
		Streams: []mediainfo.Stream{
			{Index: 0, Type: "video", Codec: "h264"},
			{Index: 1, Type: "audio", Codec: "aac"},
		},
	}}

	if _, err := h.q.Enqueue(outpath.Root(), []outpath.Rel{h.rel(t, "a.mkv")}, Options{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to finish", func() bool { return h.states()["a.mkv"] == Done })
	if !gotRemux {
		t.Error("an h264+aac source was re-encoded instead of remuxed")
	}
}

// A resolver that hands back a fixed path, and records what it was asked for.
type fakeSubs struct {
	path      string
	err       error
	mu        sync.Mutex
	asked     []string
	langs     []string
	cleanedUp int
}

func (f *fakeSubs) Resolve(_ context.Context, _ string, _ outpath.Rel, preferred, lang string) (string, func(), error) {
	f.mu.Lock()
	f.asked = append(f.asked, preferred)
	f.langs = append(f.langs, lang)
	f.mu.Unlock()
	if f.err != nil {
		return "", func() {}, f.err
	}
	return f.path, func() { f.mu.Lock(); f.cleanedUp++; f.mu.Unlock() }, nil
}

func (f *fakeSubs) preferences() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

func h264Only() mediainfo.Info {
	return mediainfo.Info{
		Duration: 60,
		Streams: []mediainfo.Stream{
			{Index: 0, Type: "video", Codec: "h264"},
			{Index: 1, Type: "audio", Codec: "aac"},
		},
	}
}

// Burning subtitles redraws every frame, so the copy shortcut cannot apply.
func TestBurningSubtitlesDisablesRemux(t *testing.T) {
	var gotRemux bool
	var gotSubs string
	runner := fakeRunner{fn: func(_ context.Context, spec ffmpeg.Spec, _ ffmpeg.Settings, _ func(ffmpeg.Progress)) error {
		gotRemux, gotSubs = spec.Remux, spec.BurnSubs
		return writeOutput(spec)
	}}
	h := newHarness(t, runner, 1, "a.mkv")
	h.q.prober = fakeProber{info: h264Only()}
	resolver := &fakeSubs{path: "/tmp/x.ass"}
	h.q.subtitles = resolver

	if _, err := h.q.Enqueue(outpath.Root(), []outpath.Rel{h.rel(t, "a.mkv")},
		Options{Subtitles: true, Burn: true, PickedSubtitleID: "embedded:2"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to finish", func() bool { return h.states()["a.mkv"] == Done })
	if gotRemux {
		t.Error("subtitles were burned into a stream copy")
	}
	if gotSubs != "/tmp/x.ass" {
		t.Errorf("BurnSubs = %q", gotSubs)
	}
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	if resolver.cleanedUp != 1 {
		t.Errorf("the temporary subtitle was cleaned up %d times, want 1", resolver.cleanedUp)
	}
}

// Only the first file of a batch carries the viewer's choice. Nobody picks a
// track for episode 17 of a folder conversion, so the rest resolve their own.
func TestOnlyTheFirstJobUsesThePickedSubtitle(t *testing.T) {
	runner := fakeRunner{fn: func(_ context.Context, spec ffmpeg.Spec, _ ffmpeg.Settings, _ func(ffmpeg.Progress)) error {
		return writeOutput(spec)
	}}
	h := newHarness(t, runner, 1, "S/ep1.mkv", "S/ep2.mkv", "S/ep3.mkv")
	h.q.prober = fakeProber{info: h264Only()}
	resolver := &fakeSubs{path: "/tmp/x.ass"}
	h.q.subtitles = resolver

	files := []outpath.Rel{h.rel(t, "S/ep1.mkv"), h.rel(t, "S/ep2.mkv"), h.rel(t, "S/ep3.mkv")}
	if _, err := h.q.Enqueue(h.rel(t, "S"), files,
		Options{Subtitles: true, PickedSubtitleID: "embedded:4", SubtitleLang: "kor"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "all three", func() bool { return len(resolver.preferences()) == 3 })

	got := resolver.preferences()
	if got[0] != "embedded:4" {
		t.Errorf("first job asked for %q, want the picked track", got[0])
	}
	for _, p := range got[1:] {
		if p != "" {
			t.Errorf("a later job was given the picked track %q", p)
		}
	}

	// They are given the language instead, so they stay in it.
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	for i, l := range resolver.langs {
		if l != "kor" {
			t.Errorf("job %d was asked for language %q, want kor", i, l)
		}
	}
}

func TestSubtitlesOffMeansTheResolverIsNotConsulted(t *testing.T) {
	runner := fakeRunner{fn: func(_ context.Context, spec ffmpeg.Spec, _ ffmpeg.Settings, _ func(ffmpeg.Progress)) error {
		if spec.BurnSubs != "" {
			t.Errorf("subtitles were burned without being asked for: %q", spec.BurnSubs)
		}
		return writeOutput(spec)
	}}
	h := newHarness(t, runner, 1, "a.mkv")
	resolver := &fakeSubs{path: "/tmp/x.ass"}
	h.q.subtitles = resolver

	if _, err := h.q.Enqueue(outpath.Root(), []outpath.Rel{h.rel(t, "a.mkv")}, Options{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to finish", func() bool { return h.states()["a.mkv"] == Done })
	if n := len(resolver.preferences()); n != 0 {
		t.Errorf("the resolver was consulted %d times with subtitles off", n)
	}
}

// A subtitle that cannot be prepared fails the job rather than silently
// producing a file without the subtitles that were asked for.
func TestSubtitleFailureFailsTheJob(t *testing.T) {
	runner := fakeRunner{fn: func(_ context.Context, spec ffmpeg.Spec, _ ffmpeg.Settings, _ func(ffmpeg.Progress)) error {
		t.Error("the conversion ran despite the subtitle failing")
		return writeOutput(spec)
	}}
	h := newHarness(t, runner, 1, "a.mkv")
	h.q.subtitles = &fakeSubs{err: errors.New("cp949 decode failed")}

	if _, err := h.q.Enqueue(outpath.Root(), []outpath.Rel{h.rel(t, "a.mkv")},
		Options{Subtitles: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to fail", func() bool { return h.states()["a.mkv"] == Failed })
}

func TestProgressReachesTheView(t *testing.T) {
	release := make(chan struct{})
	runner := fakeRunner{fn: func(ctx context.Context, spec ffmpeg.Spec, _ ffmpeg.Settings, onProgress func(ffmpeg.Progress)) error {
		onProgress(ffmpeg.Progress{OutTime: 25 * time.Second, FPS: 17})
		<-release
		return writeOutput(spec)
	}}
	h := newHarness(t, runner, 1, "a.mkv") // fake prober reports a 100s duration
	// Let the job finish before the test returns, or its final write races
	// with the temporary directory being removed.
	defer func() {
		close(release)
		waitFor(t, "the job to finish", func() bool { return h.states()["a.mkv"].Terminal() })
	}()

	if _, err := h.q.Enqueue(outpath.Root(), []outpath.Rel{h.rel(t, "a.mkv")}, Options{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "progress to be visible", func() bool {
		for _, v := range h.q.Snapshot() {
			if v.Name == "a.mkv" && v.Percent > 0.2 {
				return true
			}
		}
		return false
	})
	for _, v := range h.q.Snapshot() {
		if v.Name == "a.mkv" {
			if v.Percent < 0.24 || v.Percent > 0.26 {
				t.Errorf("Percent = %v, want about 0.25", v.Percent)
			}
			if v.FPS != 17 {
				t.Errorf("FPS = %v", v.FPS)
			}
		}
	}
}

// Sweep exists because a .part has no index: nothing can ever use one.
func TestSweepRemovesInterruptedOutput(t *testing.T) {
	base := t.TempDir()
	out := filepath.Join(base, "out", "S")
	live := filepath.Join(base, "live", "job1")
	os.MkdirAll(out, 0o755)
	os.MkdirAll(live, 0o755)
	os.WriteFile(filepath.Join(out, "good.mp4"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(out, "bad.mp4.part"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(live, "seg0.m4s"), []byte("x"), 0o644)

	Sweep(filepath.Join(base, "out"), filepath.Join(base, "live"))

	if _, err := os.Stat(filepath.Join(out, "bad.mp4.part")); !os.IsNotExist(err) {
		t.Error(".part survived the sweep")
	}
	if _, err := os.Stat(filepath.Join(out, "good.mp4")); err != nil {
		t.Error("a finished file was swept away")
	}
	if _, err := os.Stat(live); !os.IsNotExist(err) {
		t.Error("a stale live job directory survived the sweep")
	}
	// The root itself is recreated, ready for the next job.
	if ents, err := os.ReadDir(filepath.Join(base, "live")); err != nil || len(ents) != 0 {
		t.Errorf("live root = %v, %v; want an empty directory", ents, err)
	}
}

// The preview exists for the checkpoint — one early look at the subtitles and
// the picture — and that look happens on the first file. Every later one would
// write segments nobody opens, and pay for them in quality: the HLS branch
// forces a keyframe every few seconds.
func TestOnlyTheFirstFileGetsAPreview(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	liveDirs := map[string]string{}

	runner := fakeRunner{fn: func(ctx context.Context, spec ffmpeg.Spec, _ ffmpeg.Settings, _ func(ffmpeg.Progress)) error {
		mu.Lock()
		liveDirs[filepath.Base(spec.Src)] = spec.LiveDir
		mu.Unlock()
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return writeOutput(spec)
	}}
	h := newHarness(t, runner, 1, "S/ep1.mkv", "S/ep2.mkv", "S/ep3.mkv")

	if _, err := h.q.EnqueueDir(h.lib, h.rel(t, "S/ep1.mkv"), library.ScopeFolder, Options{Live: true}); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitFor(t, "every job to finish", func() bool {
		s := h.states()
		return s["ep1.mkv"] == Done && s["ep2.mkv"] == Done && s["ep3.mkv"] == Done
	})

	mu.Lock()
	defer mu.Unlock()
	if liveDirs["ep1.mkv"] == "" {
		t.Error("the first file had no preview, so there is nothing to check at the checkpoint")
	}
	for _, name := range []string{"ep2.mkv", "ep3.mkv"} {
		if liveDirs[name] != "" {
			t.Errorf("%s wrote a preview nobody asked for: %q", name, liveDirs[name])
		}
	}

	// The view has to agree, or the page offers a player for a stream that was
	// never written.
	for _, v := range h.q.Snapshot() {
		if v.Name != "ep1.mkv" && v.Live {
			t.Errorf("%s reports a live preview it does not have", v.Name)
		}
	}
}

// Carrying the subtitle as a track of its own leaves the picture alone, so a
// file that only needed its container swapped still only needs that — seconds
// rather than the hour burning would cost.
func TestASoftSubtitleKeepsTheRemuxShortcut(t *testing.T) {
	var gotRemux bool
	var gotBurn, gotSoft, gotLang string
	runner := fakeRunner{fn: func(_ context.Context, spec ffmpeg.Spec, _ ffmpeg.Settings, _ func(ffmpeg.Progress)) error {
		gotRemux, gotBurn, gotSoft, gotLang = spec.Remux, spec.BurnSubs, spec.SoftSubs, spec.SubsLang
		return writeOutput(spec)
	}}
	h := newHarness(t, runner, 1, "a.mkv")
	h.q.prober = fakeProber{info: h264Only()}
	h.q.subtitles = &fakeSubs{path: "/tmp/x.ass"}

	if _, err := h.q.Enqueue(outpath.Root(), []outpath.Rel{h.rel(t, "a.mkv")},
		Options{Subtitles: true, PickedSubtitleID: "embedded:2", SubtitleLang: "kor"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to finish", func() bool { return h.states()["a.mkv"] == Done })

	if !gotRemux {
		t.Error("adding a subtitle track threw away the stream copy")
	}
	if gotSoft != "/tmp/x.ass" {
		t.Errorf("SoftSubs = %q, want the prepared file", gotSoft)
	}
	if gotBurn != "" {
		t.Errorf("BurnSubs = %q, want nothing drawn into the picture", gotBurn)
	}
	if gotLang != "kor" {
		t.Errorf("SubsLang = %q, so a player would not name the track", gotLang)
	}
}

// Copying an episode's streams takes seconds; re-encoding one takes forty
// minutes. Making the first wait behind the second wastes the case the queue
// is quickest at — someone adding subtitles to something they want to watch
// tonight.
func TestCopyOnlyJobsGoFirst(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var order []string

	runner := fakeRunner{fn: func(ctx context.Context, spec ffmpeg.Spec, _ ffmpeg.Settings, _ func(ffmpeg.Progress)) error {
		mu.Lock()
		order = append(order, filepath.Base(spec.Src))
		mu.Unlock()
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return writeOutput(spec)
	}}

	// One worker, and a job already running, so everything else queues up.
	h := newHarness(t, runner, 1, "S/hevc1.mkv", "S/hevc2.mkv", "S/copy.mkv")

	// Two sources that need encoding and one that only needs its container
	// swapped. The prober here answers for every path, so the distinction
	// comes from the options: burning forces an encode.
	h.q.prober = fakeProber{info: h264Only()}
	h.q.subtitles = &fakeSubs{path: "/tmp/x.ass"}

	if _, err := h.q.Enqueue(outpath.Root(), []outpath.Rel{
		h.rel(t, "S/hevc1.mkv"), h.rel(t, "S/hevc2.mkv"),
	}, Options{Subtitles: true, Burn: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first job to start", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == 1
	})

	// Now a copy-only job arrives behind an encode that has not started.
	if _, err := h.q.Enqueue(outpath.Root(), []outpath.Rel{h.rel(t, "S/copy.mkv")},
		Options{}); err != nil {
		t.Fatal(err)
	}

	close(release)
	waitFor(t, "everything to finish", func() bool {
		s := h.states()
		return s["hevc1.mkv"] == Done && s["hevc2.mkv"] == Done && s["copy.mkv"] == Done
	})

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 {
		t.Fatalf("ran %v", order)
	}
	// The running job is never disturbed — this is ordering, not preemption.
	if order[0] != "hevc1.mkv" {
		t.Errorf("the running job was displaced: %v", order)
	}
	if order[1] != "copy.mkv" {
		t.Errorf("the copy waited behind an encode: %v", order)
	}
}

// A file nothing has looked at yet cannot be told apart from an encode
// without opening it, and opening every queued file to sort them would cost
// more than the sorting saves.
func TestAnUnknownFileWaitsItsTurn(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var order []string
	runner := fakeRunner{fn: func(ctx context.Context, spec ffmpeg.Spec, _ ffmpeg.Settings, _ func(ffmpeg.Progress)) error {
		mu.Lock()
		order = append(order, filepath.Base(spec.Src))
		mu.Unlock()
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return writeOutput(spec)
	}}
	h := newHarness(t, runner, 1, "a.mkv", "b.mkv", "c.mkv")
	h.q.prober = fakeProber{info: h264Only(), uncached: true}

	if _, err := h.q.Enqueue(outpath.Root(), []outpath.Rel{
		h.rel(t, "a.mkv"), h.rel(t, "b.mkv"), h.rel(t, "c.mkv"),
	}, Options{}); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitFor(t, "everything to finish", func() bool {
		s := h.states()
		return s["a.mkv"] == Done && s["b.mkv"] == Done && s["c.mkv"] == Done
	})

	mu.Lock()
	defer mu.Unlock()
	want := []string{"a.mkv", "b.mkv", "c.mkv"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want arrival order %v", order, want)
		}
	}
}
