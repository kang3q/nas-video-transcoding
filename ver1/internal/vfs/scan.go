package vfs

import (
	"sync"
	"time"
)

// scanDetector tells a player sweeping the share apart from a viewer watching
// something.
//
// It cannot be done by looking at a single request. A player building its
// library opens each file, reads the container index at the end, and grabs a
// frame from the middle for a thumbnail -- and the request that starts all
// that is byte-for-byte the same as the one that starts playback. What differs
// is breadth: a sweep touches every file in a directory within seconds, a
// viewer touches one.
type scanDetector struct {
	window time.Duration
	limit  int

	mu   sync.Mutex
	seen map[string]time.Time
}

func newScanDetector(window time.Duration, limit int) *scanDetector {
	return &scanDetector{window: window, limit: limit, seen: map[string]time.Time{}}
}

// Touch records a request and reports whether the recent traffic looks like a
// sweep. Repeated requests for one file never do, however many there are.
func (s *scanDetector) Touch(path string) bool {
	if s.limit <= 0 {
		return false
	}
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	for p, at := range s.seen {
		if now.Sub(at) > s.window {
			delete(s.seen, p)
		}
	}
	s.seen[path] = now
	return len(s.seen) > s.limit
}
