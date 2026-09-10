// Package history remembers where you stopped watching.
//
// It is deliberately one shared record rather than one per viewer. This runs
// on a NAS in a house, behind a single password, and "where did I get to?"
// is a question about the household's evening, not about an account. Adding
// identities would mean adding accounts, and there are none.
package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Entry is one film or episode someone has started.
type Entry struct {
	Rel      string  `json:"rel"`
	Name     string  `json:"name"`
	Pos      float64 `json:"pos"`      // seconds
	Duration float64 `json:"duration"` // seconds, 0 when unknown
	Updated  int64   `json:"updated"`  // unix seconds

	// Seq breaks ties. Unix seconds are too coarse to order two things
	// watched in the same second, and an unstable sort over equal keys makes
	// the list reshuffle itself between page loads. Entries written before
	// this existed have zero and fall back to the timestamp.
	Seq uint64 `json:"seq,omitempty"`
}

// Done reports whether this was watched to the end. The last minutes of an
// episode are credits, and offering to resume them is offering nothing.
func (e Entry) Done() bool {
	if e.Duration <= 0 {
		return false
	}
	return e.Pos >= e.Duration*doneFraction || e.Duration-e.Pos < doneTailSecs
}

// Resumable reports whether there is a point worth going back to. The first
// half minute is the titles, and jumping there is the same as starting.
func (e Entry) Resumable() bool { return e.Pos >= minResumeSecs && !e.Done() }

const (
	doneFraction  = 0.97
	doneTailSecs  = 45
	minResumeSecs = 30
	// keep is how many titles to remember. Long enough to cover a season
	// someone is part way through, short enough that the file stays small.
	keep = 200
)

type Store struct {
	file string

	mu      sync.Mutex
	entries map[string]Entry
	seq     uint64
	dirty   bool
}

// New loads whatever was remembered last time. A missing or unreadable file
// is an empty history, not an error: nothing here is worth failing to start
// the server over.
func New(stateDir string) *Store {
	s := &Store{
		file:    filepath.Join(stateDir, "history.json"),
		entries: map[string]Entry{},
	}
	if b, err := os.ReadFile(s.file); err == nil {
		var loaded map[string]Entry
		if json.Unmarshal(b, &loaded) == nil {
			s.entries = loaded
			for _, e := range loaded {
				if e.Seq > s.seq {
					s.seq = e.Seq
				}
			}
		}
	}
	return s
}

// Note records where playback has reached. A position at or past the end is
// kept too, so the list can say what has been finished rather than quietly
// forgetting it.
func (s *Store) Note(rel, name string, pos, duration float64) {
	if rel == "" || pos < 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.seq++
	e := s.entries[rel]
	e.Rel, e.Name, e.Pos, e.Updated, e.Seq = rel, name, pos, time.Now().Unix(), s.seq
	if duration > 0 {
		e.Duration = duration
	}
	s.entries[rel] = e
	s.dirty = true
	s.trimLocked()
}

// Get returns what is remembered about one title.
func (s *Store) Get(rel string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[rel]
	return e, ok
}

// Forget drops one title, for someone who wants it off the list.
func (s *Store) Forget(rel string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[rel]; ok {
		delete(s.entries, rel)
		s.dirty = true
	}
}

// Recent returns what was watched, most recent first.
func (s *Store) Recent(limit int) []Entry {
	s.mu.Lock()
	out := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e)
	}
	s.mu.Unlock()

	sort.Slice(out, func(i, j int) bool { return newerFirst(out[i], out[j]) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func newerFirst(a, b Entry) bool {
	if a.Updated != b.Updated {
		return a.Updated > b.Updated
	}
	if a.Seq != b.Seq {
		return a.Seq > b.Seq
	}
	// Nothing left to tell them apart, so pick something that does not
	// change between page loads.
	return a.Rel < b.Rel
}

func (s *Store) trimLocked() {
	if len(s.entries) <= keep {
		return
	}
	all := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		all = append(all, e)
	}
	sort.Slice(all, func(i, j int) bool { return newerFirst(all[i], all[j]) })
	for _, e := range all[keep:] {
		delete(s.entries, e.Rel)
	}
}

// Flush writes the file if anything changed. Called on a timer and at
// shutdown; losing the last few seconds of a position is not worth a write
// on every progress report.
func (s *Store) Flush() {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	b, err := json.Marshal(s.entries)
	s.dirty = false
	s.mu.Unlock()
	if err != nil {
		return
	}
	tmp := s.file + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, s.file)
	}
}
