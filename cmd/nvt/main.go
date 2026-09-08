// Command nvt serves a media directory over WebDAV, transparently converting
// files whose codecs the player cannot handle.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/webdav"

	"nvt/internal/cache"
	"nvt/internal/config"
	"nvt/internal/probe"
	"nvt/internal/transcode"
	"nvt/internal/vfs"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("nvt ")

	cfg := config.Load()

	if fi, err := os.Stat(cfg.MediaDir); err != nil || !fi.IsDir() {
		log.Fatalf("media dir %q is not readable: %v", cfg.MediaDir, err)
	}

	c, err := cache.New(cfg.CacheDir)
	if err != nil {
		log.Fatalf("cache dir %q: %v", cfg.CacheDir, err)
	}
	p, err := probe.New(cfg)
	if err != nil {
		log.Fatalf("probe init: %v", err)
	}
	tm := transcode.NewManager(cfg, c)
	fsys := vfs.New(cfg, p, c, tm)

	dav := &webdav.Handler{
		FileSystem: fsys,
		LockSystem: webdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("dav %s %s: %v", r.Method, r.URL.Path, err)
			}
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/__nvt/status", statusHandler(c, tm, cfg))
	mux.HandleFunc("/__nvt/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.Handle("/", readOnly(withMethod(serveMedia(cfg, fsys, quiet(dav)))))

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           basicAuth(cfg, mux),
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: responses are whole movies.
	}

	log.Printf("media=%s cache=%s listen=%s", cfg.MediaDir, cfg.CacheDir, cfg.Listen)
	log.Printf("playable video: %s", strings.Join(cfg.VideoOK, ","))
	log.Printf("playable audio: %s", strings.Join(cfg.AudioOK, ","))
	log.Printf("convert to: %s %s, jobs=%d, wait=%v",
		cfg.AudioCodec, cfg.AudioBitrate, cfg.TranscodeJobs, cfg.WaitForComplete)
	log.Printf("prefetch=%v max=%d video=%v log_requests=%v",
		cfg.PrefetchOnList, cfg.PrefetchMax, cfg.PrefetchVideo, cfg.LogRequests)

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Print("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	p.Flush()
}

// readOnly rejects anything that would modify the share. The source tree is
// mounted read-only anyway; this makes the refusal explicit and cheap.
func readOnly(next http.Handler) http.Handler {
	allowed := map[string]bool{
		"GET": true, "HEAD": true, "OPTIONS": true, "PROPFIND": true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[r.Method] {
			w.Header().Set("Allow", "OPTIONS, GET, HEAD, PROPFIND")
			http.Error(w, "read-only server", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// serveMedia answers GET and HEAD itself instead of letting webdav do it.
//
// webdav serves content through http.ServeContent, which has to know the total
// size before it writes anything. For a conversion that is still running that
// size is a guess, and a guess is not good enough: too low truncates playback
// partway through, too high leaves the player waiting for bytes that never
// come. Since FLAC output is larger than the source it replaces, the obvious
// guess is always too low.
//
// So an unfinished conversion is streamed with no Content-Length at all. That
// costs seeking until the job finishes, which is the honest trade; every
// finished file still goes through ServeContent and seeks normally.
func serveMedia(cfg *config.Config, fsys *vfs.FS, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		if cfg.LogRequests {
			if rng := r.Header.Get("Range"); rng != "" {
				log.Printf("%s %s range=%s", r.Method, r.URL.Path, rng)
			} else {
				log.Printf("%s %s", r.Method, r.URL.Path)
			}
		}

		f, err := fsys.OpenFile(r.Context(), r.URL.Path, os.O_RDONLY, 0)
		if err != nil {
			next.ServeHTTP(w, r) // let webdav decide the status
			return
		}
		defer f.Close()

		fi, err := f.Stat()
		if err != nil || fi.IsDir() {
			next.ServeHTTP(w, r)
			return
		}

		if inc, ok := f.(vfs.Incomplete); ok && inc.Incomplete() {
			streamUnsized(w, r, fi, f)
			return
		}
		http.ServeContent(w, r, path.Base(r.URL.Path), fi.ModTime(), f)
	})
}

// streamUnsized sends a file of unknown length, flushing as it goes so the
// player starts on the first chunk rather than on a full buffer.
func streamUnsized(w http.ResponseWriter, r *http.Request, fi fs.FileInfo, src io.Reader) {
	if ct, ok := fi.(webdav.ContentTyper); ok {
		if t, err := ct.ContentType(r.Context()); err == nil {
			w.Header().Set("Content-Type", t)
		}
	}
	// Range requests cannot be honoured without a known size; say so rather
	// than answering one wrongly.
	w.Header().Set("Accept-Ranges", "none")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}

	flush, _ := w.(http.Flusher)
	buf := make([]byte, 128<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // client went away
			}
			if flush != nil {
				flush.Flush()
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("stream %s: %v", r.URL.Path, err)
			}
			return
		}
	}
}

// withMethod passes the request method down to the filesystem, which needs it
// to distinguish a directory listing from an actual playback request.
func withMethod(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(vfs.WithMethod(r.Context(), r.Method)))
	})
}

// quiet suppresses the "superfluous response.WriteHeader" warning that
// x/net/webdav provokes on every abandoned request: handlePropfind reports a
// late write error by returning a status, but by then the multistatus body has
// already gone out. Players walk away from PROPFINDs constantly while scanning
// a library, so the warning is pure noise. The underlying error is still
// reported through the handler's Logger.
func quiet(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&quietWriter{ResponseWriter: w}, r)
	})
}

type quietWriter struct {
	http.ResponseWriter
	wrote bool
}

func (q *quietWriter) WriteHeader(code int) {
	if q.wrote {
		return
	}
	q.wrote = true
	q.ResponseWriter.WriteHeader(code)
}

func (q *quietWriter) Write(b []byte) (int, error) {
	q.wrote = true
	return q.ResponseWriter.Write(b)
}

// ReadFrom keeps the sendfile fast path that http.ServeContent relies on;
// wrapping the writer would otherwise hide it.
func (q *quietWriter) ReadFrom(r io.Reader) (int64, error) {
	q.wrote = true
	if rf, ok := q.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(q.ResponseWriter, r)
}

func (q *quietWriter) Flush() {
	if f, ok := q.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func basicAuth(cfg *config.Config, next http.Handler) http.Handler {
	if cfg.User == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, pw, ok := r.BasicAuth()
		okUser := subtle.ConstantTimeCompare([]byte(u), []byte(cfg.User)) == 1
		okPass := subtle.ConstantTimeCompare([]byte(pw), []byte(cfg.Pass)) == 1
		if !ok || !okUser || !okPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="nvt"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func statusHandler(c *cache.Cache, tm *transcode.Manager, cfg *config.Config) http.HandlerFunc {
	type job struct {
		Source   string `json:"source"`
		Action   string `json:"action"`
		Reason   string `json:"reason"`
		Priority string `json:"priority"`
		State    string `json:"state"`
		Elapsed  string `json:"elapsed,omitempty"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		jobs := []job{}
		for _, s := range tm.Active() {
			j := job{
				Source:   s.Src,
				Action:   s.Action,
				Reason:   s.Reason,
				Priority: s.Priority,
				State:    "queued",
			}
			if s.Running {
				j.State = "running"
				j.Elapsed = s.Elapsed.String()
			}
			jobs = append(jobs, j)
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(map[string]any{
			"cache_bytes":     c.Total(),
			"cache_max_bytes": cfg.CacheMaxBytes,
			"active_jobs":     jobs,
		})
	}
}
