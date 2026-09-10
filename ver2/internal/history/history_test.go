package history

import (
	"os"
	"path/filepath"
	"testing"
)

// The first half minute is titles and the last minutes are credits. Offering
// to resume either is offering nothing.
func TestResumableSkipsTheEndsOfAnEpisode(t *testing.T) {
	cases := []struct {
		name      string
		pos, dur  float64
		resumable bool
		done      bool
	}{
		{"barely started", 5, 1440, false, false},
		{"part way through", 700, 1440, true, false},
		{"into the credits", 1430, 1440, false, true},
		{"at the very end", 1440, 1440, false, true},
		{"duration unknown", 700, 0, true, false},
		{"short clip, part way", 40, 120, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := Entry{Pos: tc.pos, Duration: tc.dur}
			if got := e.Resumable(); got != tc.resumable {
				t.Errorf("Resumable() = %v, want %v", got, tc.resumable)
			}
			if got := e.Done(); got != tc.done {
				t.Errorf("Done() = %v, want %v", got, tc.done)
			}
		})
	}
}

// What was watched last is what someone is most likely coming back for.
func TestRecentIsNewestFirst(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)

	s.Note("a.mkv", "a", 100, 1400)
	s.Note("b.mkv", "b", 200, 1400)
	s.Note("c.mkv", "c", 300, 1400)
	// Watching a again puts it back at the top.
	s.Note("a.mkv", "a", 400, 1400)

	got := s.Recent(0)
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3", len(got))
	}
	if got[0].Rel != "a.mkv" {
		t.Errorf("newest is %q, want a.mkv", got[0].Rel)
	}
	if got[0].Pos != 400 {
		t.Errorf("position was not updated: %v", got[0].Pos)
	}
	if n := len(s.Recent(2)); n != 2 {
		t.Errorf("limit ignored: got %d", n)
	}
}

// A position is worth nothing if it does not survive the restart that
// interrupted the evening.
func TestHistorySurvivesARestart(t *testing.T) {
	dir := t.TempDir()

	s := New(dir)
	s.Note("애니/ep1.mkv", "ep1.mkv", 615, 1440)
	s.Flush()

	again := New(dir)
	e, ok := again.Get("애니/ep1.mkv")
	if !ok {
		t.Fatal("the position was lost")
	}
	if e.Pos != 615 || e.Duration != 1440 || e.Name != "ep1.mkv" {
		t.Errorf("came back as %+v", e)
	}
}

// A history file someone edited, or a half-written one from a power cut, is
// an empty history — not a reason to refuse to start.
func TestAnUnreadableHistoryIsJustEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "history.json"), []byte("{oh dear"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := len(New(dir).Recent(0)); n != 0 {
		t.Errorf("got %d entries from a broken file", n)
	}
}

func TestForget(t *testing.T) {
	s := New(t.TempDir())
	s.Note("a.mkv", "a", 100, 1400)
	s.Forget("a.mkv")
	if _, ok := s.Get("a.mkv"); ok {
		t.Error("it is still remembered")
	}
}

// The file has to stay small on a NAS that has been running for years.
func TestOldestAreDroppedPastTheLimit(t *testing.T) {
	s := New(t.TempDir())
	for i := range keep + 50 {
		s.Note(string(rune('a'+i%26))+string(rune('a'+i/26))+".mkv", "x", float64(i), 1400)
	}
	if n := len(s.Recent(0)); n > keep {
		t.Errorf("kept %d entries, want at most %d", n, keep)
	}
}
