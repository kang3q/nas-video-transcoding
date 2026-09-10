package web

import (
	"context"
	"encoding/json"
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

	"nvt/ver2/internal/airplay"
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

// AirPlay says nothing about why a file will not play. Four short clips that
// differ in one property each turn that into something answerable, so the page
// has to show all four at once, each with its own player.
func TestAirPlayPageOffersEveryVariant(t *testing.T) {
	e := newEnv(t, false, "a.mkv")

	body := get(t, e.h, "/airplay/a.mkv").Body.String()
	for _, v := range airplay.Variants {
		if !strings.Contains(body, v.Title) {
			t.Errorf("%q is missing from the page:\n%s", v.Name, body)
		}
	}
	if strings.Contains(body, "<video") {
		t.Error("a player was offered for a clip that has not been made")
	}

	// Once the clips exist, each gets a player pointed at its own file.
	dir := filepath.Join(e.out, filepath.FromSlash(airplay.Dir(e.rel(t, "a.mkv"))))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, v := range airplay.Variants {
		if err := os.WriteFile(filepath.Join(dir, v.File()), []byte("clip"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	body = get(t, e.h, "/airplay/a.mkv").Body.String()
	if n := strings.Count(body, "<video"); n != len(airplay.Variants) {
		t.Errorf("got %d players, want %d:\n%s", n, len(airplay.Variants), body)
	}
	for _, v := range airplay.Variants {
		if !strings.Contains(body, v.File()) {
			t.Errorf("no player points at %s", v.File())
		}
	}
}

// The clips are ordinary jobs, so they queue behind a real conversion rather
// than competing with it for the one encoder this hardware has.
func TestAirPlayProbesGoThroughTheQueue(t *testing.T) {
	e := newEnv(t, true, "a.mkv")

	rec := post(t, e.h, "/airplay", url.Values{"rel": {"a.mkv"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}

	var names []string
	for _, v := range e.srv.queue.Snapshot() {
		if v.Variant != "" {
			names = append(names, v.Variant)
		}
	}
	if len(names) != len(airplay.Variants) {
		t.Fatalf("queued %v, want one job per variant", names)
	}

	// The file's real conversion must still be reachable: the diagnostics
	// write elsewhere and must not be mistaken for it.
	if _, ok := e.srv.queue.ByRel(e.rel(t, "a.mkv")); ok {
		t.Error("a diagnostic clip was taken for the file's own conversion")
	}
}

// The clips are made to answer a question being asked now. Keeping them across
// a restart would restart an experiment nobody is waiting on.
func TestAirPlayProbesAreNotPersisted(t *testing.T) {
	e := newEnv(t, true, "a.mkv")
	post(t, e.h, "/airplay", url.Values{"rel": {"a.mkv"}})

	waitFor(t, "a probe to start", func() bool {
		for _, v := range e.srv.queue.Snapshot() {
			if v.Variant != "" && v.State == jobs.Running {
				return true
			}
		}
		return false
	})
	if _, err := os.Stat(filepath.Join(e.srv.cfg.StateDir, "queue.json")); err == nil {
		t.Error("a diagnostic batch was written to the saved queue")
	}
}

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
