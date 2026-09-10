package subs

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// A cue with no end is the normal way a SAMI file finishes.
//
// SAMI marks when a line appears, not when it goes away, so the last one is
// closed by whatever comes next — and at the end of the file, nothing does.
// ffmpeg carries that through as a duration it never resolves, and the MP4
// muxer refuses it: "Application provided duration: 4294967295000 is
// invalid". That is UINT32_MAX milliseconds, about fifty days, and it
// arrives at the very end of the encode, so an hour of work fails at
// ninety-something percent.
//
// The repair is to give such a cue an ordinary length. Which length hardly
// matters — it is the last line of an episode, over the closing credits —
// so long as it is finite.
const (
	// maxCue is longer than any real subtitle line and far shorter than the
	// nonsense a missing end time produces.
	maxCue = 30 * 1000 // milliseconds
	// fallbackCue is what a broken one becomes.
	fallbackCue = 4 * 1000
)

// repairSRT rewrites cue timings that no muxer will accept. It returns how
// many it had to change, for the log: silently repairing a file is how the
// next surprise gets built.
func repairSRT(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}

	fixed := 0
	lines := bytes.Split(raw, []byte("\n"))
	for i, line := range lines {
		start, end, ok := parseCueTimes(string(line))
		if !ok {
			continue
		}
		switch {
		case end <= start:
			end = start + fallbackCue
		case end-start > maxCue:
			end = start + fallbackCue
		default:
			continue
		}
		lines[i] = []byte(formatCueTimes(start, end))
		fixed++
	}
	if fixed == 0 {
		return 0, nil
	}
	if err := os.WriteFile(path, bytes.Join(lines, []byte("\n")), 0o644); err != nil {
		return 0, err
	}
	return fixed, nil
}

// parseCueTimes reads an SRT timing line, "00:01:02,500 --> 00:01:05,000",
// into milliseconds. Anything else is not a timing line.
func parseCueTimes(line string) (start, end int64, ok bool) {
	left, right, found := strings.Cut(strings.TrimSpace(line), "-->")
	if !found {
		return 0, 0, false
	}
	start, ok = parseTimestamp(strings.TrimSpace(left))
	if !ok {
		return 0, 0, false
	}
	// A timing line can carry position settings after the end time; they are
	// not ours to interpret, but they are also not part of the timestamp.
	rightText := strings.TrimSpace(right)
	if i := strings.IndexAny(rightText, " \t"); i > 0 {
		rightText = rightText[:i]
	}
	end, ok = parseTimestamp(rightText)
	return start, end, ok
}

func parseTimestamp(s string) (int64, bool) {
	// hh:mm:ss,mmm — the comma is SRT's decimal mark, but files written by
	// other tools use a full stop, and rejecting those would mean rejecting
	// the file.
	s = strings.Replace(s, ".", ",", 1)
	clock, millis, ok := strings.Cut(s, ",")
	if !ok {
		return 0, false
	}
	parts := strings.Split(clock, ":")
	if len(parts) != 3 {
		return 0, false
	}
	var total int64
	for _, p := range parts {
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil || n < 0 {
			return 0, false
		}
		total = total*60 + n
	}
	ms, err := strconv.ParseInt(millis, 10, 64)
	if err != nil || ms < 0 || ms > 999 {
		return 0, false
	}
	return total*1000 + ms, true
}

func formatCueTimes(start, end int64) string {
	return fmt.Sprintf("%s --> %s", formatTimestamp(start), formatTimestamp(end))
}

func formatTimestamp(ms int64) string {
	h := ms / 3600000
	m := ms / 60000 % 60
	s := ms / 1000 % 60
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms%1000)
}
