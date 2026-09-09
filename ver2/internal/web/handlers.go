package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"nvt/ver2/internal/jobs"
	"nvt/ver2/internal/library"
	"nvt/ver2/internal/mediainfo"
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
	// PlaysAsIs suppresses the convert buttons for a file that does not need
	// converting. Browsing never probes, so this is what is already known
	// plus the reasonable assumption that a .mp4 is what it says it is; the
	// detail page probes and corrects it either way.
	PlaysAsIs bool
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
			item.PlaysAsIs = s.looksPlayable(e.Rel)
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

	// DefaultSub is the track the form starts on, empty meaning "굽지 않음".
	// Korean is chosen because it is what this library is watched with; any
	// other language is left switched off, since burning the wrong subtitles
	// cannot be undone and costs a full re-encode to discover.
	DefaultSub string

	LiveURL string
	LiveOn  bool

	// PlaysAsIs means the source is already H.264 + AAC in an MP4. There is
	// nothing to convert; it is simply played.
	PlaysAsIs bool
	SourceURL string
	Probed    bool
	Codecs    string
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
		if v.State == jobs.Running && v.Live && s.queue.LiveDirFor(v.ID) != "" {
			data.LiveURL = "/live/" + v.ID + "/index.m3u8"
		}
	}
	data.LiveOn = s.cfg.Live

	if info, ok := s.inspect(r.Context(), rel); ok {
		data.Probed = true
		data.Codecs = describe(info)
		if info.BrowserReady() {
			data.PlaysAsIs = true
			data.SourceURL = "/source/" + rel.String()
		}
	}
	data.Subs = s.subs.Find(rel)
	if t, ok := subs.PickLang(data.Subs, "kor"); ok {
		data.DefaultSub = t.ID
	}

	s.render(w, "watch", rel.Base(), "browse", data)
}

// looksPlayable answers, without running anything, whether a file probably
// needs no work. A cached probe is the truth; failing that a .mp4 is taken at
// its word, because offering to convert something that already plays is the
// more confusing mistake.
func (s *Server) looksPlayable(rel outpath.Rel) bool {
	src := s.mapper.Source(rel)
	fi, err := os.Stat(src)
	if err != nil {
		return false
	}
	if info, ok := s.prober.Cached(src, fi); ok {
		return info.BrowserReady()
	}
	return rel.Ext() == ".mp4"
}

// inspect reads what is in the file. The detail page is where this belongs:
// browsing has to stay free — probing every entry is what made v1 take half a
// minute to show a folder — but once someone has opened one file, a moment of
// ffprobe is what lets the page say anything useful about it.
func (s *Server) inspect(ctx context.Context, rel outpath.Rel) (mediainfo.Info, bool) {
	src := s.mapper.Source(rel)
	fi, err := os.Stat(src)
	if err != nil {
		return mediainfo.Info{}, false
	}
	if info, ok := s.prober.Cached(src, fi); ok {
		return info, true
	}
	info, err := s.prober.Probe(ctx, src, fi)
	if err != nil {
		log.Printf("probe %s: %v", rel.String(), err)
		return mediainfo.Info{}, false
	}
	return info, true
}

// describe is the one-line summary on the detail page, so why a file does or
// does not need converting is visible rather than implied.
func describe(info mediainfo.Info) string {
	var parts []string
	if v, ok := info.Video(); ok {
		d := strings.ToUpper(v.Codec)
		if v.Height > 0 {
			d += fmt.Sprintf(" %dp", v.Height)
		}
		parts = append(parts, d)
	}
	if a := info.Audio(); len(a) > 0 {
		d := strings.ToUpper(a[0].Codec)
		if a[0].Lang != "" {
			d += " " + strings.ToUpper(a[0].Lang)
		}
		parts = append(parts, d)
	}
	return strings.Join(parts, " · ")
}

// handleSource serves an original untouched, for the case where it already is
// what a conversion would have produced. It goes through the mapper's root, so
// a symlink inside the library cannot be used to read outside it, and
// ServeContent gives Range requests — and therefore seeking — for free.
func (s *Server) handleSource(w http.ResponseWriter, r *http.Request) {
	rel, ok := s.parsePath(w, r, "/source/")
	if !ok {
		return
	}
	if rel.IsRoot() {
		http.NotFound(w, r)
		return
	}
	f, err := s.mapper.Open(rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, rel.Base(), fi.ModTime(), f)
}

// handleLive serves the HLS rendition of a job that is still encoding.
//
// The path is /live/<job id>/<file>. The job id is ours — hex, from the queue
// — so it is checked against the queue rather than trusted, and the filename
// is confined to what the HLS muxer writes.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/live/")
	id, file, ok := strings.Cut(rest, "/")
	if !ok || !isHex(id) || !liveFile(file) {
		http.NotFound(w, r)
		return
	}
	dir := s.queue.LiveDirFor(id)
	if dir == "" {
		http.NotFound(w, r)
		return
	}

	full := filepath.Join(dir, file)

	// The playlist grows as segments appear, so it must never be cached.
	if strings.HasSuffix(file, ".m3u8") {
		b, err := os.ReadFile(full)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Write(startAtZero(b))
		return
	}

	// Segments never change once written, so they can be cached forever — but
	// only once we know there is a segment. Marking a 404 immutable would burn
	// that hole into the browser's cache for a year.
	if _, err := os.Stat(full); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, full)
}

// startAtZero pins the playhead to the beginning of the stream.
//
// While the encode runs the playlist has no #EXT-X-ENDLIST, and every player
// reads a playlist without an end marker as a live broadcast — which means
// joining at the newest segment. That is the worst possible place to sit. The
// newest segment is the one the encoder has just finished, and the encoder
// produces video at about 0.7x real time on this hardware, so the player runs
// out of media within seconds, waits, and then resumes at whatever landed in
// the meantime. Watched, it looks like the picture skipping: one, two, three,
// then eight, nine.
//
// #EXT-X-START says where to begin instead, and both hls.js and Safari's own
// player honour it ahead of their live-edge rule. Nothing is lost by starting
// at zero: the whole point of watching early is to watch from the start.
func startAtZero(b []byte) []byte {
	if bytes.Contains(b, []byte("#EXT-X-START")) {
		return b
	}
	head := []byte("#EXTM3U")
	if !bytes.HasPrefix(b, head) {
		return b // not a playlist we recognise; pass it through untouched
	}
	tag := []byte("\n#EXT-X-START:TIME-OFFSET=0,PRECISE=YES")
	out := make([]byte, 0, len(b)+len(tag))
	out = append(out, head...)
	out = append(out, tag...)
	return append(out, b[len(head):]...)
}

func isHex(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

// liveFile allows only the names the HLS muxer produces.
func liveFile(name string) bool {
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return false
	}
	switch {
	case name == "index.m3u8", name == "init.mp4":
		return true
	case strings.HasPrefix(name, "seg") && strings.HasSuffix(name, ".m4s"):
		return true
	}
	return false
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
		Live:             s.cfg.Live && r.FormValue("nolive") == "",
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
