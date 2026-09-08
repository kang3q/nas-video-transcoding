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
	"net/http"
	"os"
	"strings"
	"time"

	"nvt/ver2/internal/config"
	"nvt/ver2/internal/jobs"
	"nvt/ver2/internal/library"
	"nvt/ver2/internal/mediainfo"
	"nvt/ver2/internal/outpath"
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
	cfg    *config.Config
	mapper *outpath.Mapper
	lib    *library.Library
	queue  *jobs.Queue
	prober Prober

	pages map[string]*template.Template
}

func New(cfg *config.Config, m *outpath.Mapper, lib *library.Library, q *jobs.Queue, p Prober) (*Server, error) {
	s := &Server{cfg: cfg, mapper: m, lib: lib, queue: q, prober: p}
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	return s, nil
}

// parseTemplates pairs each page with the layout separately, so every page can
// define "content" without colliding with the others.
func (s *Server) parseTemplates() error {
	s.pages = map[string]*template.Template{}
	for _, name := range []string{"browse", "jobs", "watch", "error"} {
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
	mux.HandleFunc("GET /watch/", s.handleWatch)
	mux.HandleFunc("GET /jobs", s.handleJobs)

	mux.HandleFunc("POST /convert", s.handleConvert)
	mux.HandleFunc("POST /jobs/{id}/cancel", s.handleCancelJob)
	mux.HandleFunc("POST /batches/{id}/cancel", s.handleCancelBatch)

	mux.HandleFunc("GET /api/jobs", s.handleAPIJobs)
	mux.HandleFunc("GET /api/events", s.handleEvents)

	// Finished output only. A .part has no index and cannot be played; the
	// live path for an unfinished job is HLS, served separately.
	mux.Handle("GET /media/", http.StripPrefix("/media/",
		http.FileServer(http.Dir(s.cfg.OutputDir))))
	mux.Handle("GET /static/", http.FileServerFS(staticFS))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	return s.basicAuth(s.logRequests(mux))
}

// --- middleware ---

func (s *Server) basicAuth(next http.Handler) http.Handler {
	if s.cfg.User == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		// Media and event traffic is constant and says nothing useful.
		if !strings.HasPrefix(r.URL.Path, "/static/") &&
			!strings.HasPrefix(r.URL.Path, "/api/events") {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
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
