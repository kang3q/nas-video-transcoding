package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"nvt/ver2/internal/jobs"
	"nvt/ver2/internal/library"
	"nvt/ver2/internal/outpath"
	"nvt/ver2/internal/subs"
)

// row is one line of a directory listing: the file plus whatever we know about
// its conversion. The three facts come from three cheap lookups and are joined
// here rather than inside the library, which stays ignorant of jobs.
type row struct {
	library.Entry
	Converted bool
	Job       *jobs.View
	WatchURL  string
}

type browseData struct {
	Dir       string
	Crumbs    []crumb
	Rows      []row
	HasVideos bool
}

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	rel, ok := s.parsePath(w, r, "/browse/")
	if !ok {
		return
	}
	listing, err := s.lib.List(rel)
	if err != nil {
		s.fail(w, http.StatusNotFound, "폴더를 찾을 수 없습니다: "+rel.String())
		return
	}

	data := browseData{Dir: rel.String(), Crumbs: crumbs(rel.String())}
	for _, e := range listing.Entries {
		item := row{Entry: e}
		if e.IsVideo {
			data.HasVideos = true
			if _, err := os.Stat(s.mapper.Output(e.Rel)); err == nil {
				item.Converted = true
				item.WatchURL = "/watch/" + e.Rel.String()
			}
			if v, ok := s.queue.ByRel(e.Rel); ok && !v.State.Terminal() {
				jv := v
				item.Job = &jv
			}
		}
		data.Rows = append(data.Rows, item)
	}
	s.render(w, "browse", displayName(rel), "browse", data)
}

type watchData struct {
	Rel       string
	Name      string
	Crumbs    []crumb
	Converted bool
	MediaURL  string
	Job       *jobs.View
	BatchID   string
	Subs      []subs.Track
}

func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	rel, ok := s.parsePath(w, r, "/watch/")
	if !ok {
		return
	}
	if rel.IsRoot() {
		http.Redirect(w, r, "/browse/", http.StatusFound)
		return
	}

	data := watchData{
		Rel: rel.String(), Name: rel.Base(),
		Crumbs: crumbs(rel.Dir().String()),
	}
	if _, err := os.Stat(s.mapper.Output(rel)); err == nil {
		data.Converted = true
		data.MediaURL = "/media/" + mediaPath(rel)
	}
	if v, ok := s.queue.ByRel(rel); ok {
		jv := v
		data.Job = &jv
		data.BatchID = v.BatchID
	}
	data.Subs = s.subtitleTracks(r, rel)

	s.render(w, "watch", rel.Base(), "browse", data)
}

// subtitleTracks lists what could be burned into this file. The probe has to
// have happened for embedded tracks to show up, and probing here would block
// the page on a cold library — so it is kicked off and the picker fills in on
// the next visit.
func (s *Server) subtitleTracks(r *http.Request, rel outpath.Rel) []subs.Track {
	src := s.mapper.Source(rel)
	fi, err := os.Stat(src)
	if err != nil {
		return nil
	}
	if _, ok := s.prober.Cached(src, fi); !ok {
		go func() {
			if _, err := s.prober.Probe(context.WithoutCancel(r.Context()), src, fi); err != nil {
				log.Printf("probe %s: %v", rel.String(), err)
			}
		}()
	}
	return s.subs.Find(rel)
}

// handleConvert is the one action in the whole interface. It says which file
// was picked and how far around the directory to go; everything else follows.
func (s *Server) handleConvert(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.fail(w, http.StatusBadRequest, "요청을 읽을 수 없습니다")
		return
	}
	rel, err := s.mapper.ParseRel(r.FormValue("rel"))
	if err != nil || rel.IsRoot() {
		s.fail(w, http.StatusBadRequest, "잘못된 경로입니다")
		return
	}

	scope := library.Scope(r.FormValue("scope"))
	switch scope {
	case library.ScopeFile, library.ScopeOnwards, library.ScopeFolder:
	default:
		scope = library.ScopeFile
	}

	picked := r.FormValue("subs")
	opts := jobs.Options{
		Live:             r.FormValue("live") != "",
		Subtitles:        picked != "",
		PickedSubtitleID: picked,
	}
	if picked != "" {
		// Carry the chosen language to the rest of the batch. Episodes that
		// do not have it get no subtitles, rather than another language
		// burned in for good.
		if t, ok := subs.FindByID(s.subs.Find(rel), picked); ok {
			opts.SubtitleLang = t.Language()
		}
	}

	batch, err := s.queue.EnqueueDir(s.lib, rel, scope, opts)
	switch {
	case errors.Is(err, jobs.ErrNothingToDo):
		http.Redirect(w, r, "/browse/"+rel.Dir().String(), http.StatusSeeOther)
		return
	case errors.Is(err, jobs.ErrOutputClash):
		s.fail(w, http.StatusConflict,
			"같은 이름의 출력이 겹칩니다. 확장자만 다른 파일이 한 폴더에 있습니다.\n\n"+err.Error())
		return
	case err != nil:
		s.fail(w, http.StatusInternalServerError, err.Error())
		return
	}

	log.Printf("queued batch %s: %s (%s)", batch.ID, rel.String(), scope)
	http.Redirect(w, r, "/jobs?batch="+batch.ID, http.StatusSeeOther)
}

type jobsData struct {
	Jobs    []jobs.View
	Batch   *jobs.BatchView
	Running int
	Queued  int
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	data := jobsData{Jobs: s.queue.Snapshot()}
	for _, v := range data.Jobs {
		switch v.State {
		case jobs.Running:
			data.Running++
		case jobs.Queued:
			data.Queued++
		}
	}
	if id := r.URL.Query().Get("batch"); id != "" {
		if bv, ok := s.queue.BatchView(id); ok {
			data.Batch = &bv
		}
	}
	s.render(w, "jobs", "변환 작업", "jobs", data)
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	s.queue.Cancel(r.PathValue("id"))
	redirectBack(w, r, "/jobs")
}

func (s *Server) handleCancelBatch(w http.ResponseWriter, r *http.Request) {
	n := s.queue.CancelBatch(r.PathValue("id"))
	log.Printf("batch %s stopped, %d job(s) cancelled", r.PathValue("id"), n)
	redirectBack(w, r, "/jobs")
}

func (s *Server) handleAPIJobs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"jobs": s.queue.Snapshot()})
}

// handleEvents streams every job's progress down one connection. One stream
// per job would exhaust the browser's per-host connection limit and starve the
// video element sharing it.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Synology's reverse proxy buffers by default, which would hold the whole
	// stream until the job ended.
	w.Header().Set("X-Accel-Buffering", "no")

	events, stop := s.queue.Subscribe()
	defer stop()

	// Open with the full picture so a reconnecting client is never left
	// guessing about what it missed.
	writeSSE(w, "snapshot", map[string]any{"jobs": s.queue.Snapshot()})
	flusher.Flush()

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			writeSSE(w, ev.Kind, ev)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, event string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
}

// --- helpers ---

func (s *Server) parsePath(w http.ResponseWriter, r *http.Request, prefix string) (outpath.Rel, bool) {
	raw := strings.TrimPrefix(r.URL.Path, prefix)
	rel, err := s.mapper.ParseRel(raw)
	if err != nil {
		s.fail(w, http.StatusBadRequest, "접근할 수 없는 경로입니다")
		return outpath.Rel{}, false
	}
	return rel, true
}

// mediaPath is the output file's location under /media/, which mirrors the
// source tree with the extension replaced.
func mediaPath(rel outpath.Rel) string {
	p := rel.String()
	if ext := path.Ext(p); ext != "" {
		p = strings.TrimSuffix(p, ext)
	}
	return p + ".mp4"
}

func displayName(rel outpath.Rel) string {
	if rel.IsRoot() {
		return "라이브러리"
	}
	return rel.Base()
}

func redirectBack(w http.ResponseWriter, r *http.Request, def string) {
	to := r.FormValue("back")
	if to == "" || !strings.HasPrefix(to, "/") || strings.HasPrefix(to, "//") {
		to = def
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}
