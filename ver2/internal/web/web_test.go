package web

import (
	"context"
	"encoding/json"
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
)

type stubProber struct{ info mediainfo.Info }

func (s stubProber) Probe(context.Context, string, os.FileInfo) (mediainfo.Info, error) {
	return s.info, nil
}
func (s stubProber) Cached(string, os.FileInfo) (mediainfo.Info, bool) { return s.info, true }

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
		LiveRoot: filepath.Join(state, "live"), CheckpointAt: 0.10,
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

	cfg := &config.Config{OutputDir: out, SourceDir: src, StateDir: state}
	s, err := New(cfg, m, library.New(m), q, prober)
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
	for _, want := range []string{"ep2.mkv", "ep10.mkv", "notes.txt", "이 파일만", "폴더 전체"} {
		if !strings.Contains(body, want) {
			t.Errorf("browse page is missing %q", want)
		}
	}
	// Episode 2 before episode 10, or the queue order it implies is wrong.
	if strings.Index(body, "ep2.mkv") > strings.Index(body, "ep10.mkv") {
		t.Error("listing is not in natural order")
	}
	// Only videos get conversion buttons.
	if strings.Count(body, `name="scope"`) != 6 { // two videos, three buttons each
		t.Errorf("expected buttons for the two videos only:\n%s", body)
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
	if rec.Code != http.StatusTemporaryRedirect {
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

// The picker has to show what is actually in the file — including that a
// bitmap track can only be burned in.
func TestWatchListsSubtitleTracks(t *testing.T) {
	e := newEnv(t, false, "a.mkv")
	body := get(t, e.h, "/watch/a.mkv").Body.String()

	if !strings.Contains(body, "embedded:2") || !strings.Contains(body, "ENG") {
		t.Errorf("the English track is missing from the picker:\n%s", body)
	}
	if !strings.Contains(body, "그림 자막") {
		t.Error("a bitmap track was not marked as such")
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
