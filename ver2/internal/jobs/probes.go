package jobs

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"nvt/ver2/internal/airplay"
	"nvt/ver2/internal/ffmpeg"
	"nvt/ver2/internal/outpath"
)

// EnqueueAirPlayProbes renders the diagnostic ladder for one video.
//
// These are ordinary jobs — same worker, same queue, same cancel — so they
// cannot run alongside a real conversion and steal the encoder from it. They
// write beside the library output rather than into it, under a plain ASCII
// path, and they do not collide with the file's real conversion because the
// queue keys work by output path and these have their own.
func (q *Queue) EnqueueAirPlayProbes(rel outpath.Rel) (*Batch, error) {
	if rel.IsRoot() {
		return nil, fmt.Errorf("airplay: no file given")
	}
	src := q.mapper.Source(rel)
	if _, err := os.Stat(src); err != nil {
		return nil, err
	}

	dir := filepath.Join(q.mapper.OutputRoot(), filepath.FromSlash(airplay.Dir(rel)))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	// Redoing the ladder should replace it, not leave half of a previous run
	// mixed in with half of this one.
	for _, v := range airplay.Variants {
		os.Remove(filepath.Join(dir, v.File()))
	}

	batch := &Batch{ID: newID(), Dir: rel.Dir(), Created: time.Now()}

	q.mu.Lock()
	var created []*Job
	for _, v := range airplay.Variants {
		dst := filepath.Join(dir, v.File())
		if id, busy := q.byOutput[dst]; busy {
			if j, ok := q.jobs[id]; ok && !j.State().Terminal() {
				continue
			}
		}
		j := &Job{
			ID: newID(), BatchID: batch.ID, Rel: rel, Name: v.Name,
			src: src, part: dst + ".part", dst: dst,
			state: Queued, queuedAt: time.Now(),
			speed:   ffmpeg.NewSpeedTracker(30 * time.Second),
			variant: v.Name,
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
	// A diagnostic batch is deliberately left out of the saved queue: it is a
	// couple of minutes of work made to answer a question being asked right
	// now, and resuming it after a restart would answer nobody.
	q.batches[batch.ID] = batch
	q.mu.Unlock()

	for _, j := range created {
		q.emit(Event{Kind: "state", Job: j.View(), BatchID: batch.ID})
	}
	log.Printf("queued airplay probes %s: %s (%d clips)", batch.ID, rel.String(), len(created))
	q.nudge()
	return batch, nil
}
