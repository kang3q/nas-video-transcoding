package jobs

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"

	"nvt/ver2/internal/outpath"
)

// Persistence exists because a batch is a night's work. Losing the queue to a
// restart — a NAS reboot, a container update, a power cut — would mean picking
// through a folder by hand to work out what was left.
//
// What survives is the list of files still outstanding, in order, with the
// options they were queued under. What does not is partial encoding: an
// interrupted ffmpeg leaves an MP4 with no index, and there is no way to
// continue one. That file starts again, and because it was at the front of the
// queue it starts again first.

type persistedBatch struct {
	ID         string   `json:"id"`
	Dir        string   `json:"dir"`
	Created    int64    `json:"created"`
	Checkpoint bool     `json:"checkpoint"` // the inspection prompt already went out
	Options    Options  `json:"options"`
	Rels       []string `json:"rels"` // still to do, in order
}

type persisted struct {
	Version int              `json:"version"`
	Batches []persistedBatch `json:"batches"`
}

const persistVersion = 1

// save writes the outstanding work. Running jobs count as outstanding: if the
// process is gone, so is their progress.
func (q *Queue) save() {
	q.mu.Lock()
	stopping := q.stopping
	q.mu.Unlock()
	if stopping {
		return
	}
	q.persist()
}

// persist does the writing. Running jobs count as outstanding: if the process
// is gone, so is their progress.
func (q *Queue) persist() {
	if q.stateFile == "" {
		return
	}

	q.mu.Lock()
	// Preserve the order jobs were queued in: pending first in queue order,
	// then whatever is running, which was ahead of all of it.
	outstanding := map[string][]string{}
	var running []*Job
	for _, j := range q.running {
		running = append(running, j)
	}
	order := append([]string(nil), q.pending...)

	byBatch := map[string]*Batch{}
	for id, b := range q.batches {
		byBatch[id] = b
	}
	for _, j := range running {
		outstanding[j.BatchID] = append(outstanding[j.BatchID], j.Rel.String())
	}
	for _, id := range order {
		if j, ok := q.jobs[id]; ok {
			outstanding[j.BatchID] = append(outstanding[j.BatchID], j.Rel.String())
		}
	}

	var out persisted
	out.Version = persistVersion
	for id, rels := range outstanding {
		b, ok := byBatch[id]
		if !ok || len(rels) == 0 {
			continue
		}
		b.mu.Lock()
		pb := persistedBatch{
			ID: b.ID, Dir: b.Dir.String(), Created: b.Created.Unix(),
			Checkpoint: b.checkpoint, Options: b.opts, Rels: rels,
		}
		b.mu.Unlock()
		out.Batches = append(out.Batches, pb)
	}
	q.mu.Unlock()

	if len(out.Batches) == 0 {
		os.Remove(q.stateFile)
		return
	}
	blob, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return
	}
	tmp := q.stateFile + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o644); err != nil {
		log.Printf("saving queue: %v", err)
		return
	}
	if err := os.Rename(tmp, q.stateFile); err != nil {
		log.Printf("saving queue: %v", err)
	}
}

// Restore puts back whatever was outstanding when the process last stopped.
// Files that have since been converted are skipped by Enqueue, so a restart
// after a partial run picks up exactly where it left off.
func (q *Queue) Restore() {
	if q.stateFile == "" {
		return
	}
	blob, err := os.ReadFile(q.stateFile)
	if err != nil {
		return
	}
	var in persisted
	if err := json.Unmarshal(blob, &in); err != nil {
		log.Printf("queue state unreadable, starting empty: %v", err)
		return
	}
	if in.Version != persistVersion {
		log.Printf("queue state is version %d, this build writes %d; starting empty",
			in.Version, persistVersion)
		return
	}

	for _, pb := range in.Batches {
		dir, err := q.mapper.ParseRel(pb.Dir)
		if err != nil {
			continue
		}
		var rels []outpath.Rel
		for _, r := range pb.Rels {
			rel, err := q.mapper.ParseRel(r)
			if err != nil {
				continue
			}
			if _, err := os.Stat(q.mapper.Source(rel)); err != nil {
				continue // the source is gone
			}
			rels = append(rels, rel)
		}
		if len(rels) == 0 {
			continue
		}
		b, err := q.enqueue(pb.ID, dir, rels, pb.Options)
		if err != nil {
			continue
		}
		if pb.Checkpoint {
			// The prompt already went out for this batch; sending it again
			// after a restart would be noise.
			b.mu.Lock()
			b.checkpoint = true
			b.mu.Unlock()
		}
		log.Printf("resumed batch %s: %d file(s) still to do", b.ID, len(rels))
	}
}

// Shutdown stops cleanly: the outstanding work is written down first, then
// running conversions are interrupted so ffmpeg closes its files, and the
// partial output is discarded because it cannot be continued.
func (q *Queue) Shutdown(ctx context.Context) {
	// Close the queue first. Otherwise interrupting the running job frees a
	// slot, the next one starts, and its bookkeeping overwrites the state we
	// are about to write.
	q.mu.Lock()
	q.stopping = true
	q.mu.Unlock()

	q.persist()

	q.mu.Lock()
	running := make([]*Job, 0, len(q.running))
	for _, j := range q.running {
		running = append(running, j)
	}
	q.mu.Unlock()

	for _, j := range running {
		if c := j.cancelFunc(); c != nil {
			log.Printf("interrupting %s for shutdown", j.Rel.String())
			c()
		}
	}

	for {
		q.mu.Lock()
		left := len(q.running)
		q.mu.Unlock()
		if left == 0 {
			return
		}
		select {
		case <-ctx.Done():
			log.Printf("%d conversion(s) did not stop in time", left)
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func stateFilePath(dir string) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "queue.json")
}
