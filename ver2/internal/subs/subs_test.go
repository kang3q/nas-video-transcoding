package subs

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/transform"

	"nvt/ver2/internal/mediainfo"
	"nvt/ver2/internal/outpath"
)

type stubProber struct{ info mediainfo.Info }

func (s stubProber) Cached(string, os.FileInfo) (mediainfo.Info, bool) { return s.info, true }

func newFinder(t *testing.T, info mediainfo.Info, files ...string) (*Finder, *outpath.Mapper, string) {
	t.Helper()
	base := t.TempDir()
	src := filepath.Join(base, "media")
	out := filepath.Join(base, "out")
	for _, d := range []string{src, out} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range files {
		p := filepath.Join(src, filepath.FromSlash(f))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m, err := outpath.NewMapper(src, out, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return NewFinder(m, stubProber{info: info}), m, src
}

func (s stubProber) CachedAt(string, int64, int64) (mediainfo.Info, bool) {
	return s.info, true
}

func rel(t *testing.T, m *outpath.Mapper, p string) outpath.Rel {
	t.Helper()
	r, err := m.ParseRel(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The file that prompted all of this has English, Russian and Japanese tracks
// and no Korean one — the picker has to behave sensibly anyway.
func TestFindEmbedded(t *testing.T) {
	info := mediainfo.Info{Streams: []mediainfo.Stream{
		{Index: 0, Type: "video", Codec: "hevc"},
		{Index: 1, Type: "audio", Codec: "aac", Lang: "jpn"},
		{Index: 2, Type: "subtitle", Codec: "ass", Lang: "eng", Title: "Full", Default: true},
		{Index: 3, Type: "subtitle", Codec: "subrip", Lang: "rus"},
		{Index: 4, Type: "subtitle", Codec: "dvd_subtitle", Lang: "jpn"},
	}}
	f, m, _ := newFinder(t, info, "S/ep1.mkv")

	got := f.Find(rel(t, m, "S/ep1.mkv"))
	if len(got) != 3 {
		t.Fatalf("found %d tracks, want 3", len(got))
	}
	// The bitmap track sorts last, because it cannot be burned in.
	if !got[2].Bitmap {
		t.Errorf("bitmap track is not last: %+v", got)
	}
	if got[2].Burnable() {
		t.Error("a bitmap track was reported as burnable")
	}
	if !strings.Contains(got[2].Label, "굽기 불가") {
		t.Errorf("the label does not explain why: %q", got[2].Label)
	}
	if got[0].ID != "embedded:2" {
		t.Errorf("first track = %q, want the default English one", got[0].ID)
	}
}

// A Korean track is what this library is normally watched with, so it goes
// first however the file happens to order its streams.
func TestKoreanSortsFirst(t *testing.T) {
	info := mediainfo.Info{Streams: []mediainfo.Stream{
		{Index: 1, Type: "subtitle", Codec: "ass", Lang: "eng", Default: true},
		{Index: 2, Type: "subtitle", Codec: "subrip", Lang: "kor"},
	}}
	f, m, _ := newFinder(t, info, "a.mkv")

	got := f.Find(rel(t, m, "a.mkv"))
	if got[0].Lang != "kor" {
		t.Errorf("first track = %q, want the Korean one", got[0].Lang)
	}
	picked, ok := Pick(got)
	if !ok || picked.Lang != "kor" {
		t.Errorf("Pick = %+v, want the Korean track", picked)
	}
}

func TestKoreanDetection(t *testing.T) {
	cases := []struct {
		t    Track
		want bool
	}{
		{Track{Lang: "kor"}, true},
		{Track{Lang: "ko"}, true},
		{Track{Lang: "KOR"}, true},
		{Track{Lang: "eng"}, false},
		{Track{File: "Show.ko.srt"}, true},
		{Track{File: "Show.korean.smi"}, true},
		{Track{File: "미래소년 코난 1화 한글.smi"}, true},
		{Track{File: "Show.eng.srt"}, false},
		{Track{Title: "한국어"}, true},
		// "kor" inside an unrelated word should not be a false positive here,
		// but this is a heuristic and worth knowing about.
		{Track{File: "Show.jpn.srt"}, false},
	}
	for _, c := range cases {
		if got := c.t.Korean(); got != c.want {
			t.Errorf("Korean(%+v) = %v, want %v", c.t, got, c.want)
		}
	}
}

func TestFindSidecars(t *testing.T) {
	f, m, _ := newFinder(t, mediainfo.Info{},
		"S/ep1.mkv",
		"S/ep1.ko.smi",
		"S/ep1.eng.srt",
		"S/ep1.txt",    // not a subtitle
		"S/ep2.ko.srt", // a different episode
		"S/Subs/extra.srt",
	)
	got := f.Find(rel(t, m, "S/ep1.mkv"))

	var ids []string
	for _, tr := range got {
		ids = append(ids, tr.ID)
	}
	if len(got) < 3 {
		t.Fatalf("found %v, expected the two matching sidecars and the Subs/ one", ids)
	}
	// The Korean one leads.
	if !got[0].Korean() {
		t.Errorf("first track = %+v, want the Korean sidecar", got[0])
	}
	for _, tr := range got {
		if strings.Contains(tr.File, "ep2") {
			t.Errorf("a sidecar for another episode was picked up: %s", tr.File)
		}
		if strings.HasSuffix(tr.File, ".txt") {
			t.Error("a non-subtitle file was offered")
		}
	}
}

func TestPickSkipsBitmapTracks(t *testing.T) {
	tracks := []Track{
		{ID: "embedded:1", Lang: "kor", Bitmap: true},
		{ID: "embedded:2", Lang: "eng"},
	}
	got, ok := Pick(tracks)
	if !ok || got.ID != "embedded:2" {
		t.Errorf("Pick = %+v, want the burnable track even though it is not Korean", got)
	}
}

func TestPickWithNothingBurnable(t *testing.T) {
	if _, ok := Pick([]Track{{Bitmap: true}}); ok {
		t.Error("Pick offered a track that cannot be burned")
	}
	if _, ok := Pick(nil); ok {
		t.Error("Pick invented a track")
	}
}

func TestFindByID(t *testing.T) {
	tracks := []Track{{ID: "embedded:2"}, {ID: "sidecar:S/ep1.ko.smi"}}
	if got, ok := FindByID(tracks, "sidecar:S/ep1.ko.smi"); !ok || got.ID != "sidecar:S/ep1.ko.smi" {
		t.Errorf("FindByID = %+v, %v", got, ok)
	}
	if _, ok := FindByID(tracks, "embedded:9"); ok {
		t.Error("FindByID matched something that is not there")
	}
}

// The encoding trap: a Korean .smi handed to ffmpeg as CP949 comes out as
// mojibake, if the demuxer accepts it at all.
func TestToUTF8DecodesCP949(t *testing.T) {
	const korean0 = "미래소년 코난 자막입니다"

	cp949, _, err := transform.Bytes(korean.EUCKR.NewEncoder(), []byte(korean0))
	if err != nil {
		t.Fatal(err)
	}
	if string(cp949) == korean0 {
		t.Fatal("the fixture is not actually encoded")
	}

	got, err := toUTF8(cp949)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != korean0 {
		t.Errorf("decoded %q, want %q", got, korean0)
	}
}

func TestToUTF8LeavesUTF8Alone(t *testing.T) {
	in := []byte("이미 UTF-8 입니다")
	got, err := toUTF8(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(in) {
		t.Errorf("UTF-8 input was altered: %q", got)
	}
}

func TestToUTF8StripsBOM(t *testing.T) {
	in := append([]byte{0xEF, 0xBB, 0xBF}, []byte("hello")...)
	got, err := toUTF8(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("got %q, want the BOM removed", got)
	}
}

func TestLangFromName(t *testing.T) {
	cases := map[string]string{
		"Show.S01E01.ko":     "ko",
		"Show.S01E01.korean": "korean",
		"Show.S01E01":        "",
		"Show.S01E01-eng":    "eng",
	}
	for stem, want := range cases {
		if got := langFromName(stem, "Show.S01E01"); got != want {
			t.Errorf("langFromName(%q) = %q, want %q", stem, got, want)
		}
	}
}

func TestPrepareRejectsAMalformedID(t *testing.T) {
	p := &Preparer{TempDir: t.TempDir(), FFmpeg: "false"}
	if _, _, err := p.Prepare(t.Context(), "job1", outpath.Rel{}, "nonsense"); err == nil {
		t.Error("a malformed track id was accepted")
	}
	if _, _, err := p.Prepare(t.Context(), "job1", outpath.Rel{}, "elsewhere:1"); err == nil {
		t.Error("an unknown track kind was accepted")
	}
}

// No subtitle chosen is the common case and must not be an error.
func TestPrepareWithNoSelection(t *testing.T) {
	p := &Preparer{TempDir: t.TempDir(), FFmpeg: "false"}
	path, cleanup, err := p.Prepare(t.Context(), "job1", outpath.Rel{}, "")
	if err != nil || path != "" {
		t.Errorf("Prepare(\"\") = %q, %v", path, err)
	}
	cleanup()
}

// Once a language is chosen, the rest of the batch stays in it. An episode
// without that language gets nothing, because burning the wrong one cannot be
// undone.
func TestPickLang(t *testing.T) {
	tracks := []Track{
		{ID: "a", Lang: "eng"},
		{ID: "b", Lang: "kor"},
		{ID: "c", Lang: "jpn", Bitmap: true},
	}
	if got, ok := PickLang(tracks, "kor"); !ok || got.ID != "b" {
		t.Errorf("PickLang(kor) = %+v", got)
	}
	if got, ok := PickLang(tracks, "eng"); !ok || got.ID != "a" {
		t.Errorf("PickLang(eng) = %+v", got)
	}
	if _, ok := PickLang(tracks, "jpn"); ok {
		t.Error("PickLang offered a bitmap track")
	}
	if _, ok := PickLang(tracks, "rus"); ok {
		t.Error("PickLang invented a track for a language that is not there")
	}
	if _, ok := PickLang(tracks, ""); ok {
		t.Error("PickLang with no language should decline")
	}
	// A Korean sidecar with no language tag still counts.
	byName := []Track{{ID: "d", File: "Show.한글.smi"}}
	if got, ok := PickLang(byName, "kor"); !ok || got.ID != "d" {
		t.Errorf("PickLang did not recognise a Korean filename: %+v", got)
	}
}

func TestTrackLanguage(t *testing.T) {
	if got := (Track{Lang: "eng"}).Language(); got != "eng" {
		t.Errorf("Language = %q", got)
	}
	if got := (Track{File: "Show.한글.smi"}).Language(); got != "kor" {
		t.Errorf("a Korean filename should report kor, got %q", got)
	}
}

// The case from the library this was built for: one subtitle file named
// exactly like the video, carrying no language tag because there is only one
// of them and nothing to distinguish it from. Every naming rule finds nothing
// here, so the file itself has to be read.
func TestUntaggedSidecarIsReadRatherThanGuessed(t *testing.T) {
	const video = "DEATH NOTE 데스노트 02 (704x396 DivX).avi"
	const sub = "DEATH NOTE 데스노트 02 (704x396 DivX).smi"

	f, m, src := newFinder(t, mediainfo.Info{}, video)

	// Real .smi files from this era are CP949, not UTF-8.
	body, _, err := transform.Bytes(korean.EUCKR.NewEncoder(),
		[]byte("<SYNC Start=1000><P Class=KRCC>류크, 사과 줄까?\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, sub), body, 0o644); err != nil {
		t.Fatal(err)
	}

	tracks := f.Find(rel(t, m, video))
	if len(tracks) != 1 {
		t.Fatalf("got %d tracks, want 1: %+v", len(tracks), tracks)
	}
	if !tracks[0].Korean() {
		t.Errorf("the Korean subtitle was not recognised: %+v", tracks[0])
	}
	if _, ok := PickLang(tracks, "kor"); !ok {
		t.Error("PickLang could not find it, so the form would default to off")
	}
}

// Reading the file must not turn an English subtitle into a Korean one — that
// is the mistake a filename rule makes, and the reason for reading at all.
func TestUntaggedEnglishSidecarStaysEnglish(t *testing.T) {
	f, m, src := newFinder(t, mediainfo.Info{}, "Show.mkv")
	if err := os.WriteFile(filepath.Join(src, "Show.srt"),
		[]byte("1\n00:00:01,000 --> 00:00:02,000\nI'll take a potato chip. And eat it!\n"),
		0o644); err != nil {
		t.Fatal(err)
	}

	tracks := f.Find(rel(t, m, "Show.mkv"))
	if len(tracks) != 1 {
		t.Fatalf("got %d tracks, want 1: %+v", len(tracks), tracks)
	}
	if tracks[0].Korean() {
		t.Errorf("an English subtitle was taken for Korean: %+v", tracks[0])
	}
	if _, ok := PickLang(tracks, "kor"); ok {
		t.Error("PickLang offered an English track as the Korean one")
	}
}

// An explicit tag is the author saying what the file is, and outranks
// whatever the first few kilobytes happen to contain — a Korean-subtitled
// English learning track, say.
func TestAnExplicitTagWins(t *testing.T) {
	f, m, src := newFinder(t, mediainfo.Info{}, "Show.mkv")
	if err := os.WriteFile(filepath.Join(src, "Show.eng.srt"),
		[]byte("1\n00:00:01,000 --> 00:00:02,000\n사과\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tracks := f.Find(rel(t, m, "Show.mkv"))
	if len(tracks) != 1 || tracks[0].Lang != "eng" {
		t.Fatalf("tag was not kept: %+v", tracks)
	}
}

func TestHasHangul(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"류크, 사과 줄까?", true},
		{"ㄱㄴㄷ", true}, // compatibility Jamo
		{"I'll take a potato chip", false},
		{"リュークりんご食べる?", false}, // Japanese must not count
		{"", false},
	}
	for _, tc := range cases {
		if got := hasHangul([]byte(tc.in)); got != tc.want {
			t.Errorf("hasHangul(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// What went into the picture is the thing most often wrong and the hardest to
// check later — it is drawn into the frames, so short of watching the file
// there is no way to tell. The log has to say.
func TestResolveSaysWhatItBurned(t *testing.T) {
	var out strings.Builder
	log.SetOutput(&out)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	f, m, src := newFinder(t, mediainfo.Info{}, "Show.mkv")
	if err := os.WriteFile(filepath.Join(src, "Show.ko.srt"), []byte("1\n한글\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tracks := f.Find(rel(t, m, "Show.mkv"))
	if _, ok := PickLang(tracks, "kor"); !ok {
		t.Fatal("setup: the Korean track was not found")
	}

	// A resolver with no preparer would panic, so only the decision is
	// exercised here — the point is the line it writes on the way through.
	r := &Resolver{Finder: f}
	if _, _, err := r.Resolve(context.Background(), "job1", rel(t, m, "Other.mkv"), "", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no subtitles found for Other.mkv") {
		t.Errorf("a conversion without subtitles said nothing: %q", out.String())
	}
}

// ffmpeg exits zero after writing an ASS file with a style block and no
// events, which SAMI produces often enough to matter: its markup is frequently
// broken past the header. Burning that in succeeds and draws nothing, and an
// hour later the only way to find out is to watch the result.
func TestAnEmptySubtitleFileIsRefused(t *testing.T) {
	dir := t.TempDir()

	empty := filepath.Join(dir, "empty.ass")
	if err := os.WriteFile(empty, []byte(
		"[Script Info]\nScriptType: v4.00+\n\n[V4+ Styles]\nStyle: Default\n\n[Events]\nFormat: Layer, Start, End, Text\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	err := checkHasDialogue(empty, "Show.smi")
	if err == nil {
		t.Fatal("a subtitle file with no lines was accepted")
	}
	if !strings.Contains(err.Error(), "Show.smi") {
		t.Errorf("the error does not name the file: %v", err)
	}

	full := filepath.Join(dir, "full.ass")
	if err := os.WriteFile(full, []byte(
		"[Events]\nFormat: Layer, Start, End, Text\nDialogue: 0,0:00:01.00,0:00:03.00,류크, 사과 줄까?\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkHasDialogue(full, "Show.smi"); err != nil {
		t.Errorf("a subtitle file with lines was refused: %v", err)
	}
}
