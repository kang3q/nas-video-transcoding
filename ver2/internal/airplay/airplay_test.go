package airplay

import (
	"strings"
	"testing"

	"nvt/ver2/internal/outpath"
)

// fakeRel builds a Rel the way a request would, through the only constructor.
func fakeRel(p string) outpath.Rel {
	m, err := outpath.NewMapper(".", ".", "")
	if err != nil {
		panic(err)
	}
	defer m.Close()
	r, err := m.ParseRel(p)
	if err != nil {
		panic(err)
	}
	return r
}

// The variants only mean something as a ladder: each rung has to give up
// something the one above it kept, or the answer "it started working here"
// points at nothing in particular.
func TestTheVariantsFormALadder(t *testing.T) {
	rank := map[string]int{"high": 3, "main": 2, "baseline": 1}

	if len(Variants) != 4 {
		t.Fatalf("got %d variants, want 4", len(Variants))
	}
	for i, v := range Variants {
		if v.Name == "" || v.Title == "" || v.Gives == "" {
			t.Errorf("variant %d is missing its copy: %+v", i, v)
		}
		if strings.ContainsAny(v.Name, " /\\") || v.Name != strings.ToLower(v.Name) {
			t.Errorf("variant name %q is not a plain ASCII stem", v.Name)
		}
		if rank[v.Profile] == 0 {
			t.Errorf("variant %q has an unknown profile %q", v.Name, v.Profile)
		}
		if i == 0 {
			continue
		}
		prev := Variants[i-1]
		if rank[v.Profile] > rank[prev.Profile] {
			t.Errorf("%q asks for more than %q: %s after %s",
				v.Name, prev.Name, v.Profile, prev.Profile)
		}
		if prev.MaxHeight != 0 && (v.MaxHeight == 0 || v.MaxHeight > prev.MaxHeight) {
			t.Errorf("%q is not smaller than %q: %d after %d",
				v.Name, prev.Name, v.MaxHeight, prev.MaxHeight)
		}
	}

	// The first rung has to be what is already produced, or "it works now"
	// tells us nothing about the file that does not.
	if first := Variants[0]; first.Profile != "high" || first.Level != "4.2" || first.MaxHeight != 0 {
		t.Errorf("the first rung is not the current settings: %+v", first)
	}
}

// The clips have to be reachable by a path with nothing in it that could
// itself be what AirPlay dislikes.
func TestTheDirectoryIsPlainASCII(t *testing.T) {
	for _, in := range []string{
		"애니/데스노트/DEATH NOTE 데스노트 03 (704x396 DivX).avi",
		"a.mkv",
	} {
		got := Dir(fakeRel(in))
		for _, r := range got {
			if r > 127 {
				t.Errorf("Dir(%q) = %q, which is not ASCII", in, got)
				break
			}
		}
		if !strings.HasPrefix(got, Root+"/") {
			t.Errorf("Dir(%q) = %q, want it under %q", in, got, Root)
		}
	}
	if Dir(fakeRel("a.mkv")) == Dir(fakeRel("b.mkv")) {
		t.Error("two files share a directory")
	}
}

// A clip taken from the very start is often black or a still, which makes
// "is it playing?" harder to answer than it needs to be.
func TestStartSecSkipsTheOpeningOfALongFile(t *testing.T) {
	if got := StartSec(1440); got != ClipSecs {
		t.Errorf("StartSec(24min) = %v, want %v", got, float64(ClipSecs))
	}
	if got := StartSec(30); got != 0 {
		t.Errorf("StartSec(30s) = %v, want 0 — there is nowhere to skip to", got)
	}
	// The clip has to fit: starting late enough to run off the end would make
	// a short file produce nothing at all.
	for _, d := range []float64{0, 10, 61, 179, 180, 600, 1440} {
		if at := StartSec(d); d > 0 && at+ClipSecs > d && at != 0 {
			t.Errorf("StartSec(%v) = %v, which runs past the end", d, at)
		}
	}
}
