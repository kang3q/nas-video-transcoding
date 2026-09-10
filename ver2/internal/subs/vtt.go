package subs

import (
	"bytes"
	"fmt"
	"os"
	"strings"
)

// WebVTT exists here because a subtitle inside an MP4 and a subtitle beside
// a web page are read by different things, and only one of them gets the
// position right.
//
// An Apple TV plays the track inside the file and places it where its own
// subtitle settings say. Safari does not: it reads the same track, converts
// the cues itself, and honours the position recorded in them — which lands
// the text in the bottom right corner on an iPhone and outside the picture
// on a Mac. Chrome does not read in-file subtitles at all.
//
// A WebVTT file offered to the page as a <track> is positioned by the
// browser's own rules and works in all three. It cannot replace the track
// inside the file, because AirPlay hands the television a URL and nothing
// else — the page's tracks do not travel. So both are written, and each is
// used where it works.

// WriteVTT converts an SRT file to WebVTT beside a converted video.
func WriteVTT(srtPath, vttPath string) error {
	raw, err := os.ReadFile(srtPath)
	if err != nil {
		return err
	}
	return os.WriteFile(vttPath, srtToVTT(raw), 0o644)
}

// srtToVTT is a conversion of punctuation, near enough. The two formats
// agree on cue text and differ on the decimal mark in a timestamp and on
// needing a header line.
func srtToVTT(srt []byte) []byte {
	var out bytes.Buffer
	out.WriteString("WEBVTT\n\n")

	for _, line := range bytes.Split(bytes.TrimRight(srt, "\n"), []byte("\n")) {
		text := strings.TrimRight(string(line), "\r")
		if start, end, ok := parseCueTimes(text); ok {
			fmt.Fprintf(&out, "%s --> %s\n", vttTime(start), vttTime(end))
			continue
		}
		// A bare number is SRT's cue counter. WebVTT allows an identifier
		// there but does not need one, and a stray number is one more thing
		// for a parser to be unhappy about.
		if isCueNumber(text) {
			continue
		}
		out.WriteString(text)
		out.WriteByte('\n')
	}
	return out.Bytes()
}

func isCueNumber(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// vttTime is an SRT timestamp with a full stop instead of a comma.
func vttTime(ms int64) string {
	return strings.Replace(formatTimestamp(ms), ",", ".", 1)
}
