package subs

import (
	"strings"
	"testing"
)

// Safari places a subtitle from inside an MP4 wherever the track says, which
// put it in the corner. A WebVTT file offered to the page is placed by the
// browser, which puts it where subtitles go.
func TestSRTBecomesWebVTT(t *testing.T) {
	const srt = "1\r\n00:00:01,000 --> 00:00:03,500\r\n류크, 사과 줄까?\r\n\r\n" +
		"2\r\n00:01:02,004 --> 00:01:05,000\r\n두 번째 줄\r\n"

	got := string(srtToVTT([]byte(srt)))

	if !strings.HasPrefix(got, "WEBVTT\n\n") {
		t.Errorf("no WEBVTT header:\n%s", got)
	}
	// The decimal mark is the difference that matters.
	if !strings.Contains(got, "00:00:01.000 --> 00:00:03.500") {
		t.Errorf("timestamps were not converted:\n%s", got)
	}
	if strings.Contains(got, ",500") {
		t.Errorf("an SRT comma survived:\n%s", got)
	}
	for _, want := range []string{"류크, 사과 줄까?", "두 번째 줄"} {
		if !strings.Contains(got, want) {
			t.Errorf("lost the text %q:\n%s", want, got)
		}
	}
	// Carriage returns from a Windows-written file must not reach the parser.
	if strings.Contains(got, "\r") {
		t.Errorf("carriage returns survived:\n%q", got)
	}
	// The cue counter is SRT's; WebVTT does not need it, and a stray number
	// is one more thing for a parser to trip on.
	for _, line := range strings.Split(got, "\n") {
		if line == "1" || line == "2" {
			t.Errorf("a cue number was carried over:\n%s", got)
		}
	}
}

// A comma inside the subtitle text is not a timestamp and must survive.
func TestVTTKeepsPunctuationInTheText(t *testing.T) {
	got := string(srtToVTT([]byte("1\n00:00:01,000 --> 00:00:02,000\n1,000명이 왔다\n")))
	if !strings.Contains(got, "1,000명이 왔다") {
		t.Errorf("the text was mangled:\n%s", got)
	}
}
