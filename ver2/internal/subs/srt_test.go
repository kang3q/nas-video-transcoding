package subs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SAMI marks when a line appears, never when it goes away, so the last cue of
// a file is closed by nothing. ffmpeg carries that through as a duration it
// never resolves and the MP4 muxer refuses it — at the very end of the
// encode, which is how an hour of work fails at ninety-something percent.
func TestRepairSRTBoundsAnEndlessCue(t *testing.T) {
	const in = `1
00:00:01,000 --> 00:00:03,500
류크, 사과 줄까?

2
00:00:04,000 --> 49:17:15,295
마지막 줄
`
	path := filepath.Join(t.TempDir(), "a.srt")
	if err := os.WriteFile(path, []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}

	n, err := repairSRT(path)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("repaired %d cues, want 1", n)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "00:00:04,000 --> 00:00:08,000") {
		t.Errorf("the endless cue was not bounded:\n%s", got)
	}
	// Everything else survives untouched — the text, and a cue that was fine.
	if !strings.Contains(string(got), "00:00:01,000 --> 00:00:03,500") {
		t.Errorf("a good cue was rewritten:\n%s", got)
	}
	if !strings.Contains(string(got), "류크, 사과 줄까?") ||
		!strings.Contains(string(got), "마지막 줄") {
		t.Errorf("the text was damaged:\n%s", got)
	}
}

// A cue that ends before it starts is the other way a muxer is handed
// something it cannot use.
func TestRepairSRTFixesABackwardsCue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.srt")
	if err := os.WriteFile(path, []byte(
		"1\n00:00:10,000 --> 00:00:02,000\n뒤집힌 줄\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := repairSRT(path); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "00:00:10,000 --> 00:00:14,000") {
		t.Errorf("not fixed:\n%s", got)
	}
}

// A file that is already fine must come out byte for byte the same. Rewriting
// it would risk more than it fixes.
func TestRepairSRTLeavesAGoodFileAlone(t *testing.T) {
	const in = "1\n00:00:01,000 --> 00:00:03,500\n괜찮은 줄\n\n2\n00:01:00,000 --> 00:01:25,000\n긴 줄이지만 정상\n"
	path := filepath.Join(t.TempDir(), "a.srt")
	if err := os.WriteFile(path, []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := repairSRT(path)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("changed %d cues in a file that needed none", n)
	}
	got, _ := os.ReadFile(path)
	if string(got) != in {
		t.Errorf("the file was rewritten:\n%q", got)
	}
}

func TestParseCueTimes(t *testing.T) {
	cases := []struct {
		in         string
		start, end int64
		ok         bool
	}{
		{"00:00:01,000 --> 00:00:03,500", 1000, 3500, true},
		{"01:02:03,004 --> 01:02:04,005", 3723004, 3724005, true},
		{"00:00:01.000 --> 00:00:02.000", 1000, 2000, true}, // full stop, seen in the wild
		{"00:00:01,000 --> 00:00:02,000 line:90%", 1000, 2000, true},
		{"1", 0, 0, false},
		{"류크, 사과 줄까?", 0, 0, false},
		{"", 0, 0, false},
		{"00:00:01,000 -->", 0, 0, false},
		{"aa:bb:cc,ddd --> 00:00:02,000", 0, 0, false},
	}
	for _, tc := range cases {
		start, end, ok := parseCueTimes(tc.in)
		if ok != tc.ok || (ok && (start != tc.start || end != tc.end)) {
			t.Errorf("parseCueTimes(%q) = %d, %d, %v; want %d, %d, %v",
				tc.in, start, end, ok, tc.start, tc.end, tc.ok)
		}
	}
}
