package vfs

import (
	"testing"

	"nvt/internal/probe"
)

func file(name string, a probe.Action) entry {
	return entry{virtName: name, plan: probe.Plan{Action: a}}
}

func TestAssignNames(t *testing.T) {
	cases := []struct {
		name string
		in   []entry
		want []string
	}{
		{
			"converted files take the .mkv extension",
			[]entry{file("Movie.avi", probe.AudioOnly), file("Other.mp4", probe.Passthrough)},
			[]string{"Movie.mkv", "Other.mp4"},
		},
		{
			"an .mkv that needs work keeps its name",
			[]entry{file("Movie.mkv", probe.AudioOnly)},
			[]string{"Movie.mkv"},
		},
		{
			"a collision with a real file is disambiguated",
			[]entry{file("Movie.avi", probe.AudioOnly), file("Movie.mkv", probe.Passthrough)},
			[]string{"Movie (nvt).mkv", "Movie.mkv"},
		},
		{
			"directories are left alone",
			[]entry{{virtName: "Season 1", isDir: true}},
			[]string{"Season 1"},
		},
		{
			"non-media files keep their extension",
			[]entry{file("Movie.srt", probe.Passthrough), file("Movie.avi", probe.AudioOnly)},
			[]string{"Movie.srt", "Movie.mkv"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ents := append([]entry(nil), tc.in...)
			assignNames(ents)
			for i := range ents {
				if ents[i].virtName != tc.want[i] {
					t.Errorf("entry %d = %q, want %q", i, ents[i].virtName, tc.want[i])
				}
			}
		})
	}
}
