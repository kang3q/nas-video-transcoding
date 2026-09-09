// Package airplay makes a handful of short clips of one video, each encoded a
// little more conservatively than the last, so an AirPlay failure can be
// narrowed down by watching rather than guessed at.
//
// AirPlay from a web page is not the browser sending pixels. Safari hands the
// Apple TV a URL and the Apple TV fetches and decodes the file itself, which
// means a failure could be the profile, the level, the resolution, the audio,
// where the index sits in the file, or nothing about the file at all. None of
// those announce themselves: the screen simply stays black.
//
// So the variants form a ladder. Each rung gives up something the one above it
// kept, and the rung where playback starts working is the thing that mattered.
// If the first rung works, it was the index position; if none of them do, it
// was never the encoding.
package airplay

import (
	"crypto/sha256"
	"encoding/hex"
	"path"

	"nvt/ver2/internal/ffmpeg"
	"nvt/ver2/internal/outpath"
)

// Dir is where the clips for one video live, under the output root.
//
// The name is a hash rather than the title. Library paths here are Korean with
// spaces and brackets, and a URL is one more thing that could be what AirPlay
// is unhappy about — testing four encodings through a path that is itself
// suspect would prove nothing. Every clip is reached by plain ASCII.
const Root = "_airplay"

func Dir(rel outpath.Rel) string {
	sum := sha256.Sum256([]byte(rel.String()))
	return path.Join(Root, hex.EncodeToString(sum[:])[:12])
}

// Variant is one rung of the ladder.
type Variant struct {
	// Name is the file's stem, and its identity everywhere. ASCII only.
	Name string
	// Title and Gives are what the page says about it: what it is, and what
	// it gives up compared to the rung above.
	Title string
	Gives string

	Profile      string
	Level        string
	MaxHeight    int
	VideoBitrate string
	AudioBitrate string
}

func (v Variant) File() string { return v.Name + ".mp4" }

// Variants runs from "what we already make" to "what could not plausibly be
// refused", so the answer is the first one that plays.
var Variants = []Variant{
	{
		Name:  "a-faststart",
		Title: "지금 설정 그대로, 인덱스만 앞으로",
		Gives: "지금 나오는 파일과 인코딩이 같습니다. 다른 것은 인덱스(moov)를 " +
			"파일 앞에 둔 것 하나뿐입니다. 미리보기를 켜고 변환하면 인덱스가 " +
			"파일 끝에 남는데, 애플TV 는 URL 을 받아 직접 받아가므로 이게 " +
			"문제일 수 있습니다. 이것만 재생되면 원인은 여기입니다.",
		Profile: "high", Level: "4.2",
		VideoBitrate: "2600k", AudioBitrate: "320k",
	},
	{
		Name:  "b-main",
		Title: "프로필을 Main 으로",
		Gives: "High 프로필과 레벨 4.2 를 포기합니다. 오디오도 192k 로 낮춥니다. " +
			"이것부터 재생되면 디코더가 프로필이나 레벨에서 걸린 것입니다.",
		Profile: "main", Level: "4.0",
		VideoBitrate: "2600k", AudioBitrate: "192k",
	},
	{
		Name:  "c-720p",
		Title: "720p 로 줄이고 레벨 3.1",
		Gives: "해상도와 비트레이트까지 내립니다. 이것부터 재생되면 원본 크기나 " +
			"레벨이 문제였던 것입니다.",
		Profile: "main", Level: "3.1", MaxHeight: 720,
		VideoBitrate: "1800k", AudioBitrate: "128k",
	},
	{
		Name:  "d-baseline",
		Title: "가장 보수적으로",
		Gives: "Baseline 프로필, 360p, 96k 오디오. 십 년 넘은 기기도 받는 조합이라 " +
			"애플TV 4K 가 이걸 거부할 이유는 없습니다. 그런데도 안 되면 원인은 " +
			"인코딩이 아닙니다 — 주소, 네트워크, 또는 에어플레이 경로입니다.",
		Profile: "baseline", Level: "3.0", MaxHeight: 360,
		VideoBitrate: "1200k", AudioBitrate: "96k",
	},
}

func Find(name string) (Variant, bool) {
	for _, v := range Variants {
		if v.Name == name {
			return v, true
		}
	}
	return Variant{}, false
}

// ClipSecs is how much of the video each variant carries. Long enough to see
// and hear that it is really playing, short enough that four of them are a
// couple of minutes of encoding rather than an evening.
const ClipSecs = 60

// StartSec picks a moment with something on screen. The very beginning of an
// episode is often black or a still, which makes "is it playing?" a harder
// question than it needs to be.
func StartSec(duration float64) float64 {
	if duration < 3*ClipSecs {
		return 0
	}
	if at := duration * 0.1; at < ClipSecs {
		return at
	}
	return ClipSecs
}

// Opts turns a variant into the encoder settings for one clip.
func (v Variant) Opts(duration float64) *ffmpeg.ProbeOpts {
	return &ffmpeg.ProbeOpts{
		StartSec:     StartSec(duration),
		Secs:         ClipSecs,
		Profile:      v.Profile,
		Level:        v.Level,
		MaxHeight:    v.MaxHeight,
		VideoBitrate: v.VideoBitrate,
		AudioBitrate: v.AudioBitrate,
	}
}
