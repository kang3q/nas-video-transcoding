// Package web is the interface: browse, pick, watch it convert, play it.
//
// This is where v2 differs from v1 in the way that matters. v1 had to work out
// from a player's requests whether it was about to play something or merely
// building thumbnails, and those requests are identical. Here the click is the
// answer, so nothing has to be inferred.
//
// Everything is server-rendered with html/template and embedded, so the whole
// thing is one static binary with no build step and no CDN — the NAS may have
// no internet at all.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"fmt"
	"html/template"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"nvt/ver2/internal/config"
	"nvt/ver2/internal/jobs"
	"nvt/ver2/internal/library"
	"nvt/ver2/internal/mediainfo"
	"nvt/ver2/internal/outpath"
	"nvt/ver2/internal/subs"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Prober is the part of mediainfo the pages need. An interface so handlers can
// be tested without ffprobe installed.
type Prober interface {
	Probe(ctx context.Context, path string, fi os.FileInfo) (mediainfo.Info, error)
	Cached(path string, fi os.FileInfo) (mediainfo.Info, bool)
}

type Server struct {
	secret []byte // signs media links, so AirPlay can fetch past basic auth

	cfg    *config.Config
	mapper *outpath.Mapper
	lib    *library.Library
	queue  *jobs.Queue
	prober Prober
	subs   *subs.Finder

	pages map[string]*template.Template
}

func New(cfg *config.Config, m *outpath.Mapper, lib *library.Library, q *jobs.Queue, p Prober, f *subs.Finder) (*Server, error) {
	s := &Server{cfg: cfg, mapper: m, lib: lib, queue: q, prober: p, subs: f}
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	secret, err := loadSecret(cfg.StateDir, cfg.Secret)
	if err != nil {
		return nil, err
	}
	s.secret = secret
	return s, nil
}

// parseTemplates pairs each page with the layout separately, so every page can
// define "content" without colliding with the others.
func (s *Server) parseTemplates() error {
	s.pages = map[string]*template.Template{}
	for _, name := range []string{"browse", "converted", "jobs", "watch", "airplay", "error"} {
		t, err := template.New("layout.html").Funcs(funcs).
			ParseFS(templateFS, "templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return fmt.Errorf("template %s: %w", name, err)
		}
		s.pages[name] = t
	}
	return nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/browse/", http.StatusFound)
	})
	mux.HandleFunc("GET /browse/", s.handleBrowse)
	mux.HandleFunc("GET /converted/", s.handleConverted)
	mux.HandleFunc("GET /watch/", s.handleWatch)
	mux.HandleFunc("GET /jobs", s.handleJobs)

	mux.HandleFunc("POST /convert", s.handleConvert)
	mux.HandleFunc("POST /discard", s.handleDiscard)
	mux.HandleFunc("GET /airplay/", s.handleAirPlay)
	mux.HandleFunc("POST /airplay", s.handleAirPlayMake)
	mux.HandleFunc("POST /jobs/{id}/cancel", s.handleCancelJob)
	mux.HandleFunc("POST /batches/{id}/cancel", s.handleCancelBatch)

	mux.HandleFunc("GET /api/jobs", s.handleAPIJobs)
	mux.HandleFunc("GET /api/events", s.handleEvents)

	// Finished output only. A .part has no index and cannot be played; a job
	// still running is watched over HLS instead, below.
	mux.Handle("GET /media/", http.StripPrefix("/media/",
		http.FileServer(http.Dir(s.cfg.OutputDir))))
	mux.HandleFunc("GET /live/", s.handleLive)
	// A source that is already H.264 + AAC in an MP4 needs no conversion at
	// all; it is played where it lies.
	mux.HandleFunc("GET /source/", s.handleSource)
	mux.Handle("GET /static/", http.FileServerFS(staticFS))

	// The icon is an SVG, which every browser here prefers when the page
	// links to one. Some clients ask for /favicon.ico anyway, before the page
	// is even parsed — behind a password that is an auth challenge and a 404
	// on every single page load, which buries the log. Answering "there is
	// nothing here, stop asking" costs one line.
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	// Logging goes outside authentication, not inside it. With the order the
	// other way a rejected request never reached the log, so a client being
	// turned away looked exactly like a client that never arrived — and those
	// two have completely different causes.
	return s.logRequests(s.basicAuth(mux))
}

// --- middleware ---

func (s *Server) basicAuth(next http.Handler) http.Handler {
	if s.cfg.User == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A television has nowhere to type a password. A link the browser
		// minted after logging in carries its own permission instead, good
		// for that one path and only for a day.
		if mediaPathPrefix(r.URL.Path) && s.signedOK(r.URL.Path, r.URL.Query().Get(signParam)) {
			next.ServeHTTP(w, r)
			return
		}
		u, p, ok := r.BasicAuth()
		okUser := subtle.ConstantTimeCompare([]byte(u), []byte(s.cfg.User)) == 1
		okPass := subtle.ConstantTimeCompare([]byte(p), []byte(s.cfg.Pass)) == 1
		if !ok || !okUser || !okPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="nvt"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	if !s.cfg.LogRequests {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Static assets and the event stream are constant and say nothing.
		quiet := strings.HasPrefix(r.URL.Path, "/static/") ||
			strings.HasPrefix(r.URL.Path, "/api/events") ||
			r.URL.Path == "/favicon.ico"
		if quiet {
			next.ServeHTTP(w, r)
			return
		}

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// The status and the caller are the whole point when something is
		// fetching a file and failing: a 401 means it was turned away, and no
		// line at all means it never got here. Media requests carry who asked,
		// because AirPlay is the Apple TV fetching the file itself — it
		// arrives as a different client than the browser that started it.
		if mediaPathPrefix(r.URL.Path) {
			log.Printf("%s %s -> %d  from %s  %s",
				r.Method, r.URL.Path, rec.status, host(r.RemoteAddr), agent(r))
			return
		}
		log.Printf("%s %s -> %d", r.Method, r.URL.Path, rec.status)
	})
}

// mediaPathPrefix reports whether a path serves bytes of video, which are the
// requests worth knowing the caller of.
func mediaPathPrefix(p string) bool {
	return strings.HasPrefix(p, "/media/") ||
		strings.HasPrefix(p, "/source/") ||
		strings.HasPrefix(p, "/live/")
}

func host(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// agent keeps enough of the User-Agent to tell an Apple TV fetching a file
// ("AppleCoreMedia/...") from the browser that asked it to.
func agent(r *http.Request) string {
	ua := r.UserAgent()
	if ua == "" {
		return "(no user-agent)"
	}
	if i := strings.IndexByte(ua, ' '); i > 0 {
		ua = ua[:i]
	}
	if len(ua) > 40 {
		ua = ua[:40]
	}
	return ua
}

// statusRecorder remembers what was actually sent. It has to pass Flush
// through: the event stream depends on it, and a wrapper that swallowed it
// would leave the progress bars frozen.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// --- shared rendering ---

type pageData struct {
	Title   string
	Nav     string
	Content any
}

func (s *Server) render(w http.ResponseWriter, page, title, nav string, content any) {
	t, ok := s.pages[page]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.Execute(w, pageData{Title: title, Nav: nav, Content: content}); err != nil {
		log.Printf("render %s: %v", page, err)
	}
}

func (s *Server) fail(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	s.render(w, "error", "오류", "", msg)
}

// --- template helpers ---

var funcs = template.FuncMap{
	"pct": func(f float64) string {
		if f < 0 {
			return "—"
		}
		return fmt.Sprintf("%.0f%%", f*100)
	},
	"pctwidth": func(f float64) template.CSS {
		if f < 0 {
			return template.CSS("0%")
		}
		return template.CSS(fmt.Sprintf("%.1f%%", math.Min(f, 1)*100))
	},
	"dur":  humanDuration,
	"size": humanSize,
	"rate": func(f float64) string {
		if f <= 0 {
			return "—"
		}
		return fmt.Sprintf("%.2fx", f)
	},
	"pathup": func(p string) string {
		if p == "" {
			return ""
		}
		if i := strings.LastIndex(p, "/"); i >= 0 {
			return p[:i]
		}
		return ""
	},
	"crumbs": crumbs,
}

type crumb struct {
	Name string
	Path string
	// Href is the whole link. The library's own pages build theirs from Path
	// because they all sit under /browse/; the converted tree needs its own
	// prefix, and encoding the path is not optional once Korean and spaces
	// are in it.
	Href string
}

// crumbsFor builds a trail under some other prefix, with its own name for the
// root.
func crumbsFor(base, rootName, p string) []crumb {
	out := crumbs(p)
	out[0].Name = rootName
	for i := range out {
		out[i].Href = (&url.URL{Path: base + out[i].Path}).String()
	}
	return out
}

func crumbs(p string) []crumb {
	out := []crumb{{Name: "라이브러리", Path: ""}}
	if p == "" {
		return out
	}
	var acc string
	for _, el := range strings.Split(p, "/") {
		if acc == "" {
			acc = el
		} else {
			acc += "/" + el
		}
		out = append(out, crumb{Name: el, Path: acc})
	}
	return out
}

func humanDuration(sec float64) string {
	if sec < 0 || math.IsNaN(sec) || math.IsInf(sec, 0) {
		return "—"
	}
	d := time.Duration(sec * float64(time.Second))
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d초", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d분 %d초", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%d시간 %d분", int(d.Hours()), int(d.Minutes())%60)
	}
}

func humanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
