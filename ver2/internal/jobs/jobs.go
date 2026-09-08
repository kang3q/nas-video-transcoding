// Package jobs runs conversions in order and reports on them.
//
// It is deliberately much smaller than v1's scheduler. v1 arbitrated between
// work a viewer had asked for and work it had guessed at, which needed
// priorities and preemption. Here every job exists because someone clicked it,
// so the only ordering that matters is the one they chose — and preemption
// would be actively harmful, since killing a running job throws away up to an
// hour of encoding.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"nvt/ver2/internal/ffmpeg"
	"nvt/ver2/internal/library"
	"nvt/ver2/internal/mediainfo"
	"nvt/ver2/internal/outpath"
)

type State string

const (
	Queued   State = "queued"
	Running  State = "running"
	Done     State = "done"
	Failed   State = "failed"
	Canceled State = "canceled"
)

func (s State) Terminal() bool { return s == Done || s == Failed || s == Canceled }

var (
	ErrNothingToDo   = errors.New("jobs: nothing to convert")
	ErrOutputClash   = errors.New("jobs: two sources want the same output name")
	ErrAlreadyQueued = errors.New("jobs: already queued")
)

// Options are the choices made when a batch is started.
type Options struct {
	// BurnSubs is a subtitle file to draw into the picture. Already copied to
	// a safe, simple path by the caller.
	BurnSubs string
	// Live also produces an HLS rendition, so the job can be watched before it
	// finishes. Costs a little disk and almost no CPU: the encode happens once
	// either way.
	Live bool
}

type Job struct {
	ID      string
	BatchID string
	Rel     outpath.Rel
	Name    string

	src, part, dst, liveDir string
	opts                    Options

	mu        sync.Mutex
	state     State
	remux     bool
	duration  time.Duration
	out       time.Duration
	fps       float64
	errText   string
	speed     *ffmpeg.SpeedTracker
	queuedAt  time.Time
	startedAt time.Time
	endedAt   time.Time
	cancel    context.CancelFunc
}

// View is a snapshot safe to render or serialise while the job keeps running.
type View struct {
	ID       string  `json:"id"`
	BatchID  string  `json:"batch_id"`
	Name     string  `json:"name"`
	Rel      string  `json:"rel"`
	State    State   `json:"state"`
	Remux    bool    `json:"remux"`
	Percent  float64 `json:"percent"` // 0..1, or -1 when the duration is unknown
	Rate     float64 `json:"rate"`    // media-seconds per wall-second
	FPS      float64 `json:"fps"`
	Duration float64 `json:"duration_sec"`
	Out      float64 `json:"out_sec"`
	ETASec   float64 `json:"eta_sec"` // -1 when unknown
	Elapsed  float64 `json:"elapsed_sec"`
	Error    string  `json:"error,omitempty"`
	Live     bool    `json:"live"`
}

func (j *Job) View() View {
	j.mu.Lock()
	defer j.mu.Unlock()

	v := View{
		ID: j.ID, BatchID: j.BatchID, Name: j.Name, Rel: j.Rel.String(),
		State: j.state, Remux: j.remux, Error: j.errText,
		Duration: j.duration.Seconds(), Out: j.out.Seconds(),
		FPS: j.fps, Live: j.opts.Live,
		Percent: ffmpeg.Percent(j.out, j.duration, j.state == Done),
		ETASec:  -1,
	}
	if j.speed != nil {
		v.Rate = j.speed.Rate()
		if eta := j.speed.ETA(j.out, j.duration); eta > 0 {
			v.ETASec = eta.Seconds()
		}
	}
	switch {
	case j.state == Queued:
	case j.endedAt.IsZero():
		v.Elapsed = time.Since(j.startedAt).Seconds()
	default:
		v.Elapsed = j.endedAt.Sub(j.startedAt).Seconds()
	}
	return v
}

type Batch struct {
	ID      string
	Dir     outpath.Rel
	Created time.Time

	mu         sync.Mutex
	jobIDs     []string
	checkpoint bool // the inspection prompt has been sent
}

type BatchView struct {
	ID      string `json:"id"`
	Dir     string `json:"dir"`
	Jobs    []View `json:"jobs"`
	Pending int    `json:"pending"`
	Done    int    `json:"done"`
	Failed  int    `json:"failed"`
	Active  bool   `json:"active"`
}

// Event is what the UI and the notifier listen to.
type Event struct {
	Kind    string `json:"kind"` // progress, state, checkpoint
	Job     View   `json:"job"`
	BatchID string `json:"batch_id"`
}

// Prober is the part of mediainfo the queue needs. Keeping it an interface is
// what lets the scheduling be tested without ffprobe on the machine.
type Prober interface {
	Probe(ctx context.Context, path string, fi os.FileInfo) (mediainfo.Info, error)
}

type Queue struct {
	mapper   *outpath.Mapper
	prober   Prober
	runner   ffmpeg.Runner
	settings ffmpeg.Settings
	workers  int
	liveRoot string
	// checkpointAt is the fraction of the first file in a batch to reach
	// before asking whether the result looks right. Far enough in that an
	// opening sequence is over and burned-in subtitles are on screen.
	checkpointAt float64

	mu       sync.Mutex
	jobs     map[string]*Job
	batches  map[string]*Batch
	pending  []string
	running  map[string]*Job
	byOutput map[string]string
	finished []string // bounded history, newest last

	wake chan struct{}

	submu sync.Mutex
	subs  map[chan Event]struct{}
}

type Deps struct {
	Mapper       *outpath.Mapper
	Prober       Prober
	Runner       ffmpeg.Runner
	Settings     ffmpeg.Settings
	Workers      int
	LiveRoot     string
	CheckpointAt float64
}

func NewQueue(d Deps) *Queue {
	if d.Workers < 1 {
		d.Workers = 1
	}
	if d.CheckpointAt <= 0 || d.CheckpointAt >= 1 {
		d.CheckpointAt = 0.10
	}
	q := &Queue{
		mapper: d.Mapper, prober: d.Prober, runner: d.Runner,
		settings: d.Settings, workers: d.Workers,
		liveRoot: d.LiveRoot, checkpointAt: d.CheckpointAt,
		jobs: map[string]*Job{}, batches: map[string]*Batch{},
		running: map[string]*Job{}, byOutput: map[string]string{},
		wake: make(chan struct{}, 1),
		subs: map[chan Event]struct{}{},
	}
	go q.dispatchLoop()
	return q
}

// --- enqueue ---

// Enqueue creates a batch from an ordered list of files. The order is the
// caller's: library.Order has already put the file that was clicked first.
//
// Files whose output already exists are skipped rather than redone, and two
// files that would produce the same output name are refused outright — one
// silently overwriting the other is the worse outcome.
func (q *Queue) Enqueue(dir outpath.Rel, rels []outpath.Rel, opts Options) (*Batch, error) {
	batch := &Batch{ID: newID(), Dir: dir, Created: time.Now()}

	// Check the whole list before creating anything. Rejecting halfway would
	// leave the accepted half already converting.
	wanted := map[string]outpath.Rel{}
	for _, rel := range rels {
		dst := q.mapper.Output(rel)
		if prev, clash := wanted[dst]; clash {
			return nil, fmt.Errorf("%w: %s and %s both become %s",
				ErrOutputClash, prev.Base(), rel.Base(), filepath.Base(dst))
		}
		wanted[dst] = rel
	}

	q.mu.Lock()
	var created []*Job
	for _, rel := range rels {
		dst := q.mapper.Output(rel)

		if _, err := os.Stat(dst); err == nil {
			continue // already converted
		}
		if id, busy := q.byOutput[dst]; busy {
			if j, ok := q.jobs[id]; ok && !j.State().Terminal() {
				continue // already on its way
			}
		}

		j := &Job{
			ID: newID(), BatchID: batch.ID, Rel: rel, Name: rel.Base(),
			src: q.mapper.Source(rel), part: q.mapper.Partial(rel), dst: dst,
			opts: opts, state: Queued, queuedAt: time.Now(),
			speed: ffmpeg.NewSpeedTracker(30 * time.Second),
		}
		if opts.Live {
			j.liveDir = filepath.Join(q.liveRoot, j.ID)
		}
		q.jobs[j.ID] = j
		q.byOutput[dst] = j.ID
		q.pending = append(q.pending, j.ID)
		batch.jobIDs = append(batch.jobIDs, j.ID)
		created = append(created, j)
	}

	if len(created) == 0 {
		q.mu.Unlock()
		return nil, ErrNothingToDo
	}
	q.batches[batch.ID] = batch
	q.mu.Unlock()

	for _, j := range created {
		q.emit(Event{Kind: "state", Job: j.View(), BatchID: batch.ID})
	}
	q.nudge()
	return batch, nil
}

// --- cancel ---

// Cancel stops one job. A queued job simply never starts; a running one is
// interrupted so ffmpeg can close its files, and the partial output is removed.
func (q *Queue) Cancel(id string) bool {
	q.mu.Lock()
	j, ok := q.jobs[id]
	if !ok {
		q.mu.Unlock()
		return false
	}
	if j.State() == Queued {
		q.removePendingLocked(id)
		// A job cancelled before it ran never reaches the dispatcher, so it
		// has to be recorded here or it vanishes from the listing entirely.
		q.finishedLocked(id)
		q.mu.Unlock()
		j.finish(Canceled, "")
		os.Remove(j.part)
		q.release(j)
		q.emit(Event{Kind: "state", Job: j.View(), BatchID: j.BatchID})
		q.nudge()
		return true
	}
	cancel := j.cancelFunc()
	q.mu.Unlock()
	if cancel != nil {
		cancel()
		return true
	}
	return false
}

// CancelBatch stops everything still outstanding in a batch. This is what the
// inspection prompt's "stop" button reaches: the point of looking at the first
// result is being able to call off the rest.
func (q *Queue) CancelBatch(batchID string) int {
	q.mu.Lock()
	b, ok := q.batches[batchID]
	if !ok {
		q.mu.Unlock()
		return 0
	}
	ids := append([]string(nil), b.jobIDs...)
	q.mu.Unlock()

	n := 0
	for _, id := range ids {
		if j, ok := q.job(id); ok && !j.State().Terminal() {
			if q.Cancel(id) {
				n++
			}
		}
	}
	return n
}

// --- reading ---

func (q *Queue) job(id string) (*Job, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, ok := q.jobs[id]
	return j, ok
}

func (j *Job) State() State {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state
}

func (j *Job) cancelFunc() context.CancelFunc {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.cancel
}

// Snapshot lists running jobs first, then the queue in order, then recent
// history — which is the order they matter in.
func (q *Queue) Snapshot() []View {
	q.mu.Lock()
	running := make([]*Job, 0, len(q.running))
	for _, j := range q.running {
		running = append(running, j)
	}
	pending := make([]*Job, 0, len(q.pending))
	for _, id := range q.pending {
		if j, ok := q.jobs[id]; ok {
			pending = append(pending, j)
		}
	}
	finished := make([]*Job, 0, len(q.finished))
	for _, id := range q.finished {
		if j, ok := q.jobs[id]; ok {
			finished = append(finished, j)
		}
	}
	q.mu.Unlock()

	sort.Slice(running, func(i, k int) bool {
		return running[i].startedAt.Before(running[k].startedAt)
	})

	out := make([]View, 0, len(running)+len(pending)+len(finished))
	for _, set := range [][]*Job{running, pending, finished} {
		for _, j := range set {
			out = append(out, j.View())
		}
	}
	return out
}

func (q *Queue) BatchView(id string) (BatchView, bool) {
	q.mu.Lock()
	b, ok := q.batches[id]
	if !ok {
		q.mu.Unlock()
		return BatchView{}, false
	}
	jobs := make([]*Job, 0, len(b.jobIDs))
	for _, jid := range b.jobIDs {
		if j, ok := q.jobs[jid]; ok {
			jobs = append(jobs, j)
		}
	}
	dir := b.Dir
	q.mu.Unlock()

	v := BatchView{ID: id, Dir: dir.String()}
	for _, j := range jobs {
		jv := j.View()
		v.Jobs = append(v.Jobs, jv)
		switch jv.State {
		case Done:
			v.Done++
		case Failed:
			v.Failed++
		case Queued, Running:
			v.Pending++
		}
	}
	v.Active = v.Pending > 0
	return v, true
}

// ByRel finds the newest job for a file, so a listing can show its state.
func (q *Queue) ByRel(rel outpath.Rel) (View, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	id, ok := q.byOutput[q.mapper.Output(rel)]
	if !ok {
		return View{}, false
	}
	j, ok := q.jobs[id]
	if !ok {
		return View{}, false
	}
	return j.View(), true
}

// --- events ---

// Subscribe returns a channel of events and the function to stop it. Progress
// events are dropped when a subscriber falls behind, because the next one
// supersedes them; state changes never are, because nothing supersedes a job
// finishing.
func (q *Queue) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	q.submu.Lock()
	q.subs[ch] = struct{}{}
	q.submu.Unlock()

	return ch, func() {
		q.submu.Lock()
		if _, ok := q.subs[ch]; ok {
			delete(q.subs, ch)
			close(ch)
		}
		q.submu.Unlock()
	}
}

func (q *Queue) emit(ev Event) {
	q.submu.Lock()
	defer q.submu.Unlock()
	for ch := range q.subs {
		select {
		case ch <- ev:
		default:
			if ev.Kind == "progress" {
				continue // the next reading says the same thing, only fresher
			}
			// A state change that cannot be delivered means this subscriber's
			// picture is now wrong. Cut it loose; the client reconnects and
			// re-reads the whole snapshot.
			delete(q.subs, ch)
			close(ch)
		}
	}
}

// --- dispatch ---

func (q *Queue) nudge() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *Queue) dispatchLoop() {
	for range q.wake {
		for {
			q.mu.Lock()
			if len(q.running) >= q.workers || len(q.pending) == 0 {
				q.mu.Unlock()
				break
			}
			id := q.pending[0]
			q.pending = q.pending[1:]
			j, ok := q.jobs[id]
			if !ok {
				q.mu.Unlock()
				continue
			}
			q.running[id] = j
			q.mu.Unlock()

			go q.run(j)
		}
	}
}

func (q *Queue) run(j *Job) {
	defer func() {
		q.mu.Lock()
		delete(q.running, j.ID)
		q.finishedLocked(j.ID)
		q.mu.Unlock()
		q.emit(Event{Kind: "state", Job: j.View(), BatchID: j.BatchID})
		q.nudge()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	j.start(cancel)
	defer cancel()

	q.emit(Event{Kind: "state", Job: j.View(), BatchID: j.BatchID})

	spec, err := q.plan(ctx, j)
	if err != nil {
		j.finish(Failed, err.Error())
		q.release(j)
		return
	}

	if err := os.MkdirAll(filepath.Dir(j.dst), 0o755); err != nil {
		j.finish(Failed, err.Error())
		q.release(j)
		return
	}
	if j.liveDir != "" {
		os.RemoveAll(j.liveDir)
		if err := os.MkdirAll(j.liveDir, 0o755); err != nil {
			log.Printf("live output unavailable for %s: %v", j.Name, err)
			j.dropLive()
			spec.LiveDir = ""
		}
	}

	log.Printf("convert start: %s (remux=%v live=%v)", j.Rel.String(), spec.Remux, spec.LiveDir != "")
	runErr := q.runner.Run(ctx, spec, q.settings, func(p ffmpeg.Progress) {
		q.onProgress(j, p)
	})

	if runErr != nil {
		os.Remove(j.part)
		q.cleanupLive(j)
		if ctx.Err() != nil {
			j.finish(Canceled, "")
			log.Printf("convert canceled: %s", j.Rel.String())
		} else {
			j.finish(Failed, runErr.Error())
			log.Printf("convert failed: %s: %v", j.Rel.String(), runErr)
		}
		q.release(j)
		return
	}

	// Renaming into place is what makes "the file exists" mean "the file is
	// finished". Until now there was only a .part.
	if err := os.Rename(j.part, j.dst); err != nil {
		os.Remove(j.part)
		q.cleanupLive(j)
		j.finish(Failed, err.Error())
		q.release(j)
		return
	}
	q.cleanupLive(j)
	j.finish(Done, "")
	log.Printf("convert done: %s in %s", j.Rel.String(), time.Since(j.startedAt).Round(time.Second))
}

// plan probes the source and decides how much work it needs.
func (q *Queue) plan(ctx context.Context, j *Job) (ffmpeg.Spec, error) {
	fi, err := os.Stat(j.src)
	if err != nil {
		return ffmpeg.Spec{}, err
	}
	info, err := q.prober.Probe(ctx, j.src, fi)
	if err != nil {
		return ffmpeg.Spec{}, err
	}
	video, ok := info.Video()
	if !ok {
		return ffmpeg.Spec{}, fmt.Errorf("no video stream in %s", j.Name)
	}

	audioIdx := -1
	if audio := info.Audio(); len(audio) > 0 {
		audioIdx = audio[0].Index
	}

	// Burning subtitles means redrawing every frame, so the copy shortcut is
	// off however convenient the codecs are.
	remux := info.RemuxOnly() && j.opts.BurnSubs == ""

	j.setPlan(time.Duration(info.Duration*float64(time.Second)), remux)

	return ffmpeg.Spec{
		Src: j.src, Dst: j.part,
		Remux:      remux,
		VideoIndex: video.Index,
		AudioIndex: audioIdx,
		BurnSubs:   j.opts.BurnSubs,
		LiveDir:    j.liveDir,
	}, nil
}

func (q *Queue) onProgress(j *Job, p ffmpeg.Progress) {
	j.mu.Lock()
	j.out = p.OutTime
	j.fps = p.FPS
	j.speed.Add(time.Now(), p.OutTime)
	pct := ffmpeg.Percent(j.out, j.duration, false)
	batchID := j.BatchID
	j.mu.Unlock()

	q.emit(Event{Kind: "progress", Job: j.View(), BatchID: batchID})

	if pct >= q.checkpointAt {
		q.maybeCheckpoint(j)
	}
}

// maybeCheckpoint fires once per batch, on its first job, when enough of the
// output exists to judge it. Everything keeps running; the point is only that
// there is now something to look at before the remaining hours are spent.
func (q *Queue) maybeCheckpoint(j *Job) {
	q.mu.Lock()
	b, ok := q.batches[j.BatchID]
	q.mu.Unlock()
	if !ok {
		return
	}

	b.mu.Lock()
	first := len(b.jobIDs) > 0 && b.jobIDs[0] == j.ID
	if b.checkpoint || !first {
		b.mu.Unlock()
		return
	}
	b.checkpoint = true
	b.mu.Unlock()

	q.emit(Event{Kind: "checkpoint", Job: j.View(), BatchID: j.BatchID})
}

// --- small helpers ---

func (j *Job) start(cancel context.CancelFunc) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.state = Running
	j.startedAt = time.Now()
	j.cancel = cancel
}

func (j *Job) setPlan(d time.Duration, remux bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.duration = d
	j.remux = remux
}

func (j *Job) finish(s State, errText string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.state = s
	j.errText = errText
	j.endedAt = time.Now()
	j.cancel = nil
	if j.startedAt.IsZero() {
		j.startedAt = j.endedAt
	}
}

func (j *Job) dropLive() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.opts.Live = false
	j.liveDir = ""
}

// LiveDir is where a running job's HLS rendition is written, or "" when it has
// none or has finished with it.
func (j *Job) LiveDir() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.liveDir
}

func (q *Queue) cleanupLive(j *Job) {
	if dir := j.LiveDir(); dir != "" {
		os.RemoveAll(dir)
		j.dropLive()
	}
}

// release lets another job claim the same output path once this one is over
// without having produced it.
func (q *Queue) release(j *Job) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if id, ok := q.byOutput[j.dst]; ok && id == j.ID {
		delete(q.byOutput, j.dst)
	}
}

func (q *Queue) removePendingLocked(id string) {
	for i, pid := range q.pending {
		if pid == id {
			q.pending = append(q.pending[:i], q.pending[i+1:]...)
			return
		}
	}
}

const historyLimit = 200

func (q *Queue) finishedLocked(id string) {
	q.finished = append(q.finished, id)
	if len(q.finished) > historyLimit {
		drop := q.finished[0]
		q.finished = q.finished[1:]
		delete(q.jobs, drop)
	}
}

// Sweep removes partial output and live segments left behind by a process that
// died mid-job. A .part is unusable — it has no index — and nothing will ever
// come looking for it.
func Sweep(outRoot, liveRoot string) {
	os.RemoveAll(liveRoot)
	os.MkdirAll(liveRoot, 0o755)

	filepath.WalkDir(outRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && filepath.Ext(p) == ".part" {
			log.Printf("removing interrupted output: %s", p)
			os.Remove(p)
		}
		return nil
	})
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// EnqueueDir is the whole flow behind one click: list the directory, put the
// chosen file first, and queue the result.
func (q *Queue) EnqueueDir(lib *library.Library, picked outpath.Rel, scope library.Scope, opts Options) (*Batch, error) {
	dir := picked.Dir()
	files, err := lib.Videos(dir)
	if err != nil {
		return nil, err
	}
	return q.Enqueue(dir, library.Order(files, picked, scope), opts)
}
