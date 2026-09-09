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

	// Width and Height are the source's, and decide whether it has to be
	// scaled down. H.264 above 1080p is outside what an Apple TV will play,
	// and writing a 4K frame with a level that claims otherwise produces a
	// file its decoder rejects outright.
	Width  int
	Height int

	// LiveDir, when set, also writes an HLS rendition there so the job can be
	// watched before it finishes. The encode still happens once.
	LiveDir     string
	SegmentSecs int

	// Probe, when set, replaces all of the above encoding decisions with one
	// short diagnostic clip. See [ProbeOpts].
	Probe *ProbeOpts
}

// Args assembles the command line.
func Args(s Spec, set Settings) []string {
	if s.Probe != nil {
		return probeArgs(s, set)
	}
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

	// Subtitles and data streams are already excluded by mapping only the two
	// streams we want, but saying so costs nothing and survives someone
	// changing the mapping later.
	a = append(a, "-sn", "-dn")

	switch {
	case s.Remux && s.BurnSubs == "":
		a = append(a, "-c", "copy")
	default:
		if set.Threads > 0 {
			a = append(a, "-threads", strconv.Itoa(set.Threads))
		}
		if vf := videoFilter(s); vf != "" {
			a = append(a, "-vf", vf)
		}
		a = append(a,
			"-c:v", "libx264",
			"-preset", set.Preset,
			// High profile and level 4.2 are what an Apple TV expects for
			// 1080p H.264. The level has to match what is actually written:
			// claiming 4.0 over a larger frame is a malformed file, and
			// Apple's decoder checks.
			"-profile:v", "high",
			"-level", "4.2",
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
		// faststart moves the index to the front of the file. Without it a
		// player has to fetch the end before it can begin, which over a
		// network means a long stall or a timeout on a large file — and
		// network playback is the entire point here. With tee it goes on the
		// mp4 branch instead; see teeSpec.
		return append(a, "-movflags", "+faststart", "-f", "mp4", s.Dst)
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

	// No faststart on this branch. It works by seeking back at the end, and
	// whether tee gives its slaves a seekable output is unverified — a failure
	// here would lose the artifact, which is the one thing that must survive.
	return fmt.Sprintf("[f=mp4]%s|[%s]%s/index.m3u8", s.Dst, hls, s.LiveDir)
}

// videoFilter builds the -vf chain. Scaling comes before subtitles so libass
// draws text at the size it will actually be shown, rather than having it
// resampled afterwards.
func videoFilter(s Spec) string {
	var parts []string
	if s.Width > 1920 || s.Height > 1080 {
		// Cap the width at 1920; -2 derives a height that keeps the aspect
		// ratio and stays even, which H.264 requires.
		parts = append(parts, "scale='min(1920,iw)':-2")
	}
	if s.BurnSubs != "" {
		parts = append(parts, "subtitles="+FilterPath(s.BurnSubs))
	}
	return strings.Join(parts, ",")
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
