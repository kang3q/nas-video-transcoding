package ffmpeg

import (
	"fmt"
	"strconv"
)

// ProbeOpts renders a short diagnostic clip instead of the normal output.
//
// It exists because AirPlay either plays a file or does not, and says nothing
// about why. Encoding a handful of short clips that differ in one property
// each turns an unanswerable question into a ladder: whichever rung it starts
// working on is the property that mattered.
//
// Everything here is deliberately plain — no subtitles, no tee, no live
// rendition. Each of those is a way for the test to fail for a reason that has
// nothing to do with what is being tested.
type ProbeOpts struct {
	StartSec float64 // where in the source to take the clip
	Secs     float64 // how much of it

	Profile string // H.264 profile: baseline, main, high
	Level   string // H.264 level: "3.0" … "4.2"

	// MaxHeight scales the picture down to fit, keeping the aspect ratio.
	// Zero leaves it alone.
	MaxHeight int

	VideoBitrate string
	AudioBitrate string
}

// probeArgs builds the command for one variant. Every one of them writes a
// progressive MP4 with the index at the front, because that is the shape Apple
// asks for and the shape the normal live-preview path cannot produce.
func probeArgs(s Spec, set Settings) []string {
	p := s.Probe
	a := []string{"-nostdin", "-hide_banner", "-loglevel", "warning", "-y"}

	// Seeking before -i decodes from the preceding keyframe and throws the
	// rest away, which is both fast and accurate enough for a clip whose only
	// job is to exist.
	if p.StartSec > 0 {
		a = append(a, "-ss", strconv.FormatFloat(p.StartSec, 'f', 2, 64))
	}
	a = append(a, "-fflags", "+genpts", "-i", s.Src)
	if p.Secs > 0 {
		a = append(a, "-t", strconv.FormatFloat(p.Secs, 'f', 2, 64))
	}

	a = append(a, "-map", fmt.Sprintf("0:%d", s.VideoIndex))
	if s.AudioIndex >= 0 {
		a = append(a, "-map", fmt.Sprintf("0:%d", s.AudioIndex))
	}
	a = append(a, "-sn", "-dn")

	if set.Threads > 0 {
		a = append(a, "-threads", strconv.Itoa(set.Threads))
	}
	if p.MaxHeight > 0 {
		// -2 keeps the aspect ratio and lands on an even width, which H.264
		// requires. min() leaves anything already smaller untouched.
		a = append(a, "-vf", fmt.Sprintf("scale=-2:'min(%d,ih)'", p.MaxHeight))
	}
	a = append(a,
		"-c:v", "libx264",
		"-preset", set.Preset,
		"-profile:v", p.Profile,
		"-level", p.Level,
		"-pix_fmt", "yuv420p",
		"-b:v", p.VideoBitrate,
		"-fps_mode", "vfr",
	)
	if s.AudioIndex >= 0 {
		a = append(a,
			"-c:a", "aac",
			"-b:a", p.AudioBitrate,
			"-ac", "2",
			"-ar", "48000",
		)
	}
	return append(a,
		"-max_muxing_queue_size", "4096",
		"-stats_period", "1", "-nostats", "-progress", "pipe:1",
		"-movflags", "+faststart",
		"-f", "mp4", s.Dst,
	)
}
