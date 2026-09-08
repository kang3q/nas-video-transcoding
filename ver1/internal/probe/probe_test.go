package probe

import (
	"testing"

	"nvt/ver1/internal/config"
)

func testCfg() *config.Config {
	return &config.Config{
		VideoOK: []string{"h264", "hevc", "mpeg4", "mpeg2video"},
		AudioOK: []string{"aac", "mp3", "flac"},
	}
}

func stream(typ, codec string) ffStream {
	return ffStream{CodecType: typ, CodecName: codec}
}

func TestDecide(t *testing.T) {
	cases := []struct {
		name    string
		streams []ffStream
		want    Action
	}{
		{"everything supported", []ffStream{stream("video", "h264"), stream("audio", "aac")}, Passthrough},
		{"ac3 audio", []ffStream{stream("video", "mpeg4"), stream("audio", "ac3")}, AudioOnly},
		{"dts audio", []ffStream{stream("video", "h264"), stream("audio", "dts")}, AudioOnly},
		{"unsupported video", []ffStream{stream("video", "vc1"), stream("audio", "aac")}, FullTranscode},
		{"both unsupported", []ffStream{stream("video", "vc1"), stream("audio", "ac3")}, FullTranscode},
		{"codec case is ignored", []ffStream{stream("video", "H264"), stream("audio", "AAC")}, Passthrough},
		{"cover art is not a video stream", []ffStream{
			stream("video", "h264"), stream("video", "mjpeg"), stream("audio", "aac"),
		}, Passthrough},
		{"one bad track among several", []ffStream{
			stream("video", "h264"), stream("audio", "aac"), stream("audio", "truehd"),
		}, AudioOnly},
		{"subtitles are ignored", []ffStream{
			stream("video", "h264"), stream("audio", "aac"), stream("subtitle", "hdmv_pgs_subtitle"),
		}, Passthrough},
	}

	cfg := testCfg()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decide(ffOutput{Streams: tc.streams}, cfg)
			if got.Action != tc.want {
				t.Errorf("decide() = %q (%s), want %q", got.Action, got.Reason, tc.want)
			}
		})
	}
}

// A file whose only video stream is cover art is audio, not video, and must
// not be dragged through a needless re-encode.
func TestDecideAudioOnlyFile(t *testing.T) {
	got := decide(ffOutput{Streams: []ffStream{
		stream("video", "png"), stream("audio", "flac"),
	}}, testCfg())
	if got.Action != Passthrough {
		t.Errorf("decide() = %q, want %q", got.Action, Passthrough)
	}
}
