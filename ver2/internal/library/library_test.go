package library

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"nvt/ver2/internal/outpath"
)

func newLib(t *testing.T, outRel string, files ...string) (*Library, *outpath.Mapper, string) {
	t.Helper()
	base := t.TempDir()
	src := filepath.Join(base, "media")
	out := filepath.Join(base, "out")
	if outRel != "" {
		out = filepath.Join(src, outRel)
	}
	for _, f := range files {
		p := filepath.Join(src, filepath.FromSlash(f))
		if strings.HasSuffix(f, "/") {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	m, err := outpath.NewMapper(src, out, outRel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return New(m), m, src
}

// Episode 10 must not sort before episode 2.
func TestNaturalLess(t *testing.T) {
	in := []string{
		"Conan - 10.mkv", "Conan - 2.mkv", "Conan - 1.mkv",
		"Conan - 21.mkv", "Conan - 3.mkv", "Conan - 20.mkv",
	}
	want := []string{
		"Conan - 1.mkv", "Conan - 2.mkv", "Conan - 3.mkv",
		"Conan - 10.mkv", "Conan - 20.mkv", "Conan - 21.mkv",
	}
	got := append([]string(nil), in...)
	sort.Slice(got, func(i, j int) bool { return NaturalLess(got[i], got[j]) })
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sorted = %v\nwant     %v", got, want)
		}
	}
}

func TestNaturalLessDetails(t *testing.T) {
	cases := []struct {
		a, b string
		less bool
	}{
		{"ep2", "ep10", true},
		{"ep10", "ep2", false},
		// Numerically equal, so the raw string breaks the tie. Which way it
		// breaks does not matter; that it always breaks the same way does.
		{"ep02", "ep2", true},
		{"ep007", "ep8", true}, // leading zeros ignored
		{"a", "b", true},
		{"Show S01E02", "Show S01E10", true},
		{"Show S02E01", "Show S10E01", true},
		{"abc", "abcd", true},
		{"ABC.mkv", "abd.mkv", true}, // case-insensitive primary comparison
		{"1화", "10화", true},
		{"미래소년 코난 2", "미래소년 코난 12", true},
	}
	for _, c := range cases {
		if got := NaturalLess(c.a, c.b); got != c.less {
			t.Errorf("NaturalLess(%q, %q) = %v, want %v", c.a, c.b, got, c.less)
		}
	}
}

// Sorting must be a strict weak ordering or sort.Slice can misbehave.
func TestNaturalLessIsAntisymmetric(t *testing.T) {
	names := []string{"ep1", "ep01", "ep2", "ep10", "a", "A", "", "9", "10x", "10"}
	for _, a := range names {
		for _, b := range names {
			if a == b {
				if NaturalLess(a, b) {
					t.Errorf("NaturalLess(%q, %q) = true for equal values", a, b)
				}
				continue
			}
			if NaturalLess(a, b) && NaturalLess(b, a) {
				t.Errorf("both NaturalLess(%q,%q) and its reverse are true", a, b)
			}
		}
	}
}

func rels(t *testing.T, m *outpath.Mapper, names ...string) []outpath.Rel {
	t.Helper()
	out := make([]outpath.Rel, 0, len(names))
	for _, n := range names {
		r, err := m.ParseRel(n)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func strs(rs []outpath.Rel) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.String())
	}
	return out
}

// Whatever the scope, the file that was clicked is converted first — it is the
// one being waited on.
func TestOrder(t *testing.T) {
	_, m, _ := newLib(t, "")
	files := rels(t, m, "S/ep1.mkv", "S/ep2.mkv", "S/ep3.mkv", "S/ep4.mkv", "S/ep5.mkv")
	picked := files[2] // ep3

	cases := map[Scope][]string{
		ScopeFile:    {"S/ep3.mkv"},
		ScopeOnwards: {"S/ep3.mkv", "S/ep4.mkv", "S/ep5.mkv"},
		ScopeFolder:  {"S/ep3.mkv", "S/ep4.mkv", "S/ep5.mkv", "S/ep1.mkv", "S/ep2.mkv"},
	}
	for scope, want := range cases {
		t.Run(string(scope), func(t *testing.T) {
			got := strs(Order(files, picked, scope))
			if len(got) != len(want) {
				t.Fatalf("Order = %v, want %v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("Order = %v, want %v", got, want)
				}
			}
		})
	}
}

func TestOrderEdges(t *testing.T) {
	_, m, _ := newLib(t, "")
	files := rels(t, m, "S/a.mkv", "S/b.mkv", "S/c.mkv")

	t.Run("first file, whole folder", func(t *testing.T) {
		got := strs(Order(files, files[0], ScopeFolder))
		want := []string{"S/a.mkv", "S/b.mkv", "S/c.mkv"}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	})

	t.Run("last file, onwards", func(t *testing.T) {
		got := strs(Order(files, files[2], ScopeOnwards))
		if len(got) != 1 || got[0] != "S/c.mkv" {
			t.Errorf("got %v, want just the last file", got)
		}
	})

	t.Run("pick not in the listing", func(t *testing.T) {
		other := rels(t, m, "S/zzz.mkv")[0]
		got := strs(Order(files, other, ScopeFolder))
		if len(got) != 1 || got[0] != "S/zzz.mkv" {
			t.Errorf("got %v, want just the picked file", got)
		}
	})

	t.Run("whole folder keeps every file exactly once", func(t *testing.T) {
		got := Order(files, files[1], ScopeFolder)
		if len(got) != len(files) {
			t.Fatalf("got %d files, want %d", len(got), len(files))
		}
		seen := map[outpath.Rel]int{}
		for _, r := range got {
			seen[r]++
		}
		for _, f := range files {
			if seen[f] != 1 {
				t.Errorf("%s appears %d times", f.String(), seen[f])
			}
		}
	})
}

func TestListSortsDirectoriesFirstThenNaturally(t *testing.T) {
	lib, _, _ := newLib(t, "",
		"S/ep10.mkv", "S/ep2.mkv", "S/ep1.mkv",
		"S/Extras/", "S/Behind/",
		"S/notes.txt", "S/.hidden.mkv",
	)
	l, err := lib.List(mustRel(t, lib, "S"))
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(l.Entries))
	for _, e := range l.Entries {
		got = append(got, e.Name)
	}
	want := []string{"Behind", "Extras", "ep1.mkv", "ep2.mkv", "ep10.mkv", "notes.txt"}
	if len(got) != len(want) {
		t.Fatalf("List = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("List = %v, want %v", got, want)
		}
	}
}

func TestListMarksVideos(t *testing.T) {
	lib, _, _ := newLib(t, "", "S/a.mkv", "S/b.txt", "S/c.srt", "S/d.avi")
	l, err := lib.List(mustRel(t, lib, "S"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"a.mkv": true, "b.txt": false, "c.srt": false, "d.avi": true}
	for _, e := range l.Entries {
		if e.IsVideo != want[e.Name] {
			t.Errorf("%s IsVideo = %v, want %v", e.Name, e.IsVideo, want[e.Name])
		}
	}
}

// The output directory lives inside the share, so the library must not offer
// to convert what it has already produced.
func TestListHidesTheOutputDirectory(t *testing.T) {
	lib, _, _ := newLib(t, "_nvt", "a.mkv", "_nvt/a.mp4")
	l, err := lib.List(outpath.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range l.Entries {
		if e.Name == "_nvt" {
			t.Fatal("the output directory was listed as browsable content")
		}
	}
}

func TestVideosReturnsOnlyVideosInOrder(t *testing.T) {
	lib, _, _ := newLib(t, "", "S/ep10.mkv", "S/ep2.mkv", "S/readme.txt", "S/Extras/")
	got, err := lib.Videos(mustRel(t, lib, "S"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"S/ep2.mkv", "S/ep10.mkv"}
	if len(got) != len(want) {
		t.Fatalf("Videos = %v, want %v", strs(got), want)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Fatalf("Videos = %v, want %v", strs(got), want)
		}
	}
}

func mustRel(t *testing.T, l *Library, p string) outpath.Rel {
	t.Helper()
	r, err := l.m.ParseRel(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The converted tree mirrors the library, so the same relative path means the
// same show on both sides. What must not appear is work in progress: a .part
// has no index and cannot be played, and the diagnostic clips are test output
// nobody asked to keep.
func TestListOutputHidesWhatIsNotFinished(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "media")
	out := filepath.Join(base, "out")
	for _, d := range []string{src, filepath.Join(out, "S"), filepath.Join(out, "_airplay", "ab12")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{
		"ep1.mp4", "ep10.mp4", "ep2.mp4", "ep3.mp4.part", ".hidden.mp4",
	} {
		if err := os.WriteFile(filepath.Join(out, "S", f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(out, "_airplay", "ab12", "a-faststart.mp4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := outpath.NewMapper(src, out, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	lib := New(m)

	top, err := lib.ListOutput(outpath.Rel{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range top.Entries {
		if e.Name == airplayDir {
			t.Error("the diagnostic clips are listed as library content")
		}
	}

	dir, err := m.ParseRel("S")
	if err != nil {
		t.Fatal(err)
	}
	listing, err := lib.ListOutput(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range listing.Entries {
		names = append(names, e.Name)
	}
	want := []string{"ep1.mp4", "ep2.mp4", "ep10.mp4"} // natural order, no .part, no dotfile
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v, want %v", names, want)
		}
	}
}
