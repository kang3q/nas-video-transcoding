package ffmpeg

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testSettings = Settings{
	Threads: 3, Preset: "superfast",
	VideoBitrate: "2600k", AudioBitrate: "320k",
	AudioChannels: 2, AudioRate: 48000,
}

func argLine(a []string) string { return " " + strings.Join(a, " ") + " " }

func TestArgsEncodePath(t *testing.T) {
	got := argLine(Args(Spec{
		Src: "/media/a.mkv", Dst: "/out/a.mp4.part",
		VideoIndex: 0, AudioIndex: 1,
	}, testSettings))

	for _, want := range []string{
		" -map 0:0 ", " -map 0:1 ", " -sn ", " -dn ",
		" -c:v libx264 ", " -preset superfast ", " -b:v 2600k ",
		" -profile:v high ", " -level 4.2 ",
		" -c:a aac ", " -b:a 320k ", " -ac 2 ", " -ar 48000 ",
		" -threads 3 ", " -fps_mode vfr ", " -pix_fmt yuv420p ",
		" -progress pipe:1 ", " -movflags +faststart ", " -f mp4 /out/a.mp4.part ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// The spelling that ffmpeg 7 removed must not come back.
	if strings.Contains(got, " -vsync ") {
		t.Error("-vsync is deprecated; -fps_mode replaced it")
	}
	// Keyframe forcing costs compression and only earns its keep for HLS.
	if strings.Contains(got, "force_key_frames") {
		t.Error("keyframes forced with no live output to segment")
	}
}

// The whole point of probing: a file that already holds what we want should
// not go near an encoder.
func TestArgsRemuxPath(t *testing.T) {
	got := argLine(Args(Spec{
		Src: "/media/a.mkv", Dst: "/out/a.mp4.part",
		Remux: true, VideoIndex: 0, AudioIndex: 1,
	}, testSettings))

	if !strings.Contains(got, " -c copy ") {
		t.Errorf("remux is not copying:\n%s", got)
	}
	if strings.Contains(got, "libx264") || strings.Contains(got, " -c:a aac ") {
		t.Errorf("remux invoked an encoder:\n%s", got)
	}
	if !strings.Contains(got, " -movflags +faststart ") {
		t.Errorf("remux should move the index to the front:\n%s", got)
	}
}

// Without faststart a player has to fetch the end of the file before it can
// start, which over a network is a long stall or a timeout. Every path that
// writes a plain MP4 needs it, not just the cheap one.
func TestArgsAlwaysMovesTheIndexToTheFront(t *testing.T) {
	for _, remux := range []bool{true, false} {
		got := argLine(Args(Spec{
			Src: "/media/a.mkv", Dst: "/out/a.mp4.part",
			Remux: remux, VideoIndex: 0, AudioIndex: 1,
		}, testSettings))
		if !strings.Contains(got, " -movflags +faststart ") {
			t.Errorf("remux=%v is missing faststart:\n%s", remux, got)
		}
	}
}

// H.264 above 1080p is outside what an Apple TV plays, and a level that does
// not match the frame it describes is a malformed file its decoder refuses.
func TestArgsScales4KDownTo1080p(t *testing.T) {
	got := argLine(Args(Spec{
		Src: "/media/a.mkv", Dst: "/out/a.mp4.part",
		VideoIndex: 0, AudioIndex: 1, Width: 3840, Height: 2160,
	}, testSettings))

	if !strings.Contains(got, "scale='min(1920,iw)':-2") {
		t.Errorf("a 4K source was not scaled down:\n%s", got)
	}
	if !strings.Contains(got, " -level 4.2 ") {
		t.Errorf("level is not 4.2:\n%s", got)
	}
}

func TestArgsLeaves1080pAlone(t *testing.T) {
	got := argLine(Args(Spec{
		Src: "/media/a.mkv", Dst: "/out/a.mp4.part",
		VideoIndex: 0, AudioIndex: 1, Width: 1920, Height: 1080,
	}, testSettings))
	if strings.Contains(got, "scale=") {
		t.Errorf("1080p was scaled for no reason:\n%s", got)
	}
}

// Scaling has to come first so libass draws text at the size it will be shown.
func TestArgsScalesBeforeBurningSubtitles(t *testing.T) {
	got := argLine(Args(Spec{
		Src: "/media/a.mkv", Dst: "/out/a.mp4.part",
		VideoIndex: 0, AudioIndex: 1, Width: 3840, Height: 2160,
		BurnSubs: "/tmp/j.ass",
	}, testSettings))

	scale := strings.Index(got, "scale=")
	subs := strings.Index(got, "subtitles=")
	if scale < 0 || subs < 0 {
		t.Fatalf("filter chain incomplete:\n%s", got)
	}
	if scale > subs {
		t.Error("subtitles are drawn before the scale, so the text gets resampled")
	}
}

// Drawing subtitles into the picture means the picture has to be redrawn, so
// the copy shortcut is off even when the codecs would have allowed it.
func TestArgsBurningSubtitlesForcesAnEncode(t *testing.T) {
	got := argLine(Args(Spec{
		Src: "/media/a.mkv", Dst: "/out/a.mp4.part",
		Remux: true, VideoIndex: 0, AudioIndex: 1,
		BurnSubs: "/tmp/job1.ass",
	}, testSettings))

	if strings.Contains(got, " -c copy ") {
		t.Errorf("subtitles were burned into a stream copy:\n%s", got)
	}
	if !strings.Contains(got, " -vf subtitles=/tmp/job1.ass ") {
		t.Errorf("subtitle filter missing:\n%s", got)
	}
	if !strings.Contains(got, " -c:v libx264 ") {
		t.Errorf("expected a re-encode:\n%s", got)
	}
}

func TestArgsNoAudio(t *testing.T) {
	got := argLine(Args(Spec{
		Src: "/media/a.mkv", Dst: "/out/a.mp4.part",
		VideoIndex: 0, AudioIndex: -1,
	}, testSettings))

	if strings.Contains(got, " -c:a ") || strings.Contains(got, " -b:a ") {
		t.Errorf("audio options emitted for a file with no audio:\n%s", got)
	}
	if strings.Contains(got, " -map 0:-1 ") {
		t.Errorf("mapped a nonexistent stream:\n%s", got)
	}
}

// Two outputs listed the ordinary way would run two encoders. tee runs one.
func TestArgsLiveUsesTee(t *testing.T) {
	got := Args(Spec{
		Src: "/media/a.mkv", Dst: "/out/a.mp4.part",
		VideoIndex: 0, AudioIndex: 1,
		LiveDir: "/state/live/j1", SegmentSecs: 4,
	}, testSettings)
	line := argLine(got)

	if !strings.Contains(line, " -f tee ") {
		t.Fatalf("live output is not using tee:\n%s", line)
	}
	spec := got[len(got)-1]
	if !strings.Contains(spec, "[f=mp4]/out/a.mp4.part|") {
		t.Errorf("the mp4 branch is missing or not first: %s", spec)
	}
	if !strings.Contains(spec, "/state/live/j1/index.m3u8") {
		t.Errorf("the hls branch is missing: %s", spec)
	}
	// The MP4 is the artifact; only the live branch may fail quietly.
	if !strings.Contains(spec, "onfail=ignore:use_fifo=1:f=hls") {
		t.Errorf("hls branch is not isolated: %s", spec)
	}
	if strings.Contains(spec, "[f=mp4:onfail=ignore]") {
		t.Error("the mp4 branch must not be allowed to fail quietly")
	}
	if !strings.Contains(spec, "hls_flags=independent_segments+temp_file") {
		t.Errorf("segments could be served half-written: %s", spec)
	}
	if !strings.Contains(line, " -force_key_frames expr:gte(t,n_forced*4) ") {
		t.Errorf("without forced keyframes hls_time is ignored:\n%s", line)
	}
}

// Library filenames are full of the characters ffmpeg's filter parser treats
// as syntax.
func TestFilterPathEscapes(t *testing.T) {
	got := FilterPath(`/tmp/Show - 24 [1080p]:x.ass`)
	for _, raw := range []string{`[`, `]`} {
		if strings.Contains(strings.ReplaceAll(got, `\`+raw, ""), raw) {
			t.Errorf("%q left unescaped in %q", raw, got)
		}
	}
	if strings.Contains(strings.ReplaceAll(got, `\:`, ""), ":") {
		t.Errorf("colon left unescaped in %q", got)
	}
}

// --- progress ---

const progressBlocks = `frame=717
fps=17.02
stream_0_0_q=18.0
bitrate=N/A
total_size=N/A
out_time_us=29820000
out_time_ms=29820000
out_time=00:00:29.820000
dup_frames=0
drop_frames=0
speed=0.717x
progress=continue
frame=1434
fps=17.10
out_time_us=59700000
speed=0.72x
progress=continue
frame=41663
fps=17.00
out_time_us=1737768000
speed=0.717x
progress=end
`

func TestParseProgress(t *testing.T) {
	var got []Progress
	if err := ParseProgress(strings.NewReader(progressBlocks), func(p Progress) {
		got = append(got, p)
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d blocks, want 3", len(got))
	}

	if got[0].OutTime != 29820*time.Millisecond {
		t.Errorf("OutTime = %v, want 29.82s", got[0].OutTime)
	}
	if got[0].Frame != 717 || got[0].FPS != 17.02 || got[0].Speed != 0.717 {
		t.Errorf("first block = %+v", got[0])
	}
	if got[0].Done || got[1].Done {
		t.Error("a continue block was reported as the end")
	}
	if !got[2].Done {
		t.Error("the end block was not marked done")
	}
	if got[2].OutTime != 1737768*time.Millisecond {
		t.Errorf("final OutTime = %v", got[2].OutTime)
	}
}

// Nothing may be emitted from a block that has not been terminated, or the UI
// shows a value assembled from two different moments.
func TestParseProgressWaitsForTheBlockTerminator(t *testing.T) {
	var got []Progress
	ParseProgress(strings.NewReader("frame=10\nout_time_us=1000000\n"), func(p Progress) {
		got = append(got, p)
	})
	if len(got) != 0 {
		t.Errorf("emitted %d blocks from an unterminated one", len(got))
	}
}

func TestParseProgressToleratesNA(t *testing.T) {
	var got []Progress
	ParseProgress(strings.NewReader("frame=5\nspeed=N/A\nbitrate=N/A\nout_time_us=N/A\nprogress=continue\n"),
		func(p Progress) { got = append(got, p) })
	if len(got) != 1 {
		t.Fatalf("got %d blocks, want 1", len(got))
	}
	if got[0].Speed != 0 || got[0].OutTime != 0 {
		t.Errorf("N/A was not read as unknown: %+v", got[0])
	}
	if got[0].Frame != 5 {
		t.Errorf("the usable field in the block was dropped: %+v", got[0])
	}
}

func TestPercent(t *testing.T) {
	d := 100 * time.Second
	cases := []struct {
		out  time.Duration
		done bool
		want float64
	}{
		{0, false, 0},
		{50 * time.Second, false, 0.5},
		{100 * time.Second, false, 0.999}, // held back until it really ends
		{101 * time.Second, false, 0.999}, // encoders overshoot
		{100 * time.Second, true, 1},
	}
	for _, c := range cases {
		if got := Percent(c.out, d, c.done); got != c.want {
			t.Errorf("Percent(%v, done=%v) = %v, want %v", c.out, c.done, got, c.want)
		}
	}
	if got := Percent(5*time.Second, 0, false); got != -1 {
		t.Errorf("Percent with unknown duration = %v, want -1", got)
	}
}

func TestSpeedTracker(t *testing.T) {
	base := time.Now()
	s := NewSpeedTracker(30 * time.Second)

	if s.Rate() != 0 {
		t.Error("a tracker with no samples claimed to know the rate")
	}
	s.Add(base, 0)
	if s.Rate() != 0 {
		t.Error("one sample is not enough to compute a rate")
	}
	// 10 wall-seconds produced 7 media-seconds.
	s.Add(base.Add(10*time.Second), 7*time.Second)
	if got := s.Rate(); got < 0.69 || got > 0.71 {
		t.Errorf("Rate = %v, want about 0.7", got)
	}

	eta := s.ETA(7*time.Second, 107*time.Second)
	if eta < 140*time.Second || eta > 145*time.Second {
		t.Errorf("ETA = %v, want about 143s", eta)
	}
}

// Old samples must fall out, or a fast opening keeps flattering the estimate.
func TestSpeedTrackerForgetsOldSamples(t *testing.T) {
	base := time.Now()
	s := NewSpeedTracker(10 * time.Second)
	s.Add(base, 0)
	s.Add(base.Add(2*time.Second), 6*time.Second) // 3x during the titles
	s.Add(base.Add(30*time.Second), 20*time.Second)
	s.Add(base.Add(40*time.Second), 25*time.Second) // 0.5x since

	if got := s.Rate(); got > 0.6 {
		t.Errorf("Rate = %v; the fast opening is still counted", got)
	}
}

// The measured rate on the target hardware is 0.717x, which is exactly the
// interesting case: watchable, but only after a wait.
func TestHeadStart(t *testing.T) {
	ep := 1738 * time.Second // a 29-minute episode

	got := HeadStart(ep, 0.717)
	if got < 680*time.Second || got > 692*time.Second {
		t.Errorf("HeadStart at 0.717x = %v, want about 11m26s", got)
	}
	if got := HeadStart(ep, 1.0); got != 0 {
		t.Errorf("HeadStart at realtime = %v, want 0", got)
	}
	if got := HeadStart(ep, 2.0); got != 0 {
		t.Errorf("HeadStart faster than realtime = %v, want 0", got)
	}
	if got := HeadStart(ep, 0); got != -1 {
		t.Errorf("HeadStart with no rate yet = %v, want -1", got)
	}
	// Half speed means waiting out the whole runtime first.
	if got := HeadStart(ep, 0.5); got < ep || got > ep+time.Second {
		t.Errorf("HeadStart at 0.5x = %v, want about the full duration", got)
	}
}

// A run that succeeds can still have had something worth hearing. The one that
// matters most here does not fail: libass warns that it found no font for the
// text, draws nothing, and ffmpeg exits zero — an hour of encoding for a file
// with no subtitles in it and no error anywhere to explain why.
func TestWarningsSurviveASuccessfulRun(t *testing.T) {
	bin := fakeFFmpeg(t, `#!/bin/sh
echo "[Parsed_subtitles_0] fontselect: failed to find any fallback with glyph 0xAC00" >&2
exit 0
`)
	var out strings.Builder
	log.SetOutput(&out)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	e := Exec{Bin: bin}
	if err := e.Run(context.Background(), Spec{Src: "a.mkv", Dst: "a.mp4"}, Settings{}, nil); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if !strings.Contains(out.String(), "fontselect") {
		t.Errorf("the warning was thrown away: %q", out.String())
	}
}

// fakeFFmpeg writes a script that stands in for the real binary.
func fakeFFmpeg(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A diagnostic clip has to be exactly what it claims: no subtitles drawn in,
// no tee, no live rendition, and the index at the front. Every one of those is
// a way for the test to fail for a reason that has nothing to do with what is
// being tested.
func TestProbeArgsAreDeliberatelyPlain(t *testing.T) {
	line := argLine(Args(Spec{
		Src: "/media/a.mkv", Dst: "/out/a-faststart.mp4.part",
		VideoIndex: 0, AudioIndex: 1,
		// These must all be ignored on the probe path.
		BurnSubs: "/state/subs/x.ass", LiveDir: "/state/live/x", Remux: true,
		Width: 3840, Height: 2160,
		Probe: &ProbeOpts{
			StartSec: 60, Secs: 60,
			Profile: "main", Level: "3.1", MaxHeight: 720,
			VideoBitrate: "1800k", AudioBitrate: "128k",
		},
	}, testSettings))

	for _, want := range []string{
		" -ss 60.00 ", " -t 60.00 ",
		" -profile:v main ", " -level 3.1 ",
		" -b:v 1800k ", " -b:a 128k ", " -ac 2 ", " -ar 48000 ",
		` -vf scale=-2:'min(720,ih)' `,
		" -movflags +faststart ", " -f mp4 ",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in:\n%s", want, line)
		}
	}
	for _, unwanted := range []string{"subtitles=", "-f tee", "hls", "-c copy"} {
		if strings.Contains(line, unwanted) {
			t.Errorf("probe carried %q, which it is not testing:\n%s", unwanted, line)
		}
	}

	// -ss has to come before -i or ffmpeg decodes the whole file to reach it.
	if strings.Index(line, " -ss ") > strings.Index(line, " -i ") {
		t.Errorf("-ss is after -i, so seeking will decode from the start:\n%s", line)
	}
}

// Left alone means left alone: a source already smaller than the cap must not
// be scaled up, and no cap at all means no filter.
func TestProbeScalesOnlyWhenAsked(t *testing.T) {
	line := argLine(Args(Spec{
		Src: "a.mkv", Dst: "b.mp4", VideoIndex: 0, AudioIndex: -1,
		Probe: &ProbeOpts{
			Secs: 60, Profile: "high", Level: "4.2",
			VideoBitrate: "2600k", AudioBitrate: "320k",
		},
	}, testSettings))
	if strings.Contains(line, "-vf") {
		t.Errorf("a variant with no height cap still filtered:\n%s", line)
	}
	if strings.Contains(line, "-c:a") {
		t.Errorf("a source with no audio was given an audio encoder:\n%s", line)
	}
}
