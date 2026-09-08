// Command nvt serves a media directory over WebDAV, transparently converting
// files whose codecs the player cannot handle.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
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
	mux.Handle("/", readOnly(dav))

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
	log.Printf("convert to: %s %s, jobs=%d, prefetch=%v, wait=%v",
		cfg.AudioCodec, cfg.AudioBitrate, cfg.TranscodeJobs, cfg.PrefetchOnList, cfg.WaitForComplete)

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
		Source  string `json:"source"`
		Action  string `json:"action"`
		Reason  string `json:"reason"`
		Elapsed string `json:"elapsed"`
		Bytes   int64  `json:"bytes_written"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		jobs := []job{}
		for _, j := range tm.Active() {
			if j.Finished() {
				continue
			}
			jobs = append(jobs, job{
				Source:  j.Src,
				Action:  string(j.Plan.Action),
				Reason:  j.Plan.Reason,
				Elapsed: time.Since(j.Started).Round(time.Second).String(),
				Bytes:   c.Size(j.Key),
			})
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
