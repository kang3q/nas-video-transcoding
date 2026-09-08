package cache

import (
	"bytes"
	"os"
	"testing"
	"time"
)

func write(t *testing.T, c *Cache, key string, size int, age time.Duration) {
	t.Helper()
	if err := os.WriteFile(c.Path(key), bytes.Repeat([]byte("x"), size), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.MarkComplete(key); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-age)
	if err := os.Chtimes(c.marker(key), at, at); err != nil {
		t.Fatal(err)
	}
}

func TestEvictDropsLeastRecentlyUsed(t *testing.T) {
	c, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, c, "old", 100, 3*time.Hour)
	write(t, c, "mid", 100, 2*time.Hour)
	write(t, c, "new", 100, time.Minute)

	c.Evict(250)

	if c.Complete("old") {
		t.Error("oldest entry survived eviction")
	}
	if !c.Complete("mid") || !c.Complete("new") {
		t.Error("eviction removed more than it needed to")
	}
	if got := c.Total(); got > 250 {
		t.Errorf("total after eviction = %d, want <= 250", got)
	}
}

// A conversion that is still running has no completion marker. Evicting it
// would pull the file out from under both ffmpeg and whoever is streaming it.
func TestEvictSkipsInFlightJobs(t *testing.T) {
	c, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, c, "done", 100, time.Hour)
	if err := os.WriteFile(c.Path("running"), bytes.Repeat([]byte("x"), 200), 0o644); err != nil {
		t.Fatal(err)
	}

	c.Evict(150)

	if _, err := os.Stat(c.Path("running")); err != nil {
		t.Error("eviction deleted an in-flight conversion")
	}
	if c.Complete("done") {
		t.Error("completed entry should have been evicted first")
	}
}

// Output left behind by a crash is unusable: it has no marker, so nothing will
// ever serve it, and it would otherwise occupy the cache forever.
func TestNewSweepsInterruptedOutput(t *testing.T) {
	dir := t.TempDir()
	c, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	write(t, c, "good", 100, time.Minute)
	if err := os.WriteFile(c.Path("orphan"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := New(dir); err != nil { // simulate a restart
		t.Fatal(err)
	}

	if _, err := os.Stat(c.Path("orphan")); !os.IsNotExist(err) {
		t.Error("interrupted output survived a restart")
	}
	if !c.Complete("good") {
		t.Error("restart discarded a completed entry")
	}
}

func TestKeyChangesWithEncodingSettings(t *testing.T) {
	a := Key("/m/x.avi", 100, 5, "audio", "aac", "384k")
	b := Key("/m/x.avi", 100, 5, "audio", "aac", "192k")
	if a == b {
		t.Error("changing the bitrate must produce a different cache key")
	}
	if a != Key("/m/x.avi", 100, 5, "audio", "aac", "384k") {
		t.Error("key is not stable for identical inputs")
	}
	if a == Key("/m/x.avi", 100, 6, "audio", "aac", "384k") {
		t.Error("a modified source file must produce a different cache key")
	}
}
