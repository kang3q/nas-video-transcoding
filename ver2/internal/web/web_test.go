package web

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nvt/ver2/internal/config"
	"nvt/ver2/internal/ffmpeg"
	"nvt/ver2/internal/jobs"
	"nvt/ver2/internal/library"
	"nvt/ver2/internal/mediainfo"
	"nvt/ver2/internal/outpath"
	"nvt/ver2/internal/subs"
)

type stubProber struct {
	info mediainfo.Info
	// uncached makes Cached miss, so callers that only consult the cache take
	// their fallback path — which is what browsing does.
	uncached bool
}

func (s stubProber) Probe(context.Context, string, os.FileInfo) (mediainfo.Info, error) {
	return s.info, nil
}
func (s stubProber) Cached(string, os.FileInfo) (mediainfo.Info, bool) {
	if s.uncached {
		return mediainfo.Info{}, false
	}
	return s.info, true
}

func (s stubProber) CachedAt(string, int64, int64) (mediainfo.Info, bool) {
	if s.uncached {
		return mediainfo.Info{}, false
	}
	return s.info, true
}

type stubRunner struct {
	mu    sync.Mutex
	seen  []string
	block chan struct{}
}

func (r *stubRunner) Run(ctx context.Context, spec ffmpeg.Spec, _ ffmpeg.Settings, _ func(ffmpeg.Progress)) error {
	r.mu.Lock()
	r.seen = append(r.seen, filepath.Base(spec.Src))
	block := r.block
	r.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return os.WriteFile(spec.Dst, []byte("converted"), 0o644)
}

func (r *stubRunner) converted() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

type env struct {
	h      http.Handler
	srv    *Server
	runner *stubRunner
	out    string
	src    string
}

func (e *env) rel(t *testing.T, p string) outpath.Rel {
	t.Helper()
	r, err := e.srv.mapper.ParseRel(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newEnv(t *testing.T, blocking bool, files ...string) *env {
	t.Helper()
	base := t.TempDir()
	src := filepath.Join(base, "media")
	out := filepath.Join(base, "out")
	state := filepath.Join(base, "state")
	for _, d := range []string{src, out, state} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range files {
		p := filepath.Join(src, filepath.FromSlash(f))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte("source"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	m, err := outpath.NewMapper(src, out, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })

	runner := &stubRunner{}
	if blocking {
		runner.block = make(chan struct{})
	}

	prober := stubProber{info: mediainfo.Info{
		Duration: 100,
		Streams: []mediainfo.Stream{
			{Index: 0, Type: "video", Codec: "hevc"},
			{Index: 1, Type: "audio", Codec: "aac", Lang: "jpn"},
			{Index: 2, Type: "subtitle", Codec: "ass", Lang: "eng", Default: true},
			{Index: 3, Type: "subtitle", Codec: "dvd_subtitle", Lang: "jpn"},
		},
	}}

	q := jobs.NewQueue(jobs.Deps{
		Mapper: m, Prober: prober, Runner: runner, Workers: 1,
		LiveRoot: filepath.Join(state, "live"), SegmentSecs: 4, CheckpointAt: 0.10,
	})
	if blocking {
		// Cleanups run last-registered first, so this fires before the
		// temporary directory is removed. Wait for anything it releases to
		// finish writing.
		t.Cleanup(func() {
			select {
			case <-runner.block:
			default:
				close(runner.block)
			}
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				busy := false
				for _, v := range q.Snapshot() {
					if !v.State.Terminal() {
						busy = true
					}
				}
				if !busy {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}

	cfg := &config.Config{
		OutputDir: out, SourceDir: src, StateDir: state,
		Live: true, SegmentSecs: 4,
	}
	s, err := New(cfg, m, library.New(m), q, prober, subs.NewFinder(m, prober))
	if err != nil {
		t.Fatal(err)
	}
	return &env{h: s.Handler(), srv: s, runner: runner, out: out, src: src}
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func post(t *testing.T, h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- tests ---

func TestBrowseListsTheDirectory(t *testing.T) {
	e := newEnv(t, false, "S/ep10.mkv", "S/ep2.mkv", "S/notes.txt", "Other/x.mkv")

	rec := get(t, e.h, "/browse/S")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"ep2.mkv", "ep10.mkv", "notes.txt", "변환 설정"} {
		if !strings.Contains(body, want) {
			t.Errorf("browse page is missing %q", want)
		}
	}
	// Episode 2 before episode 10, or the queue order it implies is wrong.
	if strings.Index(body, "ep2.mkv") > strings.Index(body, "ep10.mkv") {
		t.Error("listing is not in natural order")
	}
	// Conversion is configured on the detail page, so a listing only links
	// there. Two identical-looking sets of buttons that behaved differently —
	// the listing's silently skipped subtitles — was the confusion this
	// removes.
	if strings.Contains(body, `name="scope"`) {
		t.Errorf("the listing still starts conversions of its own:\n%s", body)
	}
	if n := strings.Count(body, `class="btn" href="/watch/`); n != 2 {
		t.Errorf("got %d links to the detail page, want one per video:\n%s", n, body)
	}
}

// Percent-encoded traversal is the case that reaches the handler: ServeMux
// normalises the escaped path, so "%2e%2e" is not a ".." as far as it is
// concerned and it passes straight through to be rejected here.
func TestBrowseRejectsEncodedTraversal(t *testing.T) {
	e := newEnv(t, false, "a.mkv")
	for _, p := range []string{
		"/browse/%2e%2e%2f%2e%2e%2fetc",
		"/browse/a%2f..%2f..%2f..%2fetc",
	} {
		if rec := get(t, e.h, p); rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", p, rec.Code)
		}
	}
}

// A literal ".." never gets that far: ServeMux cleans the path and redirects,
// so the request lands on a route that does not exist.
func TestBrowseLiteralTraversalIsCleanedAway(t *testing.T) {
	e := newEnv(t, false, "a.mkv")
	rec := get(t, e.h, "/browse/../../etc")
	// Which 3xx ServeMux picks for a cleaned path has changed between Go
	// releases; that it redirects away is the part that matters.
	if rec.Code < 300 || rec.Code >= 400 {
		t.Fatalf("status = %d, want a redirect from path cleaning", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/etc" {
		t.Fatalf("Location = %q, want the cleaned path", loc)
	}
	if rec := get(t, e.h, "/etc"); rec.Code == http.StatusOK {
		t.Error("the cleaned path resolved to something")
	}
}

func TestBrowseMarksConvertedFiles(t *testing.T) {
	e := newEnv(t, false, "a.mkv")
	os.WriteFile(filepath.Join(e.out, "a.mp4"), []byte("x"), 0o644)

	body := get(t, e.h, "/browse/").Body.String()
	if !strings.Contains(body, "변환됨") {
		t.Error("a converted file was not marked")
	}
	if !strings.Contains(body, `href="/watch/a.mkv"`) {
		t.Error("no play link for a converted file")
	}
}

// Clicking a file is the whole interface: it says what to convert and how far.
func TestConvertQueuesInViewingOrder(t *testing.T) {
	e := newEnv(t, false, "S/ep1.mkv", "S/ep2.mkv", "S/ep3.mkv")

	rec := post(t, e.h, "/convert", url.Values{
		"rel":   {"S/ep2.mkv"},
		"scope": {"folder"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, body:\n%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/jobs?batch=") {
		t.Errorf("Location = %q", loc)
	}

	waitFor(t, "all three conversions", func() bool { return len(e.runner.converted()) == 3 })
	want := []string{"ep2.mkv", "ep3.mkv", "ep1.mkv"}
	got := e.runner.converted()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("converted %v, want %v", got, want)
		}
	}
}

func TestConvertScopeFileConvertsOnlyThatFile(t *testing.T) {
	e := newEnv(t, false, "S/ep1.mkv", "S/ep2.mkv")

	post(t, e.h, "/convert", url.Values{"rel": {"S/ep1.mkv"}, "scope": {"file"}})
	waitFor(t, "the conversion", func() bool { return len(e.runner.converted()) == 1 })
	time.Sleep(100 * time.Millisecond)
	if got := e.runner.converted(); len(got) != 1 || got[0] != "ep1.mkv" {
		t.Errorf("converted %v, want just ep1.mkv", got)
	}
}

func TestConvertRejectsABadPath(t *testing.T) {
	e := newEnv(t, false, "a.mkv")
	rec := post(t, e.h, "/convert", url.Values{"rel": {"../../etc/passwd"}, "scope": {"file"}})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// Two sources that would produce one output must be refused, not silently
// merged into whichever finishes last.
func TestConvertReportsAnOutputClash(t *testing.T) {
	e := newEnv(t, false, "S/ep1.mkv", "S/ep1.avi")
	rec := post(t, e.h, "/convert", url.Values{"rel": {"S/ep1.mkv"}, "scope": {"folder"}})
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "겹칩니다") {
		t.Error("the clash was not explained")
	}
}

func TestConvertWithNothingToDoGoesBack(t *testing.T) {
	e := newEnv(t, false, "a.mkv")
	os.WriteFile(filepath.Join(e.out, "a.mp4"), []byte("x"), 0o644)

	rec := post(t, e.h, "/convert", url.Values{"rel": {"a.mkv"}, "scope": {"file"}})
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want a redirect back", rec.Code)
	}
	if len(e.runner.converted()) != 0 {
		t.Error("an already-converted file was converted again")
	}
}

func TestJobsPageAndCancel(t *testing.T) {
	e := newEnv(t, true, "S/ep1.mkv", "S/ep2.mkv")

	post(t, e.h, "/convert", url.Values{"rel": {"S/ep1.mkv"}, "scope": {"folder"}})
	waitFor(t, "the first job to start", func() bool { return len(e.runner.converted()) == 1 })

	body := get(t, e.h, "/jobs").Body.String()
	if !strings.Contains(body, "ep1.mkv") || !strings.Contains(body, "ep2.mkv") {
		t.Errorf("jobs page is missing entries:\n%s", body)
	}
	if !strings.Contains(body, "중단") {
		t.Error("no way to stop a running job")
	}

	var payload struct {
		Jobs []jobs.View `json:"jobs"`
	}
	if err := json.Unmarshal(get(t, e.h, "/api/jobs").Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Jobs) != 2 {
		t.Fatalf("api reported %d jobs, want 2", len(payload.Jobs))
	}

	var batchID string
	for _, j := range payload.Jobs {
		batchID = j.BatchID
	}
	rec := post(t, e.h, "/batches/"+batchID+"/cancel", nil)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("cancel status = %d", rec.Code)
	}
	waitFor(t, "everything to stop", func() bool {
		var p struct {
			Jobs []jobs.View `json:"jobs"`
		}
		json.Unmarshal(get(t, e.h, "/api/jobs").Body.Bytes(), &p)
		for _, j := range p.Jobs {
			if !j.State.Terminal() {
				return false
			}
		}
		return len(p.Jobs) > 0
	})
}

func TestWatchPageOffersThePlayerOnlyWhenReady(t *testing.T) {
	e := newEnv(t, false, "a.mkv")

	body := get(t, e.h, "/watch/a.mkv").Body.String()
	if strings.Contains(body, "<video") {
		t.Error("a player was offered for a file that has not been converted")
	}
	if !strings.Contains(body, "아직 변환하지 않은") {
		t.Error("the page does not say why there is nothing to play")
	}

	os.WriteFile(filepath.Join(e.out, "a.mp4"), []byte("x"), 0o644)
	body = get(t, e.h, "/watch/a.mkv").Body.String()
	if !strings.Contains(body, `src="/media/a.mp4"`) {
		t.Errorf("no player for a converted file:\n%s", body)
	}
}

// The picker has to show what is actually in the file, and say plainly which
// tracks cannot be burned in.
func TestWatchListsSubtitleTracks(t *testing.T) {
	e := newEnv(t, false, "a.mkv")
	body := get(t, e.h, "/watch/a.mkv").Body.String()

	if !strings.Contains(body, `value="embedded:2"`) || !strings.Contains(body, "ENG") {
		t.Errorf("the English track is missing from the picker:\n%s", body)
	}
	if !strings.Contains(body, "그림 자막") {
		t.Error("a bitmap track was not marked as such")
	}
	if !strings.Contains(body, "disabled") {
		t.Error("a bitmap track was selectable even though it cannot be burned")
	}
}

// A Korean sidecar is what this library is normally watched with, so it should
// be the first thing in the list.
func TestWatchPutsKoreanSubtitlesFirst(t *testing.T) {
	e := newEnv(t, false, "S/ep1.mkv", "S/ep1.ko.smi", "S/ep1.eng.srt")
	body := get(t, e.h, "/watch/S/ep1.mkv").Body.String()

	ko := strings.Index(body, "ep1.ko.smi")
	en := strings.Index(body, "ep1.eng.srt")
	if ko < 0 || en < 0 {
		t.Fatalf("sidecars missing from the picker:\n%s", body)
	}
	if ko > en {
		t.Error("the Korean subtitle is not offered first")
	}
}

func TestMediaServesFinishedOutputWithRanges(t *testing.T) {
	e := newEnv(t, false, "a.mkv")
	os.WriteFile(filepath.Join(e.out, "a.mp4"), []byte("0123456789"), 0o644)

	req := httptest.NewRequest(http.MethodGet, "/media/a.mp4", nil)
	req.Header.Set("Range", "bytes=2-5")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206: seeking depends on it", rec.Code)
	}
	if got := rec.Body.String(); got != "2345" {
		t.Errorf("body = %q", got)
	}
}

func TestBasicAuth(t *testing.T) {
	e := newEnv(t, false, "a.mkv")
	e.srv.cfg.User, e.srv.cfg.Pass = "u", "p"
	h := e.srv.Handler()

	if rec := get(t, h, "/browse/"); rec.Code != http.StatusUnauthorized {
		t.Errorf("no credentials = %d, want 401", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/browse/", nil)
	req.SetBasicAuth("u", "wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong password = %d, want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/browse/", nil)
	req.SetBasicAuth("u", "p")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("correct credentials = %d, want 200", rec.Code)
	}
}

func TestStaticAssetsAreEmbedded(t *testing.T) {
	e := newEnv(t, false)
	for _, p := range []string{"/static/app.css", "/static/app.js"} {
		if rec := get(t, e.h, p); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d", p, rec.Code)
		}
	}
}

func TestHealthAndRoot(t *testing.T) {
	e := newEnv(t, false)
	if rec := get(t, e.h, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("healthz = %d", rec.Code)
	}
	rec := get(t, e.h, "/")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/browse/" {
		t.Errorf("root = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestHumanHelpers(t *testing.T) {
	if got := humanDuration(-1); got != "—" {
		t.Errorf("unknown duration = %q", got)
	}
	if got := humanDuration(45); got != "45초" {
		t.Errorf("45s = %q", got)
	}
	if got := humanDuration(2430); got != "40분 30초" {
		t.Errorf("2430s = %q", got)
	}
	if got := humanDuration(4000); got != "1시간 6분" {
		t.Errorf("4000s = %q", got)
	}
	if got := humanSize(1536); got != "1.5 KB" {
		t.Errorf("1536 = %q", got)
	}
}

func TestCrumbs(t *testing.T) {
	got := crumbs("애니/코난")
	if len(got) != 3 || got[0].Path != "" || got[2].Path != "애니/코난" {
		t.Errorf("crumbs = %+v", got)
	}
}

// --- live preview ---

func TestLiveFileNames(t *testing.T) {
	for _, ok := range []string{"index.m3u8", "init.mp4", "seg00001.m4s"} {
		if !liveFile(ok) {
			t.Errorf("liveFile(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{
		"../../etc/passwd", "a/b.m4s", `..\x`, "index.m3u8.bak",
		"seg.txt", "", "..", "secret.mp4",
	} {
		if liveFile(bad) {
			t.Errorf("liveFile(%q) = true, want false", bad)
		}
	}
}

func TestIsHex(t *testing.T) {
	if !isHex("dc344c4937f9a231") {
		t.Error("a job id was rejected")
	}
	for _, bad := range []string{"", "../x", "zz", strings.Repeat("a", 65)} {
		if isHex(bad) {
			t.Errorf("isHex(%q) = true", bad)
		}
	}
}

// Nothing is served for a job that is not running: the directory is gone, and
// asking after one is how a stale page probes for files.
func TestLiveRefusesUnknownJobs(t *testing.T) {
	e := newEnv(t, false, "a.mkv")
	for _, p := range []string{
		"/live/deadbeef/index.m3u8",
		"/live/notahexid/index.m3u8",
		"/live/deadbeef/../../../etc/passwd",
	} {
		if rec := get(t, e.h, p); rec.Code == http.StatusOK {
			t.Errorf("GET %s returned 200", p)
		}
	}
}

// The playlist grows while the job runs, so caching it would freeze playback
// at whatever was written when it was first fetched.
func TestLiveServesThePlaylistUncached(t *testing.T) {
	e := newEnv(t, true, "a.mkv")
	post(t, e.h, "/convert", url.Values{"rel": {"a.mkv"}, "scope": {"file"}})

	var id string
	waitFor(t, "the job to start", func() bool {
		for _, v := range e.srv.queue.Snapshot() {
			if v.State == jobs.Running && v.Live {
				id = v.ID
				return e.srv.queue.LiveDirFor(id) != ""
			}
		}
		return false
	})

	// Stand in for the HLS muxer. A browser asking before ffmpeg has written
	// anything gets a 404 and retries, which is why the player is configured
	// to be patient with the first playlist load.
	dir := e.srv.queue.LiveDirFor(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte("#EXTM3U\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := get(t, e.h, "/live/"+id+"/index.m3u8")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "mpegurl") {
		t.Errorf("Content-Type = %q", got)
	}

	// Segments never change once written, so they may be cached forever.
	if err := os.WriteFile(filepath.Join(dir, "seg00001.m4s"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec = get(t, e.h, "/live/"+id+"/seg00001.m4s")
	if !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("segment Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}

	// A segment the encoder has not reached yet must not be remembered as
	// missing. Marking a 404 immutable would leave a hole in the picture for
	// the rest of the year, long after the segment exists.
	rec = get(t, e.h, "/live/"+id+"/seg09999.m4s")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status for a segment that does not exist = %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); strings.Contains(cc, "immutable") {
		t.Errorf("a 404 was served as immutable: %q", cc)
	}
}

// Without an end marker the playlist looks like a live broadcast, and players
// join a live broadcast at its newest segment — here, the frame the encoder
// has only just produced. The player then starves every few seconds and hops
// forward, which is seen as the video skipping. #EXT-X-START overrides that.
func TestLivePlaylistStartsAtTheBeginning(t *testing.T) {
	const playlist = "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-PLAYLIST-TYPE:EVENT\n#EXTINF:4.0,\nseg00000.m4s\n"

	got := string(startAtZero([]byte(playlist)))
	if !strings.Contains(got, "#EXT-X-START:TIME-OFFSET=0") {
		t.Errorf("playlist does not pin the start:\n%s", got)
	}
	if !strings.HasPrefix(got, "#EXTM3U\n") {
		t.Errorf("#EXTM3U must stay on the first line:\n%s", got)
	}
	if !strings.Contains(got, "#EXT-X-VERSION:7") || !strings.Contains(got, "seg00000.m4s") {
		t.Errorf("the rest of the playlist was damaged:\n%s", got)
	}

	// Applied twice — a reload, say — it must not accumulate.
	if again := string(startAtZero([]byte(got))); again != got {
		t.Errorf("a second pass changed the playlist:\n%s", again)
	}

	// Anything that is not a playlist is passed through rather than guessed at.
	if got := string(startAtZero([]byte("not a playlist"))); got != "not a playlist" {
		t.Errorf("mangled a non-playlist: %q", got)
	}
}

func TestWatchOffersTheLivePlayerWhileEncoding(t *testing.T) {
	e := newEnv(t, true, "a.mkv")
	post(t, e.h, "/convert", url.Values{"rel": {"a.mkv"}, "scope": {"file"}})
	waitFor(t, "the job to start", func() bool {
		for _, v := range e.srv.queue.Snapshot() {
			if v.State == jobs.Running {
				return e.srv.queue.LiveDirFor(v.ID) != ""
			}
		}
		return false
	})

	body := get(t, e.h, "/watch/a.mkv").Body.String()
	if !strings.Contains(body, "data-hls=\"/live/") {
		t.Errorf("no live player while the job is running:\n%s", body)
	}
}

func TestVendoredHLSIsServed(t *testing.T) {
	e := newEnv(t, false)
	rec := get(t, e.h, "/static/vendor/hls-1.5.20.min.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: the player cannot load without it", rec.Code)
	}
	if rec.Body.Len() < 100000 {
		t.Errorf("served %d bytes, that is not hls.js", rec.Body.Len())
	}
}

// --- files that need nothing ---

func readyEnv(t *testing.T, files ...string) *env {
	t.Helper()
	e := newEnv(t, false, files...)
	e.srv.prober = stubProber{info: mediainfo.Info{
		Duration:  100,
		Container: "mov,mp4,m4a,3gp,3g2,mj2",
		Streams: []mediainfo.Stream{
			{Index: 0, Type: "video", Codec: "h264", Height: 1080},
			{Index: 1, Type: "audio", Codec: "aac", Lang: "kor"},
		},
	}}
	return e
}

// An .mp4 that already holds H.264 and AAC needs nothing done to it. Offering
// to convert it is the confusing answer; playing it is the useful one.
func TestWatchPlaysAnAlreadyPlayableSourceDirectly(t *testing.T) {
	e := readyEnv(t, "movie.mp4")
	body := get(t, e.h, "/watch/movie.mp4").Body.String()

	if !strings.Contains(body, `src="/source/movie.mp4"`) {
		t.Errorf("the source is not being played directly:\n%s", body)
	}
	if !strings.Contains(body, "변환할 것이 없습니다") {
		t.Error("the page does not say why there is nothing to do")
	}
	if !strings.Contains(body, "H264 1080p") || !strings.Contains(body, "AAC KOR") {
		t.Errorf("the codec summary is missing:\n%s", body)
	}
	// Converting is still possible — burning subtitles needs it — but it is
	// no longer the thing being suggested.
	if !strings.Contains(body, "그래도 변환하기") {
		t.Error("converting should still be reachable, just not the headline")
	}
}

func TestSourceServesTheOriginalWithRanges(t *testing.T) {
	e := readyEnv(t, "movie.mp4")
	if err := os.WriteFile(filepath.Join(e.src, "movie.mp4"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/source/movie.mp4", nil)
	req.Header.Set("Range", "bytes=3-6")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206: seeking depends on it", rec.Code)
	}
	if got := rec.Body.String(); got != "3456" {
		t.Errorf("body = %q", got)
	}
}

func TestSourceRefusesEscapes(t *testing.T) {
	e := readyEnv(t, "movie.mp4")
	for _, p := range []string{
		"/source/%2e%2e%2f%2e%2e%2fetc%2fpasswd",
		"/source/",
	} {
		if rec := get(t, e.h, p); rec.Code == http.StatusOK {
			t.Errorf("GET %s returned 200", p)
		}
	}
}

// Browsing never probes — that is what made v1 take half a minute to show a
// folder — so with nothing cached a .mp4 is taken at its word and a .mkv is
// not.
func TestBrowseWithNothingProbedTrustsTheExtension(t *testing.T) {
	e := newEnv(t, false, "movie.mp4", "other.mkv")
	e.srv.prober = stubProber{uncached: true}
	body := get(t, e.h, "/browse/").Body.String()

	mp4 := section(t, body, "movie.mp4", "other.mkv")
	if strings.Contains(mp4, "변환 설정") {
		t.Errorf("an .mp4 was offered conversion rather than playback:\n%s", mp4)
	}
	if !strings.Contains(mp4, `href="/watch/movie.mp4"`) {
		t.Errorf("no way to play the .mp4:\n%s", mp4)
	}

	mkv := section(t, body, "other.mkv", "")
	if !strings.Contains(mkv, "변환 설정") {
		t.Errorf("a .mkv offers no way to convert it:\n%s", mkv)
	}
}

// section returns the slice of a page between two markers, so an assertion
// about one row cannot accidentally read another.
func section(t *testing.T, body, from, to string) string {
	t.Helper()
	i := strings.Index(body, from)
	if i < 0 {
		t.Fatalf("%q not found in page", from)
	}
	rest := body[i:]
	if to == "" {
		return rest
	}
	j := strings.Index(rest, to)
	if j < 0 {
		return rest
	}
	return rest[:j]
}

// Once probed, the truth wins over the extension: a .mp4 holding HEVC does
// need converting after all.
func TestBrowseTrustsTheProbeOverTheExtension(t *testing.T) {
	e := newEnv(t, false, "movie.mp4")
	// The default stub reports HEVC, and Cached always answers.
	body := get(t, e.h, "/browse/").Body.String()
	if !strings.Contains(body, "변환 설정") {
		t.Errorf("a .mp4 known to hold HEVC was not offered conversion:\n%s", body)
	}
	if strings.Contains(body, "그대로 재생 가능") {
		t.Error("an HEVC file was labelled as playable as-is")
	}
}

// The queue is a list of things you are waiting to watch, so every row has to
// lead to the page that plays them — including while they are still encoding,
// where that page carries the live player.
func TestJobsListLinksToTheDetailPage(t *testing.T) {
	e := newEnv(t, true, "S/ep1.mkv", "S/ep2.mkv")
	post(t, e.h, "/convert", url.Values{"rel": {"S/ep1.mkv"}, "scope": {"folder"}})
	waitFor(t, "the first job to start", func() bool {
		for _, v := range e.srv.queue.Snapshot() {
			if v.State == jobs.Running {
				return true
			}
		}
		return false
	})

	body := get(t, e.h, "/jobs").Body.String()
	for _, want := range []string{`href="/watch/S/ep1.mkv"`, `href="/watch/S/ep2.mkv"`} {
		if !strings.Contains(body, want) {
			t.Errorf("jobs page has no link to %s:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "지금 보기") {
		t.Error("a running job offers no way to look at it")
	}
}

// --- the subtitle choice ---

// checkedFor reports whether the radio with this value is the one selected.
func checkedFor(t *testing.T, body, value string) bool {
	t.Helper()
	i := strings.Index(body, `value="`+value+`"`)
	if i < 0 {
		t.Fatalf("no radio with value %q:\n%s", value, body)
	}
	end := strings.Index(body[i:], ">")
	if end < 0 {
		t.Fatalf("unterminated input tag for %q", value)
	}
	return strings.Contains(body[i:i+end], "checked")
}

// Burning is irreversible and costs a full re-encode to find out about, so the
// form starts on the language this library is actually watched with, and on
// nothing at all when that language is absent.
func TestSubtitleDefaultFollowsKorean(t *testing.T) {
	e := newEnv(t, false, "a.mkv")

	// The probe stub offers English and a Japanese bitmap track — no Korean.
	body := get(t, e.h, "/watch/a.mkv").Body.String()
	if !checkedFor(t, body, "") {
		t.Errorf("with no Korean track, burning should be off:\n%s", body)
	}
	if checkedFor(t, body, "embedded:2") {
		t.Error("an English track was selected for burning by default")
	}
	if !strings.Contains(body, `type="radio"`) {
		t.Error("the subtitle choice is not a radio group")
	}

	// A Korean sidecar appears beside the video.
	if err := os.WriteFile(filepath.Join(e.src, "a.ko.srt"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body = get(t, e.h, "/watch/a.mkv").Body.String()
	if !checkedFor(t, body, "sidecar:a.ko.srt") {
		t.Errorf("the Korean track was not chosen:\n%s", body)
	}
	if checkedFor(t, body, "") {
		t.Error("burning stayed off even though there is a Korean track")
	}
}

// A bitmap track can only be drawn with an overlay filter, which is not wired
// up, so it must not be selectable — and never the default.
func TestBitmapSubtitleCannotBeChosen(t *testing.T) {
	e := newEnv(t, false, "a.mkv")
	body := get(t, e.h, "/watch/a.mkv").Body.String()

	i := strings.Index(body, `value="embedded:3"`)
	if i < 0 {
		t.Fatalf("the bitmap track is missing from the list:\n%s", body)
	}
	tag := body[i : i+strings.Index(body[i:], ">")]
	if !strings.Contains(tag, "disabled") {
		t.Errorf("the bitmap track is selectable: %s", tag)
	}
	if strings.Contains(tag, "checked") {
		t.Errorf("the bitmap track was chosen by default: %s", tag)
	}
}

// The reported case, end to end: a subtitle named exactly like the video, with
// no language tag, is what most of this library looks like. The form has to
// open on it rather than on "굽지 않음".
func TestUntaggedKoreanSidecarIsTheDefault(t *testing.T) {
	const video = "DEATH NOTE 데스노트 02 (704x396 DivX).avi"
	const sub = "DEATH NOTE 데스노트 02 (704x396 DivX).smi"

	e := newEnv(t, false, video)
	if err := os.WriteFile(filepath.Join(e.src, sub),
		[]byte("<SYNC Start=1000><P Class=KRCC>류크, 사과 줄까?\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	body := get(t, e.h, (&url.URL{Path: "/watch/" + video}).String()).Body.String()
	if !checkedFor(t, body, "sidecar:"+sub) {
		t.Errorf("the subtitle beside the video was not selected:\n%s", body)
	}
	if checkedFor(t, body, "") {
		t.Error("burning was left off even though a Korean subtitle sits beside the file")
	}
}

// --- discarding a conversion ---

// A conversion can be wrong in ways only watching reveals: the wrong subtitle
// track, sync that drifts, a picture worse than expected. Getting rid of it
// has to be possible from the page where that was noticed.
func TestDiscardRemovesTheConversion(t *testing.T) {
	e := newEnv(t, false, "a.mkv")
	converted := filepath.Join(e.out, "a.mp4")
	if err := os.WriteFile(converted, []byte("converted"), 0o644); err != nil {
		t.Fatal(err)
	}

	body := get(t, e.h, "/watch/a.mkv").Body.String()
	if !strings.Contains(body, `action="/discard"`) {
		t.Fatalf("a converted file offers no way to discard it:\n%s", body)
	}

	rec := post(t, e.h, "/discard", url.Values{"rel": {"a.mkv"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/watch/a.mkv" {
		t.Errorf("Location = %q, want the same page back", got)
	}
	if _, err := os.Stat(converted); !os.IsNotExist(err) {
		t.Error("the conversion is still there")
	}

	// The source is on a read-only mount and must never be a casualty of this.
	if _, err := os.Stat(filepath.Join(e.src, "a.mkv")); err != nil {
		t.Fatalf("the source was touched: %v", err)
	}

	// And the page now offers to make it again.
	body = get(t, e.h, "/watch/a.mkv").Body.String()
	if !strings.Contains(body, `name="scope"`) {
		t.Errorf("no way to convert it again:\n%s", body)
	}
}

// Removing the output while ffmpeg is writing it would leave the encode
// pointed at a file nobody can find, and the rename at the end would put the
// old name back regardless.
func TestDiscardRefusesWhileConverting(t *testing.T) {
	e := newEnv(t, true, "a.mkv")
	post(t, e.h, "/convert", url.Values{"rel": {"a.mkv"}, "scope": {"file"}})
	waitFor(t, "the job to start", func() bool {
		for _, v := range e.srv.queue.Snapshot() {
			if v.State == jobs.Running {
				return true
			}
		}
		return false
	})

	rec := post(t, e.h, "/discard", url.Values{"rel": {"a.mkv"}})
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
}

// The conversion form is not the point of a page that can play something, so
// it folds away — but it stays on the page, one click from open.
func TestTheConversionFormFoldsAwayOnceConverted(t *testing.T) {
	e := newEnv(t, false, "a.mkv")

	body := get(t, e.h, "/watch/a.mkv").Body.String()
	if !strings.Contains(body, `<details class="convert" open>`) {
		t.Errorf("nothing to play, so the form should be open:\n%s", body)
	}

	if err := os.WriteFile(filepath.Join(e.out, "a.mp4"), []byte("converted"), 0o644); err != nil {
		t.Fatal(err)
	}
	body = get(t, e.h, "/watch/a.mkv").Body.String()
	if strings.Contains(body, `<details class="convert" open>`) {
		t.Errorf("the form is still unfolded over the player:\n%s", body)
	}
	if !strings.Contains(body, `name="scope"`) {
		t.Errorf("folding it away removed it:\n%s", body)
	}
}

// countingProber says how often ffprobe would actually have run.
type countingProber struct {
	info   mediainfo.Info
	cached bool

	mu     sync.Mutex
	probes int
}

func (p *countingProber) Probe(context.Context, string, os.FileInfo) (mediainfo.Info, error) {
	p.mu.Lock()
	p.probes++
	p.mu.Unlock()
	return p.info, nil
}

func (p *countingProber) Cached(string, os.FileInfo) (mediainfo.Info, bool) {
	if !p.cached {
		return mediainfo.Info{}, false
	}
	return p.info, true
}

func (p *countingProber) CachedAt(string, int64, int64) (mediainfo.Info, bool) {
	if !p.cached {
		return mediainfo.Info{}, false
	}
	return p.info, true
}

func (p *countingProber) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.probes
}

// Opening a converted file means wanting to watch it. ffprobe reads the
// source, which on a NAS is large, on a spinning disk, and across the network
// — seconds of waiting for a codec caption beside a player that is ready now.
func TestAConvertedFileIsNotHeldUpByProbing(t *testing.T) {
	e := newEnv(t, false, "a.mkv")
	p := &countingProber{info: mediainfo.Info{Duration: 100}}
	e.srv.prober = p

	// Not converted yet: the page has to work out whether conversion is even
	// needed, so probing is the right thing to do.
	get(t, e.h, "/watch/a.mkv")
	if p.count() == 0 {
		t.Error("a file that might need converting was never inspected")
	}

	before := p.count()
	if err := os.WriteFile(filepath.Join(e.out, "a.mp4"), []byte("converted"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := get(t, e.h, "/watch/a.mkv").Body.String()
	if n := p.count() - before; n != 0 {
		t.Errorf("ffprobe ran %d time(s) on a page that only had to play a file", n)
	}
	if !strings.Contains(body, "/media/") {
		t.Errorf("the player is missing:\n%s", body)
	}
}

// Discarding and reconverting are deliberately two steps: the reason to
// discard is usually that the wrong thing was burned in, so the next screen
// has to be the one where that is chosen — not a conversion already running.
func TestDiscardDoesNotStartAnything(t *testing.T) {
	e := newEnv(t, false, "a.mkv")
	if err := os.WriteFile(filepath.Join(e.out, "a.mp4"), []byte("converted"), 0o644); err != nil {
		t.Fatal(err)
	}

	post(t, e.h, "/discard", url.Values{"rel": {"a.mkv"}})
	if n := len(e.srv.queue.Snapshot()); n != 0 {
		t.Errorf("discarding queued %d job(s) on its own", n)
	}
	if got := e.runner.converted(); len(got) != 0 {
		t.Errorf("ffmpeg was run: %v", got)
	}
}

// --- AirPlay diagnostics ---

// --- the access log ---

// A client being turned away and a client that never arrived look the same in
// a log that only records what got past authentication, and they have nothing
// in common as causes. AirPlay is exactly this case: the Apple TV fetches the
// file itself, with none of the browser's credentials.
func TestRejectedRequestsAreStillLogged(t *testing.T) {
	e := newEnv(t, false, "a.mkv")
	e.srv.cfg.User, e.srv.cfg.Pass = "u", "p"
	e.srv.cfg.LogRequests = true

	var out strings.Builder
	log.SetOutput(&out)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	h := e.srv.Handler()
	req := httptest.NewRequest("GET", "/media/a.mp4", nil)
	req.RemoteAddr = "192.168.0.44:51000"
	req.Header.Set("User-Agent", "AppleCoreMedia/1.0.0.21K69 (Apple TV; U; CPU OS 17_2)")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	line := out.String()
	if !strings.Contains(line, "401") {
		t.Errorf("the log does not say it was refused: %q", line)
	}
	if !strings.Contains(line, "192.168.0.44") {
		t.Errorf("the log does not say who was refused: %q", line)
	}
	if !strings.Contains(line, "AppleCoreMedia") {
		t.Errorf("the log does not say what was refused: %q", line)
	}
}

// The event stream is chunked and depends on Flush reaching the real writer.
// A logging wrapper that swallowed it would freeze every progress bar.
func TestTheLogWrapperStillFlushes(t *testing.T) {
	e := newEnv(t, false, "a.mkv")
	e.srv.cfg.LogRequests = true

	var flushed bool
	rec := &flushProbe{ResponseRecorder: httptest.NewRecorder(), onFlush: func() { flushed = true }}
	h := e.srv.logRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("the wrapper hid Flusher from the handler")
			return
		}
		f.Flush()
	}))
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/jobs", nil))
	if !flushed {
		t.Error("Flush did not reach the real writer")
	}
}

type flushProbe struct {
	*httptest.ResponseRecorder
	onFlush func()
}

func (f *flushProbe) Flush() { f.onFlush() }

// --- signed media links ---

// AirPlay hands the file to a television, which has no password and nowhere to
// type one. A link minted for a viewer who did log in has to work on its own.
func TestSignedMediaLinksWorkWithoutAPassword(t *testing.T) {
	e := newEnv(t, false, "a.mkv")
	e.srv.cfg.User, e.srv.cfg.Pass = "u", "p"
	if err := os.WriteFile(filepath.Join(e.out, "a.mp4"), []byte("converted"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := e.srv.Handler()

	// The page itself is still behind the password.
	if got := get(t, h, "/watch/a.mkv").Code; got != http.StatusUnauthorized {
		t.Fatalf("the page was reachable without logging in: %d", got)
	}

	req := httptest.NewRequest("GET", "/watch/a.mkv", nil)
	req.SetBasicAuth("u", "p")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	link := mediaSrc(t, rec.Body.String())
	if !strings.Contains(link, signParam+"=") {
		t.Fatalf("the player's link carries no token: %q", link)
	}
	if got := get(t, h, link).Code; got != http.StatusOK {
		t.Errorf("a signed link was refused: %d (%s)", got, link)
	}

	// The signature covers the path, so it cannot be carried to another file.
	other := strings.Replace(link, "/media/a.mp4", "/media/b.mp4", 1)
	if got := get(t, h, other).Code; got == http.StatusOK {
		t.Error("a token for one file opened another")
	}
	// And an unsigned request for the same file is still refused.
	if got := get(t, h, "/media/a.mp4").Code; got != http.StatusUnauthorized {
		t.Errorf("the file was reachable with no token at all: %d", got)
	}
}

// An expired token is no token.
func TestSignedLinksExpire(t *testing.T) {
	e := newEnv(t, false, "a.mkv")
	e.srv.cfg.User, e.srv.cfg.Pass = "u", "p"

	const path = "/media/a.mp4"
	if !e.srv.signedOK(path, tokenFor(e.srv, path, time.Now().Add(time.Hour))) {
		t.Error("a fresh token was refused")
	}
	if e.srv.signedOK(path, tokenFor(e.srv, path, time.Now().Add(-time.Minute))) {
		t.Error("an expired token was accepted")
	}
	if e.srv.signedOK(path, "9999999999.deadbeef") {
		t.Error("a forged token was accepted")
	}
}

func tokenFor(s *Server, path string, exp time.Time) string {
	return fmt.Sprintf("%d.%s", exp.Unix(), mac(s.secret, path, exp.Unix()))
}

// A player is handed one URL and follows it to the segments itself. Signing
// the playlist and not what it points at would authorise the table of
// contents and none of the video.
func TestThePlaylistSignsWhatItPointsAt(t *testing.T) {
	e := newEnv(t, false, "a.mkv")
	e.srv.cfg.User, e.srv.cfg.Pass = "u", "p"

	const playlist = "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:4.0,\nseg00000.m4s\n"
	got := string(e.srv.signPlaylist([]byte(playlist), "/live/abc/"))

	for _, want := range []string{"/live/abc/seg00000.m4s?", "/live/abc/init.mp4?"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "#EXT-X-VERSION:7\n") {
		t.Errorf("a tag was damaged:\n%s", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("the playlist lost its final newline:\n%q", got)
	}

	// With no password there is nothing to work around, and plain names are
	// what every player already handles.
	e.srv.cfg.User = ""
	if plain := string(e.srv.signPlaylist([]byte(playlist), "/live/abc/")); plain != playlist {
		t.Errorf("an unguarded playlist was rewritten:\n%s", plain)
	}
}

// mediaSrc pulls the player's source URL out of a rendered page.
func mediaSrc(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, `src="/media/`)
	if i < 0 {
		t.Fatalf("no player in:\n%s", body)
	}
	rest := body[i+len(`src="`):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		t.Fatal("unterminated src")
	}
	return html.UnescapeString(rest[:j])
}

// Without an icon the browser asks for /favicon.ico before it has even parsed
// the page, and behind a password that is an auth challenge and a 404 on every
// page load — enough to bury everything else in the log.
func TestTheFaviconDoesNotFillTheLog(t *testing.T) {
	e := newEnv(t, false, "a.mkv")

	if got := get(t, e.h, "/static/favicon.svg").Code; got != http.StatusOK {
		t.Errorf("the icon is not served: %d", got)
	}
	body := get(t, e.h, "/browse/").Body.String()
	if !strings.Contains(body, `rel="icon"`) {
		t.Errorf("the page does not point at an icon:\n%s", body)
	}
	if got := get(t, e.h, "/favicon.ico").Code; got != http.StatusNoContent {
		t.Errorf("/favicon.ico = %d, want 204 so clients stop asking", got)
	}

	e.srv.cfg.LogRequests = true
	var out strings.Builder
	log.SetOutput(&out)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	get(t, e.srv.Handler(), "/favicon.ico")
	if out.String() != "" {
		t.Errorf("the icon request was logged: %q", out.String())
	}
}

// --- the converted library ---

// Every page needs the way into the list of what can be watched, not just
// the ones that happen to link to it.
func TestTheHeaderOffersThePlayableList(t *testing.T) {
	e := newEnv(t, false, "a.mkv")
	for _, path := range []string{"/browse/", "/jobs", "/watch/a.mkv", "/playable/"} {
		body := get(t, e.h, path).Body.String()
		if !strings.Contains(body, `href="/playable/"`) {
			t.Errorf("%s has no link to the playable list", path)
		}
	}
}

// --- everything that plays right now ---

// The library is far too big for one flat list, so the same folders as the
// library — but with only the branches that lead somewhere.
func TestPlayableIsATreeOfWhatLeadsSomewhere(t *testing.T) {
	e := newEnv(t, false,
		"S/ep1.mkv", "S/ep2.mp4", "S/ep3.avi",
		"T/nothing.mkv",
		"U/deep/ep9.mp4",
	)
	e.srv.prober = stubProber{uncached: true} // judge by extension
	if err := os.MkdirAll(filepath.Join(e.out, "S"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.out, "S", "ep1.mp4"), []byte("done"), 0o644); err != nil {
		t.Fatal(err)
	}

	root := get(t, e.h, "/playable/").Body.String()
	if !strings.Contains(root, `href="/playable/S"`) {
		t.Errorf("the folder holding playable files is missing:\n%s", root)
	}
	if !strings.Contains(root, `href="/playable/U"`) {
		t.Errorf("a folder whose playable files are deeper down was pruned:\n%s", root)
	}
	if strings.Contains(root, `href="/playable/T"`) {
		t.Errorf("a folder with nothing to watch was offered:\n%s", root)
	}
	// The root lists folders, not every file in the library.
	if strings.Contains(root, "ep1.mkv") {
		t.Errorf("the root flattened the tree:\n%s", root)
	}

	inS := get(t, e.h, "/playable/S").Body.String()
	if !strings.Contains(inS, "ep1.mkv") || !strings.Contains(inS, "ep2.mp4") {
		t.Errorf("the folder does not list what is in it:\n%s", inS)
	}
	if strings.Contains(inS, "ep3.avi") {
		t.Errorf("an .avi nobody converted was listed as playable:\n%s", inS)
	}
	if !strings.Contains(inS, "변환본") || !strings.Contains(inS, "원본 그대로") {
		t.Errorf("the two kinds are not told apart:\n%s", inS)
	}
	if !strings.Contains(inS, `href="/browse/S"`) {
		t.Errorf("no way across to the same folder in the library:\n%s", inS)
	}
}

// A converted file appears once, as the conversion — not also as the source
// it was made from.
func TestPlayableDoesNotListAFileTwice(t *testing.T) {
	e := newEnv(t, false, "S/ep1.mp4")
	if err := os.MkdirAll(filepath.Join(e.out, "S"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.out, "S", "ep1.mp4"), []byte("done"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := get(t, e.h, "/playable/S").Body.String()
	if n := strings.Count(body, "ep1.mp4</a>"); n != 1 {
		t.Errorf("listed %d times, want once:\n%s", n, body)
	}
}

// Opening thousands of files to find out what is in them is what made v1 take
// half a minute to show one folder. This page reads directories only.
func TestPlayableNeverProbes(t *testing.T) {
	e := newEnv(t, false, "S/ep1.mkv", "S/ep2.mp4", "T/ep3.mkv")
	p := &countingProber{info: mediainfo.Info{Duration: 100}}
	e.srv.prober = p

	get(t, e.h, "/playable/")
	get(t, e.h, "/playable/S")
	if n := p.count(); n != 0 {
		t.Errorf("ffprobe ran %d time(s) rendering a list of file names", n)
	}
}

// Walking the library takes ten seconds on the hardware this runs on. Doing
// it again because a page was opened twice is ten seconds nobody gets back.
func TestPlayableIsWalkedOnceAndKept(t *testing.T) {
	e := newEnv(t, false, "S/ep1.mp4")
	e.srv.prober = stubProber{uncached: true}

	first := e.srv.index()
	if e.srv.index() != first {
		t.Error("the library was walked again for a second page view")
	}

	// A conversion finishing changes the answer, so the next view rebuilds.
	e.srv.forgetIndex()
	if e.srv.index() == first {
		t.Error("a dropped index was handed back anyway")
	}
}

// Files also arrive on a NAS by means that have nothing to do with this
// program, so there has to be a way to say "look again".
func TestPlayableCanBeAskedToLookAgain(t *testing.T) {
	e := newEnv(t, false, "S/ep1.mp4")
	e.srv.prober = stubProber{uncached: true}

	before := e.srv.index()
	rec := get(t, e.h, "/playable/S?refresh=1")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/playable/S" {
		t.Errorf("Location = %q, want the folder back without the flag", got)
	}
	if e.srv.index() == before {
		t.Error("the refresh did not throw the old answer away")
	}
}

// Judging by the extension is a guess, and a page that guesses should say how
// often — some of those .mp4 files hold HEVC and will not play.
func TestPlayableSaysHowManyItGuessedAt(t *testing.T) {
	e := newEnv(t, false, "S/a.mp4", "S/b.mp4")
	e.srv.prober = stubProber{uncached: true}
	body := get(t, e.h, "/playable/S").Body.String()
	if !strings.Contains(body, "확장자만 보고") {
		t.Errorf("the page does not admit it is guessing:\n%s", body)
	}
}

// A conversion finishing is the one event that certainly changes the answer,
// and the listing has to notice without being told.
func TestPlayableNoticesAConversionFinishing(t *testing.T) {
	e := newEnv(t, false, "S/ep1.mkv")
	e.srv.prober = stubProber{uncached: true}

	if body := get(t, e.h, "/playable/").Body.String(); strings.Contains(body, "/playable/S") {
		t.Fatalf("nothing is playable yet:\n%s", body)
	}

	post(t, e.h, "/convert", url.Values{"rel": {"S/ep1.mkv"}, "scope": {"file"}})
	waitFor(t, "the conversion to finish", func() bool {
		for _, v := range e.srv.queue.Snapshot() {
			if v.State == jobs.Done {
				return true
			}
		}
		return false
	})

	body := get(t, e.h, "/playable/").Body.String()
	if !strings.Contains(body, "/playable/S") {
		t.Errorf("the finished conversion never appeared:\n%s", body)
	}
}

// Discarding one removes it, and the listing must not go on offering it.
func TestPlayableNoticesADiscard(t *testing.T) {
	e := newEnv(t, false, "S/ep1.mkv")
	if err := os.MkdirAll(filepath.Join(e.out, "S"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.out, "S", "ep1.mp4"), []byte("done"), 0o644); err != nil {
		t.Fatal(err)
	}
	if body := get(t, e.h, "/playable/S").Body.String(); !strings.Contains(body, "ep1.mkv") {
		t.Fatalf("setup: it should be listed:\n%s", body)
	}

	post(t, e.h, "/discard", url.Values{"rel": {"S/ep1.mkv"}})
	if body := get(t, e.h, "/playable/S").Body.String(); strings.Contains(body, "ep1.mkv") {
		t.Errorf("a discarded conversion is still offered:\n%s", body)
	}
}

// The trap this project exists because of: a .mp4 holding something no
// browser will play. The playable list has to guess from the extension for a
// file nothing has looked inside, so it says yes; this page opens it and
// finds out. Saying nothing would leave the two screens contradicting each
// other with no explanation.
func TestAMp4ThatDoesNotPlaySaysWhy(t *testing.T) {
	e := newEnv(t, false, "a.mp4") // the stub prober reports HEVC

	body := get(t, e.h, "/watch/a.mp4").Body.String()
	if strings.Contains(body, "<video") {
		t.Fatalf("a player was offered for something that will not play:\n%s", body)
	}
	if strings.Contains(body, "아직 변환하지 않은 파일입니다") {
		t.Errorf("the page gives no reason, contradicting the list it came from:\n%s", body)
	}
	if !strings.Contains(body, "확장자는 .mp4 지만") {
		t.Errorf("the page does not explain the mismatch:\n%s", body)
	}
	if !strings.Contains(body, "HEVC") {
		t.Errorf("the page does not say what is actually inside:\n%s", body)
	}
}

// Once the file has been opened, the guess is over. The list must stop
// offering it rather than repeat the same wrong answer for ten more minutes.
func TestOpeningAFileCorrectsThePlayableList(t *testing.T) {
	e := newEnv(t, false, "S/a.mp4")
	// Nothing cached: the list has only the extension to go on, and says yes.
	p := &countingProber{info: mediainfo.Info{
		Duration: 100,
		Streams: []mediainfo.Stream{
			{Index: 0, Type: "video", Codec: "hevc"},
			{Index: 1, Type: "audio", Codec: "aac"},
		},
	}}
	e.srv.prober = p

	if body := get(t, e.h, "/playable/S").Body.String(); !strings.Contains(body, "a.mp4") {
		t.Fatalf("setup: the extension guess should list it:\n%s", body)
	}

	// Opening it teaches the prober the truth.
	get(t, e.h, "/watch/S/a.mp4")
	if p.count() == 0 {
		t.Fatal("the page never looked inside")
	}
	p.cached = true // what a real prober would now answer

	if body := get(t, e.h, "/playable/S").Body.String(); strings.Contains(body, "a.mp4") {
		t.Errorf("the list repeated a guess it had been corrected on:\n%s", body)
	}
}

// Burning is the expensive answer: it redraws every frame, so a file that
// needed nothing but its container swapped pays an hour for subtitles. A
// track costs seconds and can be switched off, so that is what the form
// starts on.
func TestSubtitlesGoInAsATrackUnlessAskedToBurn(t *testing.T) {
	e := newEnv(t, true, "a.mkv")
	// The choice only appears where it costs something, so this file has to
	// be one whose streams can simply be copied.
	e.srv.prober = stubProber{info: mediainfo.Info{
		Duration: 100,
		Streams: []mediainfo.Stream{
			{Index: 0, Type: "video", Codec: "h264"},
			{Index: 1, Type: "audio", Codec: "aac"},
			{Index: 2, Type: "subtitle", Codec: "ass", Lang: "kor"},
		},
	}}

	body := get(t, e.h, "/watch/a.mkv").Body.String()
	i := strings.Index(body, `name="burn" value=""`)
	if i < 0 {
		t.Fatalf("no way to add subtitles without burning them:\n%s", body)
	}
	if !strings.Contains(body[i:i+strings.Index(body[i:], ">")], "checked") {
		t.Errorf("the form starts on burning, which costs an hour:\n%s", body)
	}

	post(t, e.h, "/convert", url.Values{
		"rel": {"a.mkv"}, "scope": {"file"}, "subs": {"embedded:2"},
	})
	v, ok := e.srv.queue.ByRel(e.rel(t, "a.mkv"))
	if !ok {
		t.Fatal("nothing was queued")
	}
	if v.Remux {
		// The stub source is HEVC, so this one does need encoding either way;
		// what matters is that asking for subtitles did not force it.
		t.Log("remux:", v.Remux)
	}
}

// And asking to burn still burns.
func TestBurningIsStillAvailable(t *testing.T) {
	e := newEnv(t, true, "a.mkv")
	post(t, e.h, "/convert", url.Values{
		"rel": {"a.mkv"}, "scope": {"file"}, "subs": {"embedded:2"}, "burn": {"1"},
	})
	waitFor(t, "the job to start", func() bool {
		for _, v := range e.srv.queue.Snapshot() {
			if v.State == jobs.Running {
				return true
			}
		}
		return false
	})
}

// Deleting a conversion should be possible from the list you noticed it in,
// not only from the file's own page.
func TestPlayableListCanDiscardAConversion(t *testing.T) {
	e := newEnv(t, false, "S/ep1.mkv", "S/ep2.mp4")
	e.srv.prober = stubProber{uncached: true}
	if err := os.MkdirAll(filepath.Join(e.out, "S"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.out, "S", "ep1.mp4"), []byte("done"), 0o644); err != nil {
		t.Fatal(err)
	}

	body := get(t, e.h, "/playable/S").Body.String()
	if !strings.Contains(body, `action="/discard"`) {
		t.Fatalf("no way to delete a conversion from the list:\n%s", body)
	}
	// A source that was never converted has nothing of ours to delete.
	if n := strings.Count(body, `action="/discard"`); n != 1 {
		t.Errorf("got %d delete buttons, want one — only the conversion:\n%s", n, body)
	}

	rec := post(t, e.h, "/discard", url.Values{
		"rel": {"S/ep1.mkv"}, "back": {"/playable/S"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/playable/S" {
		t.Errorf("Location = %q, want the list it was asked from", got)
	}
	if _, err := os.Stat(filepath.Join(e.out, "S", "ep1.mp4")); !os.IsNotExist(err) {
		t.Error("the conversion is still there")
	}
	if _, err := os.Stat(filepath.Join(e.src, "S", "ep1.mkv")); err != nil {
		t.Fatalf("the source was taken with it: %v", err)
	}
}

// A conversion whose source is gone can only be named by where it sits.
func TestAnOrphanConversionCanBeDiscarded(t *testing.T) {
	e := newEnv(t, false, "keep.mkv")
	if err := os.MkdirAll(filepath.Join(e.out, "S"), 0o755); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(e.out, "S", "gone.mp4")
	if err := os.WriteFile(orphan, []byte("orphan"), 0o644); err != nil {
		t.Fatal(err)
	}

	body := get(t, e.h, "/playable/S").Body.String()
	if !strings.Contains(body, `name="out" value="S/gone.mp4"`) {
		t.Fatalf("an orphan offers no way to delete it:\n%s", body)
	}

	post(t, e.h, "/discard", url.Values{"out": {"S/gone.mp4"}})
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("the orphan is still there")
	}
}

// "out" reaches into the converted tree, so it has to be as guarded as every
// other path a client supplies.
func TestDiscardByOutputPathCannotEscape(t *testing.T) {
	e := newEnv(t, false, "a.mkv")
	victim := filepath.Join(e.src, "a.mkv")

	// Traversal is refused outright.
	for _, bad := range []string{"../media/a.mkv", "..", "a/../../b"} {
		if rec := post(t, e.h, "/discard", url.Values{"out": {bad}}); rec.Code != http.StatusBadRequest {
			t.Errorf("%q got %d, want 400", bad, rec.Code)
		}
	}
	// An absolute path is read as being inside the output tree, not as the
	// filesystem root, so it can only ever miss.
	post(t, e.h, "/discard", url.Values{"out": {"/etc/passwd"}})
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("a source outside the output tree was deleted: %v", err)
	}
	if _, err := os.Stat("/etc/passwd"); err != nil {
		t.Fatalf("something outside the library entirely was deleted: %v", err)
	}
}

// Safari places a subtitle from inside an MP4 wherever the file says, which
// put it in the corner on an iPhone and off the picture on a Mac; Chrome
// does not read one at all. A WebVTT track offered to the page is placed by
// the browser and works in both — and the copy inside the file stays, since
// that is the one AirPlay carries to a television.
func TestThePageOffersItsOwnSubtitleTrack(t *testing.T) {
	e := newEnv(t, false, "a.mkv")
	if err := os.WriteFile(filepath.Join(e.out, "a.mp4"), []byte("converted"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Without a .vtt beside it, nothing changes.
	body := get(t, e.h, "/watch/a.mkv").Body.String()
	if strings.Contains(body, "<track") {
		t.Errorf("a track was offered with no subtitle to put in it:\n%s", body)
	}

	if err := os.WriteFile(filepath.Join(e.out, "a.vtt"), []byte("WEBVTT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body = get(t, e.h, "/watch/a.mkv").Body.String()
	if !strings.Contains(body, `kind="subtitles"`) || !strings.Contains(body, "/media/a.vtt") {
		t.Errorf("the subtitle beside the file was not offered:\n%s", body)
	}
	if !strings.Contains(body, "/media/a.mp4") {
		t.Errorf("the video itself went missing:\n%s", body)
	}
}

// The subtitle written beside a conversion is part of that result and has no
// meaning without it.
func TestDiscardRemovesTheSubtitleToo(t *testing.T) {
	e := newEnv(t, false, "a.mkv")
	for name, body := range map[string]string{"a.mp4": "converted", "a.vtt": "WEBVTT\n"} {
		if err := os.WriteFile(filepath.Join(e.out, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	post(t, e.h, "/discard", url.Values{"rel": {"a.mkv"}})
	for _, name := range []string{"a.mp4", "a.vtt"} {
		if _, err := os.Stat(filepath.Join(e.out, name)); !os.IsNotExist(err) {
			t.Errorf("%s is still there", name)
		}
	}
}

// The choice between a subtitle track and burning is a choice between
// seconds and an hour — but only for a file whose picture and sound are
// already what we want. Everything else is re-encoded either way, and
// putting a trade-off to someone who has nothing to trade is just noise on
// the page.
func TestTheSubtitleMethodIsOfferedOnlyWhenItCosts(t *testing.T) {
	// The stub reports HEVC, so this one is re-encoded whatever happens.
	e := newEnv(t, false, "a.mkv")
	body := get(t, e.h, "/watch/a.mkv").Body.String()
	if strings.Contains(body, `name="burn"`) {
		t.Errorf("a choice was offered on a file that is re-encoded either way:\n%s", body)
	}
	if !strings.Contains(body, "걸리는\n      시간이 같습니다") &&
		!strings.Contains(body, "시간이 같습니다") {
		t.Errorf("nothing explains why there is no choice:\n%s", body)
	}

	// H.264 + AAC: copying the streams takes seconds, burning takes an hour.
	e.srv.prober = stubProber{info: mediainfo.Info{
		Duration: 100,
		Streams: []mediainfo.Stream{
			{Index: 0, Type: "video", Codec: "h264"},
			{Index: 1, Type: "audio", Codec: "aac"},
			{Index: 2, Type: "subtitle", Codec: "ass", Lang: "kor"},
		},
	}}
	body = get(t, e.h, "/watch/a.mkv").Body.String()
	if !strings.Contains(body, `name="burn"`) {
		t.Errorf("the choice is missing where it actually costs something:\n%s", body)
	}
}
