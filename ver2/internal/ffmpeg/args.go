// Package ffmpeg builds and runs the conversion, and reports how far along it
// is.
//
// The encoding settings come from a command the user had already been running
// by hand for months, so they are treated as given. What changed, and why:
//
//   - "-map 0:0 -map 0:1" became "-map 0:v:0 -map 0:a:0?". The original assumes
//     stream 0 is the video and stream 1 the audio. Real files put subtitles,
//     attachments or a second audio track in between, and then the command
//     either encodes the wrong track or dies.
//   - "-vsync 2" became "-fps_mode vfr", its documented replacement. Same
//     behaviour; the old spelling warns on ffmpeg 6 and is gone in 7.
//   - "-force_key_frames" is new, and only matters when writing HLS: x264's
//     default keyframe interval is around ten seconds, so without it the
//     segment length is ignored.
//   - The original replaced the source file in place. This one writes to a
//     mirrored output tree and never touches the original.
package ffmpeg

import (
	"fmt"
	"strconv"
	"strings"
)

// Settings are the encoder knobs, from config.
type Settings struct {
	Threads       int
	Preset        string
	VideoBitrate  string
	AudioBitrate  string
	AudioChannels int
	AudioRate     int
}

// Spec is one conversion.
type Spec struct {
	Src string // absolute source path
	Dst string // absolute path to write, normally the ".part" file

	// Remux skips the encoders entirely: the file already holds what we want
	// and only its container is wrong. Seconds instead of an hour.
	Remux bool

	// VideoIndex and AudioIndex select tracks by their absolute stream index,
	// taken from ffprobe. AudioIndex < 0 means the file has no audio.
	VideoIndex int
	AudioIndex int

	// BurnSubs renders a subtitle file into the picture. The path must already
	// be somewhere safe and simple — see [FilterPath] for why.
	BurnSubs string

	// LiveDir, when set, also writes an HLS rendition there so the job can be
	// watched before it finishes. The encode still happens once.
	LiveDir     string
	SegmentSecs int
}

// Args assembles the command line.
func Args(s Spec, set Settings) []string {
	a := []string{
		"-nostdin", "-hide_banner", "-loglevel", "warning", "-y",
		"-fflags", "+genpts",
		"-i", s.Src,
	}

	a = append(a, "-map", fmt.Sprintf("0:%d", s.VideoIndex))
	if s.AudioIndex >= 0 {
		a = append(a, "-map", fmt.Sprintf("0:%d", s.AudioIndex))
	}
	a = append(a, "-map_metadata", "0")

	switch {
	case s.Remux && s.BurnSubs == "":
		// Nothing to re-encode. Put the index at the front while we are here,
		// so a player can start without reading the end of the file first.
		a = append(a, "-c", "copy", "-movflags", "+faststart")
	default:
		if set.Threads > 0 {
			a = append(a, "-threads", strconv.Itoa(set.Threads))
		}
		if s.BurnSubs != "" {
			a = append(a, "-vf", "subtitles="+FilterPath(s.BurnSubs))
		}
		a = append(a,
			"-c:v", "libx264",
			"-preset", set.Preset,
			"-profile:v", "main",
			"-level", "4.0",
			"-pix_fmt", "yuv420p",
			"-b:v", set.VideoBitrate,
			"-fps_mode", "vfr",
		)
		if s.LiveDir != "" {
			secs := s.SegmentSecs
			if secs <= 0 {
				secs = 4
			}
			a = append(a, "-force_key_frames",
				fmt.Sprintf("expr:gte(t,n_forced*%d)", secs))
		}
		if s.AudioIndex >= 0 {
			a = append(a,
				"-c:a", "aac",
				"-b:a", set.AudioBitrate,
				"-ac", strconv.Itoa(set.AudioChannels),
				"-ar", strconv.Itoa(set.AudioRate),
			)
		}
	}

	a = append(a, "-max_muxing_queue_size", "4096",
		"-stats_period", "1", "-nostats", "-progress", "pipe:1")

	if s.LiveDir == "" {
		return append(a, "-f", "mp4", s.Dst)
	}
	return append(a, "-f", "tee", teeSpec(s))
}

// teeSpec writes the MP4 and the HLS rendition from a single encode. Listing
// two outputs the ordinary way would instantiate two encoders, which on this
// hardware would double an already slow job.
func teeSpec(s Spec) string {
	secs := s.SegmentSecs
	if secs <= 0 {
		secs = 4
	}
	hls := strings.Join([]string{
		// The MP4 must survive the HLS branch failing, so only the HLS branch
		// is allowed to fail quietly, and it runs on its own thread.
		"onfail=ignore",
		"use_fifo=1",
		"f=hls",
		"hls_time=" + strconv.Itoa(secs),
		"hls_list_size=0",
		"hls_playlist_type=event",
		"hls_segment_type=fmp4",
		"hls_fmp4_init_filename=init.mp4",
		// temp_file renames each segment into place, so a player can never
		// fetch one that is half written.
		"hls_flags=independent_segments+temp_file",
		"hls_segment_filename=" + s.LiveDir + "/seg%05d.m4s",
	}, ":")

	return fmt.Sprintf("[f=mp4]%s|[%s]%s/index.m3u8", s.Dst, hls, s.LiveDir)
}

// FilterPath escapes a path for use inside a filter argument. ffmpeg's filter
// syntax reads ":" as an option separator and "[" as a stream label, and
// filenames in a real library are full of both — "Show - 24 [1080p].mkv" is
// typical. Callers should still copy subtitles to a plain temporary name and
// pass that; this exists so a mistake degrades into an error rather than into
// a misparsed command.
func FilterPath(p string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`:`, `\:`,
		`'`, `\'`,
		`[`, `\[`,
		`]`, `\]`,
		`,`, `\,`,
		`;`, `\;`,
	)
	return r.Replace(p)
}
