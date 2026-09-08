package ffmpeg

import (
	"bufio"
	"io"
	"strconv"
	"strings"
	"time"
)

// Progress is one reading from ffmpeg's -progress stream.
type Progress struct {
	OutTime time.Duration // how much of the output has been written
	Frame   int64
	FPS     float64
	Speed   float64 // ffmpeg's own cumulative figure; 0 when it says N/A
	Done    bool    // the final block
}

// ParseProgress reads ffmpeg's -progress output and calls fn once per block.
//
// The format is key=value lines, and the docs promise that every block ends
// with a "progress" key. Emitting only on that key is what keeps a half-read
// block from being reported as a sudden jump backwards.
//
// Two traps live here. "out_time_ms" is microseconds despite the name — a
// long-standing quirk — so out_time_us is used instead. And any value can be
// the literal "N/A" early on, which must be read as "not yet" rather than
// zero.
func ParseProgress(r io.Reader, fn func(Progress)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var cur Progress
	var have bool

	for sc.Scan() {
		key, val, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)

		switch key {
		case "out_time_us":
			if us, err := strconv.ParseInt(val, 10, 64); err == nil && us >= 0 {
				cur.OutTime = time.Duration(us) * time.Microsecond
				have = true
			}
		case "frame":
			if n, err := strconv.ParseInt(val, 10, 64); err == nil {
				cur.Frame = n
				have = true
			}
		case "fps":
			if f, err := strconv.ParseFloat(val, 64); err == nil {
				cur.FPS = f
			}
		case "speed":
			// Arrives as "0.717x", or "N/A" before there is anything to average.
			if s, err := strconv.ParseFloat(strings.TrimSuffix(val, "x"), 64); err == nil {
				cur.Speed = s
			}
		case "progress":
			cur.Done = val == "end"
			if have || cur.Done {
				fn(cur)
			}
			// Carry nothing but the clock forward: the next block restates
			// everything it knows.
			cur = Progress{}
			have = false
		}
	}
	return sc.Err()
}

// Percent is how far along a job is, as a fraction of 0..1, or -1 when the
// source duration is unknown — some containers simply do not record one.
//
// It is held just below 1 until the job actually ends, because an encoder
// routinely overshoots the probed duration by a frame or two and a progress
// bar that reads 100% while still working is a worse lie than 99%.
func Percent(out time.Duration, duration time.Duration, done bool) float64 {
	if done {
		return 1
	}
	if duration <= 0 {
		return -1
	}
	p := out.Seconds() / duration.Seconds()
	if p < 0 {
		return 0
	}
	if p > 0.999 {
		return 0.999
	}
	return p
}

// SpeedTracker estimates the current encoding rate from a sliding window.
//
// ffmpeg's own "speed" field is a cumulative average, which lags badly: a fast
// title sequence keeps flattering the estimate long after the show has slowed
// it down. Deciding when a viewer can safely start watching needs the rate
// now, not the rate since the beginning.
type SpeedTracker struct {
	window  time.Duration
	samples []sample
}

type sample struct {
	wall time.Time
	out  time.Duration
}

func NewSpeedTracker(window time.Duration) *SpeedTracker {
	if window <= 0 {
		window = 30 * time.Second
	}
	return &SpeedTracker{window: window}
}

func (s *SpeedTracker) Add(now time.Time, out time.Duration) {
	s.samples = append(s.samples, sample{wall: now, out: out})
	cut := now.Add(-s.window)
	i := 0
	for i < len(s.samples)-1 && s.samples[i].wall.Before(cut) {
		i++
	}
	s.samples = s.samples[i:]
}

// Rate is media-seconds produced per wall-second, or 0 when there is not yet
// enough history to say.
func (s *SpeedTracker) Rate() float64 {
	if len(s.samples) < 2 {
		return 0
	}
	first, last := s.samples[0], s.samples[len(s.samples)-1]
	wall := last.wall.Sub(first.wall).Seconds()
	if wall <= 0 {
		return 0
	}
	return (last.out - first.out).Seconds() / wall
}

// ETA is how much longer the job needs, or -1 when that cannot be estimated.
func (s *SpeedTracker) ETA(out, duration time.Duration) time.Duration {
	rate := s.Rate()
	if rate <= 0 || duration <= 0 || out >= duration {
		return -1
	}
	return time.Duration((duration - out).Seconds() / rate * float64(time.Second))
}

// HeadStart is how long to wait before starting playback so the encoder is
// never overtaken.
//
// Playback consumes one media-second per wall-second; the encoder produces
// rate of them. When rate >= 1 there is nothing to wait for. Below that the
// gap has to be paid up front: waiting duration*(1/rate - 1) means the last
// frame is encoded exactly as it is needed.
func HeadStart(duration time.Duration, rate float64) time.Duration {
	if rate <= 0 || duration <= 0 {
		return -1
	}
	if rate >= 1 {
		return 0
	}
	return time.Duration(duration.Seconds() * (1/rate - 1) * float64(time.Second))
}
