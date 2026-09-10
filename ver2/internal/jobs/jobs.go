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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nvt/ver2/internal/ffmpeg"
	"nvt/ver2/internal/library"
	"nvt/ver2/internal/mediainfo"
	"nvt/ver2/internal/outpath"
	"nvt/ver2/internal/subs"
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
	// Subtitles draws a subtitle track into the picture. Irreversible, and it
	// forces a re-encode even when the codecs would have allowed a copy.
	Subtitles bool
	// PickedSubtitleID applies to the first file only. A folder conversion
	// cannot ask about every episode.
	PickedSubtitleID string
	// SubtitleLang is what the rest of the batch looks for: whatever language
	// the viewer chose. Episodes without it get no subtitles rather than a
	// language nobody asked for burned in permanently.
	SubtitleLang string
	// Live also produces an HLS rendition, so the job can be watched before it
	// finishes. Costs a little disk and almost no CPU: the encode happens once
	// either way.
	Live bool

	// Burn draws the subtitle into the frames instead of carrying it as a
	// track. It is the only way to be sure every player shows it, and it
	// costs a full re-encode of a file that may have needed none.
	Burn bool
}

// SubResolver produces the subtitle file for one video, or "" when there is
// nothing to use. The format matters: burning needs ASS, a track inside the
// MP4 needs SRT. See [subs.Format].
type SubResolver interface {
	Resolve(ctx context.Context, jobID string, rel outpath.Rel, preferredID, preferLang string, format subs.Format) (path string, cleanup func(), err error)
}

type Job struct {
	ID      string
	BatchID string
	Rel     outpath.Rel
	Name    string

	src, part, dst, liveDir string
	opts                    Options
	preferredSub            string

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
	// ReadyInSec is how much longer to wait before playback can run to the
	// end without overtaking the encoder. 0 means now, -1 means not yet
	// knowable. Only meaningful while the job is running.
	ReadyInSec float64 `json:"ready_in_sec"`
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
	v.ReadyInSec = -1
	if j.speed != nil {
		v.Rate = j.speed.Rate()
		if eta := j.speed.ETA(j.out, j.duration); eta > 0 {
			v.ETASec = eta.Seconds()
		}
		// Playback consumes one second of video per second; the encoder makes
		// Rate of them. Below realtime the difference has to be banked first.
		if head := ffmpeg.HeadStart(j.duration, v.Rate); head >= 0 {
			left := head.Seconds() - time.Since(j.startedAt).Seconds()
			if left < 0 {
				left = 0
			}
			v.ReadyInSec = left
		}
	}
	if j.state == Done {
		v.ReadyInSec = 0
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
	opts       Options
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
	// Cached answers from what has already been learned, or not at all. The
	// dispatcher uses it to tell a few seconds of copying from forty minutes
	// of encoding without paying to find out.
	Cached(path string, fi os.FileInfo) (mediainfo.Info, bool)
}

type Queue struct {
	mapper      *outpath.Mapper
	prober      Prober
	runner      ffmpeg.Runner
	subtitles   SubResolver
	settings    ffmpeg.Settings
	workers     int
	liveRoot    string
	segmentSecs int
	stateFile   string
	// stopping closes the queue for business: no new job starts, and nothing
	// rewrites the state file after shutdown has recorded it.
	stopping bool
	// checkpointAt is the fraction of the first file in a batch to reach
	// before asking whether the result looks right. Far enough in that an
	// opening sequence is over and burned-in subtitles are on screen.
	checkpointAt float64

	// finishedCount lets a cached listing notice that a conversion landed,
	// without taking the queue's lock or walking anything.
	finishedCount atomic.Uint64

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
	Subs         SubResolver
	Settings     ffmpeg.Settings
	Workers      int
	LiveRoot     string
	SegmentSecs  int
	StateDir     string
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
		mapper: d.Mapper, prober: d.Prober, runner: d.Runner, subtitles: d.Subs,
		settings: d.Settings, workers: d.Workers,
		liveRoot: d.LiveRoot, segmentSecs: d.SegmentSecs,
		stateFile: stateFilePath(d.StateDir), checkpointAt: d.CheckpointAt,
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
	return q.enqueue(newID(), dir, rels, opts)
}

// enqueue takes the batch id explicitly so a restored batch keeps the one it
// had — a stop button pressed from an old notification still finds it.
func (q *Queue) enqueue(id string, dir outpath.Rel, rels []outpath.Rel, opts Options) (*Batch, error) {
	batch := &Batch{ID: id, Dir: dir, Created: time.Now(), opts: opts}

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
		// Only the first file gets a preview. Its purpose is the checkpoint —
		// look at the subtitles and the picture once, early, and stop the rest
		// if they are wrong — and that judgement is made and over with on the
		// first file. Every later one would write segments nobody opens, and
		// pay for them twice: the HLS branch forces a keyframe every four
		// seconds, which costs quality at the same bitrate and time on an
		// encoder that is already the bottleneck.
		if opts.Live && len(created) == 0 {
			j.liveDir = filepath.Join(q.liveRoot, j.ID)
		} else {
			j.opts.Live = false // so the page does not offer a player there is no stream for
		}
		if opts.Subtitles && len(created) == 0 {
			j.preferredSub = opts.PickedSubtitleID
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
	q.save()
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
		q.save()
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

// Finished counts conversions that have completed since this process began.
//
// It exists so a cached view of the library can tell, for the price of an
// atomic read, whether anything it summarised has changed. Walking a library
// takes ten seconds on this hardware; doing it again because a page was
// opened twice is ten seconds nobody gets back.
func (q *Queue) Finished() uint64 { return q.finishedCount.Load() }

// LiveDirFor returns where a running job is writing its HLS rendition, or ""
// when it has none. Used to decide whether to offer a player for something
// that is still being made.
func (q *Queue) LiveDirFor(id string) string {
	q.mu.Lock()
	j, ok := q.jobs[id]
	q.mu.Unlock()
	if !ok || j.State().Terminal() {
		return ""
	}
	return j.LiveDir()
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

// pickLocked chooses which waiting job runs next.
//
// Order is arrival order, with one exception: a job that only has to change
// the container jumps the ones that have to re-encode. The two are not the
// same kind of work. Copying the streams of an episode takes a few seconds;
// re-encoding one takes forty minutes on this hardware. Leaving a three
// second job behind a forty minute one, when the whole point of it was to
// add subtitles to something watchable tonight, makes the queue useless for
// exactly the case it was quickest at.
//
// This is not the preemption v1 had, which killed work already in progress.
// Nothing running is touched; only the order of what has not started.
//
// A job is known to be a copy only if its source has been probed before.
// Probing here to find out would cost more than the reordering saves — it is
// what made v1 take half a minute to list a folder — so an unexamined file
// simply waits its turn. In practice the ones this matters for have just
// been looked at on their own page.
func (q *Queue) pickLocked() int {
	for i, id := range q.pending {
		j, ok := q.jobs[id]
		if !ok {
			continue
		}
		if q.copyOnly(j) {
			return i
		}
	}
	return 0
}

// copyOnly reports whether this job can be done without an encoder, judged
// from what is already known. It never opens anything.
func (q *Queue) copyOnly(j *Job) bool {
	if j.opts.Burn {
		return false // drawing subtitles in means redrawing every frame
	}
	fi, err := os.Stat(j.src)
	if err != nil {
		return false
	}
	info, ok := q.prober.Cached(j.src, fi)
	return ok && info.RemuxOnly()
}

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
			if q.stopping || len(q.running) >= q.workers || len(q.pending) == 0 {
				q.mu.Unlock()
				break
			}
			at := q.pickLocked()
			id := q.pending[at]
			q.pending = append(q.pending[:at], q.pending[at+1:]...)
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
		q.save()
		q.nudge()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	j.start(cancel)
	defer cancel()

	q.emit(Event{Kind: "state", Job: j.View(), BatchID: j.BatchID})

	spec, cleanupSubs, err := q.plan(ctx, j)
	if err != nil {
		j.finish(Failed, err.Error())
		q.release(j)
		return
	}
	defer cleanupSubs()

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
	// The same subtitle, written once more beside the video for the browser
	// to read. Safari places a track from inside the file wherever the file
	// says, which is not where subtitles belong; Chrome does not read one at
	// all. A .vtt next to the .mp4 is placed by the browser and works in
	// both. It cannot replace the track inside the file — AirPlay hands the
	// television a URL and nothing else — so both exist.
	if spec.SoftSubs != "" {
		vtt := strings.TrimSuffix(j.dst, filepath.Ext(j.dst)) + ".vtt"
		if err := subs.WriteVTT(spec.SoftSubs, vtt); err != nil {
			log.Printf("could not write %s: %v", filepath.Base(vtt), err)
		}
	}

	q.cleanupLive(j)
	j.finish(Done, "")
	q.finishedCount.Add(1)
	log.Printf("convert done: %s in %s", j.Rel.String(), time.Since(j.startedAt).Round(time.Second))
}

// plan probes the source, works out how much work it needs, and gets any
// subtitle ready. The returned cleanup removes the temporary subtitle file.
func (q *Queue) plan(ctx context.Context, j *Job) (ffmpeg.Spec, func(), error) {
	noop := func() {}

	fi, err := os.Stat(j.src)
	if err != nil {
		return ffmpeg.Spec{}, noop, err
	}
	// Probe before resolving subtitles: the subtitle finder reads the embedded
	// track list out of this result.
	info, err := q.prober.Probe(ctx, j.src, fi)
	if err != nil {
		return ffmpeg.Spec{}, noop, err
	}
	video, ok := info.Video()
	if !ok {
		return ffmpeg.Spec{}, noop, fmt.Errorf("no video stream in %s", j.Name)
	}

	audioIdx := -1
	if audio := info.Audio(); len(audio) > 0 {
		audioIdx = audio[0].Index
	}

	format := subs.FormatSRT
	if j.opts.Burn {
		format = subs.FormatASS
	}
	var burn string
	cleanup := noop
	if j.opts.Subtitles && q.subtitles != nil {
		burn, cleanup, err = q.subtitles.Resolve(ctx, j.ID, j.Rel, j.preferredSub, j.opts.SubtitleLang, format)
		if err != nil {
			return ffmpeg.Spec{}, noop, err
		}
	}

	// A diagnostic clip is its own thing: no subtitles, no remux shortcut, no
	// live rendition. Each of those is a way for the test to fail for a reason
	// that has nothing to do with what it is testing.
	// Burning means redrawing every frame, so the copy shortcut is off however
	// convenient the codecs are. Carrying the subtitle as its own track does
	// not touch the picture, so the shortcut survives.
	soft := ""
	if !j.opts.Burn {
		soft, burn = burn, ""
	}
	remux := info.RemuxOnly() && burn == ""

	j.setPlan(time.Duration(info.Duration*float64(time.Second)), remux)

	return ffmpeg.Spec{
		Src: j.src, Dst: j.part,
		Remux:       remux,
		VideoIndex:  video.Index,
		AudioIndex:  audioIdx,
		BurnSubs:    burn,
		SoftSubs:    soft,
		SubsLang:    subtitleLang(j),
		Width:       video.Width,
		Height:      video.Height,
		LiveDir:     j.liveDir,
		SegmentSecs: q.segmentSecs,
	}, cleanup, nil
}

// subtitleLang is what to tag a soft subtitle track with, so a player names
// it rather than calling it "Track 1".
func subtitleLang(j *Job) string {
	if j.opts.SubtitleLang != "" {
		return j.opts.SubtitleLang
	}
	return "kor"
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

	log.Printf("checkpoint: %s reached %.0f%%, offering it for inspection",
		j.Rel.String(), q.checkpointAt*100)

	// Record that the prompt went out, so a restart does not send it again.
	q.save()
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
