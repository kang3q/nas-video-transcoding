package jobs

import (
	"context"
	"encoding/json"
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

// A restart is not an unusual event over a run that takes all night — a NAS
// reboot, a container update, a power cut. What was queued has to come back.
type restartable struct {
	base, src, out, state string
	mapper                *outpath.Mapper
	lib                   *library.Library
}

func newRestartable(t *testing.T, files ...string) *restartable {
	t.Helper()
	base := t.TempDir()
	r := &restartable{
		base:  base,
		src:   filepath.Join(base, "media"),
		out:   filepath.Join(base, "out"),
		state: filepath.Join(base, "state"),
	}
	for _, d := range []string{r.src, r.out, r.state} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range files {
		p := filepath.Join(r.src, filepath.FromSlash(f))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte("source"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m, err := outpath.NewMapper(r.src, r.out, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	r.mapper = m
	r.lib = library.New(m)
	return r
}

// start builds a queue as if the process had just come up.
func (r *restartable) start(t *testing.T, runner ffmpeg.Runner) *Queue {
	t.Helper()
	return NewQueue(Deps{
		Mapper: r.mapper, Prober: fakeProber{info: h264Info(100)}, Runner: runner,
		Workers: 1, LiveRoot: filepath.Join(r.state, "live"),
		StateDir: r.state, CheckpointAt: 0.10,
	})
}

func (r *restartable) rel(t *testing.T, p string) outpath.Rel {
	t.Helper()
	rel, err := r.mapper.ParseRel(p)
	if err != nil {
		t.Fatal(err)
	}
	return rel
}

func (r *restartable) stateJSON(t *testing.T) persisted {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join(r.state, "queue.json"))
	if err != nil {
		t.Fatalf("no queue state was written: %v", err)
	}
	var p persisted
	if err := json.Unmarshal(blob, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// recorder collects what the runner saw. The runner is on another goroutine,
// so this cannot be a bare slice.
type recorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *recorder) runner() ffmpeg.Runner {
	return fakeRunner{fn: func(_ context.Context, spec ffmpeg.Spec, _ ffmpeg.Settings, _ func(ffmpeg.Progress)) error {
		r.mu.Lock()
		r.seen = append(r.seen, filepath.Base(spec.Src))
		r.mu.Unlock()
		return os.WriteFile(spec.Dst, []byte("converted"), 0o644)
	}}
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

// blocker runs one job forever until released, so a shutdown can be staged
// mid-conversion.
type blocker struct {
	started chan string
	release chan struct{}
}

func newBlocker() *blocker {
	return &blocker{started: make(chan string, 8), release: make(chan struct{})}
}

func (b *blocker) Run(ctx context.Context, spec ffmpeg.Spec, _ ffmpeg.Settings, _ func(ffmpeg.Progress)) error {
	b.started <- filepath.Base(spec.Src)
	select {
	case <-b.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return os.WriteFile(spec.Dst, []byte("converted"), 0o644)
}

func TestQueueSurvivesARestart(t *testing.T) {
	r := newRestartable(t, "S/ep1.mkv", "S/ep2.mkv", "S/ep3.mkv")
	b := newBlocker()

	q := r.start(t, b)
	if _, err := q.EnqueueDir(r.lib, r.rel(t, "S/ep1.mkv"), library.ScopeFolder, Options{}); err != nil {
		t.Fatal(err)
	}
	<-b.started // ep1 is now encoding; ep2 and ep3 are waiting

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	q.Shutdown(ctx)
	cancel()

	// The one that was running counts as outstanding: its progress is gone
	// with the process, so it has to be done again — and first.
	state := r.stateJSON(t)
	if len(state.Batches) != 1 {
		t.Fatalf("saved %d batches, want 1", len(state.Batches))
	}
	want := []string{"S/ep1.mkv", "S/ep2.mkv", "S/ep3.mkv"}
	got := state.Batches[0].Rels
	if len(got) != len(want) {
		t.Fatalf("saved %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("saved %v, want %v", got, want)
		}
	}

	// Come back up.
	rec := &recorder{}
	q2 := r.start(t, rec.runner())
	q2.Restore()

	waitFor(t, "the resumed batch to finish", func() bool { return len(rec.list()) == 3 })
	done := rec.list()
	for i := range want {
		if done[i] != filepath.Base(want[i]) {
			t.Fatalf("resumed as %v, want the original order", done)
		}
	}
}

// Anything converted before the restart is not done twice.
func TestRestoreSkipsWhatIsAlreadyConverted(t *testing.T) {
	r := newRestartable(t, "S/ep1.mkv", "S/ep2.mkv")
	b := newBlocker()

	q := r.start(t, b)
	q.EnqueueDir(r.lib, r.rel(t, "S/ep1.mkv"), library.ScopeFolder, Options{})
	<-b.started

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	q.Shutdown(ctx)
	cancel()

	// ep1 was finished by some other means while we were down.
	os.MkdirAll(filepath.Join(r.out, "S"), 0o755)
	os.WriteFile(filepath.Join(r.out, "S", "ep1.mp4"), []byte("x"), 0o644)

	rec := &recorder{}
	q2 := r.start(t, rec.runner())
	q2.Restore()

	waitFor(t, "the remaining file", func() bool { return len(rec.list()) == 1 })
	time.Sleep(100 * time.Millisecond)
	if done := rec.list(); len(done) != 1 || done[0] != "ep2.mkv" {
		t.Errorf("converted %v, want only ep2.mkv", done)
	}
}

// The batch keeps its id, so a stop button pressed from a notification sent
// before the restart still finds the batch it names.
func TestRestoreKeepsTheBatchIdentity(t *testing.T) {
	r := newRestartable(t, "S/ep1.mkv", "S/ep2.mkv")
	b := newBlocker()

	q := r.start(t, b)
	batch, err := q.EnqueueDir(r.lib, r.rel(t, "S/ep1.mkv"), library.ScopeFolder, Options{})
	if err != nil {
		t.Fatal(err)
	}
	<-b.started

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	q.Shutdown(ctx)
	cancel()

	b2 := newBlocker()
	q2 := r.start(t, b2)
	q2.Restore()
	<-b2.started

	if _, ok := q2.BatchView(batch.ID); !ok {
		t.Fatalf("batch %s did not come back under the same id", batch.ID)
	}
	if n := q2.CancelBatch(batch.ID); n == 0 {
		t.Error("the old batch id no longer stops anything")
	}
}

// A prompt already sent must not be sent again just because the process
// restarted.
func TestRestoreDoesNotRepeatTheCheckpoint(t *testing.T) {
	r := newRestartable(t, "S/ep1.mkv", "S/ep2.mkv")

	release := make(chan struct{})
	q := r.start(t, fakeRunner{fn: func(ctx context.Context, spec ffmpeg.Spec, _ ffmpeg.Settings, onProgress func(ffmpeg.Progress)) error {
		onProgress(ffmpeg.Progress{OutTime: 30 * time.Second}) // 30% of 100s
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return os.WriteFile(spec.Dst, []byte("x"), 0o644)
	}})

	events, stop := q.Subscribe()
	go func() {
		for range events {
		}
	}()
	q.EnqueueDir(r.lib, r.rel(t, "S/ep1.mkv"), library.ScopeFolder, Options{})
	waitFor(t, "the checkpoint to fire", func() bool {
		return r.stateExists() && r.checkpointSaved(t)
	})
	stop()
	close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	q.Shutdown(ctx)
	cancel()

	q2 := r.start(t, fakeRunner{fn: func(_ context.Context, spec ffmpeg.Spec, _ ffmpeg.Settings, _ func(ffmpeg.Progress)) error {
		return os.WriteFile(spec.Dst, []byte("x"), 0o644)
	}})
	ev2, stop2 := q2.Subscribe()
	defer stop2()
	var mu sync.Mutex
	var checkpoints int
	go func() {
		for e := range ev2 {
			if e.Kind == "checkpoint" {
				mu.Lock()
				checkpoints++
				mu.Unlock()
			}
		}
	}()
	q2.Restore()
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if checkpoints != 0 {
		t.Errorf("the checkpoint fired again after a restart (%d times)", checkpoints)
	}
}

func (r *restartable) stateExists() bool {
	_, err := os.Stat(filepath.Join(r.state, "queue.json"))
	return err == nil
}

func (r *restartable) checkpointSaved(t *testing.T) bool {
	blob, err := os.ReadFile(filepath.Join(r.state, "queue.json"))
	if err != nil {
		return false
	}
	return strings.Contains(string(blob), `"checkpoint": true`)
}

// With nothing left to do the file goes away, so a clean start is really clean.
func TestStateFileRemovedWhenEverythingIsDone(t *testing.T) {
	r := newRestartable(t, "a.mkv")
	q := r.start(t, fakeRunner{fn: func(_ context.Context, spec ffmpeg.Spec, _ ffmpeg.Settings, _ func(ffmpeg.Progress)) error {
		return os.WriteFile(spec.Dst, []byte("x"), 0o644)
	}})
	q.Enqueue(outpath.Root(), []outpath.Rel{r.rel(t, "a.mkv")}, Options{})

	waitFor(t, "the queue to empty", func() bool { return !r.stateExists() })
}

func TestRestoreWithNoStateIsQuiet(t *testing.T) {
	r := newRestartable(t, "a.mkv")
	q := r.start(t, fakeRunner{})
	q.Restore() // must not panic or invent work
	if n := len(q.Snapshot()); n != 0 {
		t.Errorf("restored %d jobs from nothing", n)
	}
}

func TestRestoreIgnoresStateFromAnotherVersion(t *testing.T) {
	r := newRestartable(t, "a.mkv")
	os.WriteFile(filepath.Join(r.state, "queue.json"),
		[]byte(`{"version":999,"batches":[{"id":"b","dir":"","rels":["a.mkv"]}]}`), 0o644)

	q := r.start(t, fakeRunner{})
	q.Restore()
	if n := len(q.Snapshot()); n != 0 {
		t.Errorf("restored %d jobs from an unreadable state file", n)
	}
}

// A source deleted while the service was down is not queued for conversion.
func TestRestoreSkipsMissingSources(t *testing.T) {
	r := newRestartable(t, "a.mkv", "b.mkv")
	os.WriteFile(filepath.Join(r.state, "queue.json"),
		[]byte(`{"version":1,"batches":[{"id":"b1","dir":"","rels":["a.mkv","gone.mkv","b.mkv"]}]}`), 0o644)

	rec := &recorder{}
	q := r.start(t, rec.runner())
	q.Restore()

	waitFor(t, "both surviving files", func() bool { return len(rec.list()) == 2 })
	time.Sleep(100 * time.Millisecond)
	if done := rec.list(); len(done) != 2 {
		t.Errorf("converted %v, want only the two that still exist", done)
	}
}

var _ = mediainfo.Info{}
