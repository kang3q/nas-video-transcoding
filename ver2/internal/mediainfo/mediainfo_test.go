package mediainfo

import "testing"

// Shaped after a real file from the library this was built for: HEVC Main 10
// video, Japanese HE-AAC audio, and three subtitle tracks of which one is a
// bitmap. Notably there is no Korean track, which is exactly the case the
// subtitle picker has to handle gracefully.
const conanJSON = `{
  "streams": [
    {"index":0,"codec_name":"hevc","codec_type":"video","width":1432,"height":1080,
     "disposition":{"default":1}},
    {"index":1,"codec_name":"aac","codec_type":"audio","channels":2,
     "tags":{"language":"jpn"},"disposition":{"default":1}},
    {"index":2,"codec_name":"ass","codec_type":"subtitle",
     "tags":{"language":"eng","title":"Full"},"disposition":{"default":1}},
    {"index":3,"codec_name":"subrip","codec_type":"subtitle",
     "tags":{"language":"rus"},"disposition":{"default":0}},
    {"index":4,"codec_name":"dvd_subtitle","codec_type":"subtitle",
     "tags":{"language":"jpn"},"disposition":{"default":0}}
  ],
  "format": {"format_name":"matroska,webm","duration":"1737.768000"}
}`

func TestParseRealFile(t *testing.T) {
	info, err := parse([]byte(conanJSON))
	if err != nil {
		t.Fatal(err)
	}
	if info.Duration != 1737.768 {
		t.Errorf("Duration = %v, want 1737.768", info.Duration)
	}
	if info.Container != "matroska,webm" {
		t.Errorf("Container = %q", info.Container)
	}
	if len(info.Streams) != 5 {
		t.Fatalf("got %d streams, want 5", len(info.Streams))
	}

	v, ok := info.Video()
	if !ok || v.Codec != "hevc" || v.Width != 1432 || v.Height != 1080 {
		t.Errorf("Video = %+v", v)
	}
	audio := info.Audio()
	if len(audio) != 1 || audio[0].Codec != "aac" || audio[0].Lang != "jpn" || audio[0].Channels != 2 {
		t.Errorf("Audio = %+v", audio)
	}
	subs := info.Subtitles()
	if len(subs) != 3 {
		t.Fatalf("got %d subtitle tracks, want 3", len(subs))
	}
	if !subs[0].Default || subs[0].Lang != "eng" || subs[0].Title != "Full" {
		t.Errorf("first subtitle = %+v", subs[0])
	}
	if subs[0].Bitmap() || subs[1].Bitmap() {
		t.Error("text subtitles reported as bitmap")
	}
	if !subs[2].Bitmap() {
		t.Error("dvd_subtitle should be bitmap: it can only be burned in")
	}
}

// HEVC needs a real encode however convenient the container is.
func TestRemuxOnly(t *testing.T) {
	cases := []struct {
		name  string
		json  string
		remux bool
	}{
		{
			"h264 and aac in mkv — container swap only",
			`{"streams":[{"index":0,"codec_name":"h264","codec_type":"video"},
			             {"index":1,"codec_name":"aac","codec_type":"audio"}],
			  "format":{"format_name":"matroska,webm","duration":"60"}}`,
			true,
		},
		{
			"h264 and mp3",
			`{"streams":[{"index":0,"codec_name":"h264","codec_type":"video"},
			             {"index":1,"codec_name":"mp3","codec_type":"audio"}],
			  "format":{"format_name":"avi","duration":"60"}}`,
			true,
		},
		{
			"hevc in mp4 — looks converted, is not",
			`{"streams":[{"index":0,"codec_name":"hevc","codec_type":"video"},
			             {"index":1,"codec_name":"aac","codec_type":"audio"}],
			  "format":{"format_name":"mov,mp4,m4a","duration":"60"}}`,
			false,
		},
		{
			"h264 with ac3 audio",
			`{"streams":[{"index":0,"codec_name":"h264","codec_type":"video"},
			             {"index":1,"codec_name":"ac3","codec_type":"audio"}],
			  "format":{"format_name":"matroska,webm","duration":"60"}}`,
			false,
		},
		{
			"xvid",
			`{"streams":[{"index":0,"codec_name":"mpeg4","codec_type":"video"},
			             {"index":1,"codec_name":"mp3","codec_type":"audio"}],
			  "format":{"format_name":"avi","duration":"60"}}`,
			false,
		},
		{
			"video with no audio at all",
			`{"streams":[{"index":0,"codec_name":"h264","codec_type":"video"}],
			  "format":{"format_name":"matroska,webm","duration":"60"}}`,
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info, err := parse([]byte(tc.json))
			if err != nil {
				t.Fatal(err)
			}
			if got := info.RemuxOnly(); got != tc.remux {
				t.Errorf("RemuxOnly = %v, want %v", got, tc.remux)
			}
		})
	}
}

// Cover art is stored as a video stream. Treating it as the picture would
// convert a thumbnail instead of the film.
func TestVideoSkipsCoverArt(t *testing.T) {
	info, err := parse([]byte(`{"streams":[
	  {"index":0,"codec_name":"mjpeg","codec_type":"video","width":600,"height":900},
	  {"index":1,"codec_name":"h264","codec_type":"video","width":1920,"height":1080},
	  {"index":2,"codec_name":"aac","codec_type":"audio"}],
	  "format":{"format_name":"matroska,webm","duration":"60"}}`))
	if err != nil {
		t.Fatal(err)
	}
	v, ok := info.Video()
	if !ok || v.Codec != "h264" || v.Index != 1 {
		t.Errorf("Video = %+v, want the h264 stream at index 1", v)
	}
	if !info.RemuxOnly() {
		t.Error("cover art should not force a re-encode")
	}
}

// Some files carry no duration in the container header. Progress has to cope
// with that rather than dividing by zero.
func TestParseToleratesAMissingDuration(t *testing.T) {
	info, err := parse([]byte(`{"streams":[{"index":0,"codec_name":"h264","codec_type":"video"}],
	  "format":{"format_name":"matroska,webm"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if info.Duration != 0 {
		t.Errorf("Duration = %v, want 0", info.Duration)
	}
}

func TestParseIgnoresDataAndAttachmentStreams(t *testing.T) {
	info, err := parse([]byte(`{"streams":[
	  {"index":0,"codec_name":"h264","codec_type":"video"},
	  {"index":1,"codec_name":"aac","codec_type":"audio"},
	  {"index":2,"codec_name":"ttf","codec_type":"attachment"},
	  {"index":3,"codec_name":"bin_data","codec_type":"data"}],
	  "format":{"format_name":"matroska,webm","duration":"60"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Streams) != 2 {
		t.Errorf("kept %d streams, want only video and audio", len(info.Streams))
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if _, err := parse([]byte("not json")); err == nil {
		t.Error("garbage was accepted")
	}
}
