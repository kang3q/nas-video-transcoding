package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"nvt/ver2/internal/airplay"
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
		data.MediaURL = s.sign("/media/" + mediaPath(rel))
	}
	if v, ok := s.queue.ByRel(rel); ok {
		jv := v
		data.Job = &jv
		data.BatchID = v.BatchID
		if v.State == jobs.Running && v.Live && s.queue.LiveDirFor(v.ID) != "" {
			data.LiveURL = s.sign("/live/" + v.ID + "/index.m3u8")
		}
	}
	data.LiveOn = s.cfg.Live

	// A file that is already converted is opened to watch it, and ffprobe on
	// the source can take seconds on a NAS — the file is large, the disk is
	// spinning, and it is being read over the network. There is nothing to
	// decide here that is worth waiting for: the codecs are a caption and the
	// conversion form is folded away. So take a cached answer if there is one
	// and otherwise go straight to the player.
	info, probed := mediainfo.Info{}, false
	if data.Converted {
		info, probed = s.cachedInfo(rel)
	} else {
		info, probed = s.inspect(r.Context(), rel)
	}
	if probed {
		data.Probed = true
		data.Codecs = describe(info)
		if info.BrowserReady() {
			data.PlaysAsIs = true
			data.SourceURL = s.sign("/source/" + rel.String())
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
// cachedInfo answers from the probe cache or not at all. It never runs
// ffprobe, so a page that only needs to play something is never held up by it.
func (s *Server) cachedInfo(rel outpath.Rel) (mediainfo.Info, bool) {
	fi, err := os.Stat(s.mapper.Source(rel))
	if err != nil {
		return mediainfo.Info{}, false
	}
	return s.prober.Cached(s.mapper.Source(rel), fi)
}

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
		w.Write(s.signPlaylist(startAtZero(b), "/live/"+id+"/"))
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

// signPlaylist attaches a token to everything the playlist points at.
//
// A player is handed one URL and follows it to the segments itself, and an
// Apple TV following them has no password either. Signing only the playlist
// would authorise the table of contents and none of the video.
func (s *Server) signPlaylist(b []byte, base string) []byte {
	if s.cfg.User == "" {
		return b
	}
	var out bytes.Buffer
	for _, line := range bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n")) {
		switch {
		case len(bytes.TrimSpace(line)) == 0:
		case bytes.HasPrefix(line, []byte("#EXT-X-MAP:")):
			// The initialisation segment is named in an attribute rather than
			// on a line of its own: URI="init.mp4".
			line = signMapURI(line, func(name string) string { return s.sign(base + name) })
		case line[0] == '#':
		default:
			line = []byte(s.sign(base + string(bytes.TrimSpace(line))))
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.Bytes()
}

func signMapURI(line []byte, sign func(string) string) []byte {
	const key = `URI="`
	i := bytes.Index(line, []byte(key))
	if i < 0 {
		return line
	}
	rest := line[i+len(key):]
	j := bytes.IndexByte(rest, '"')
	if j < 0 {
		return line
	}
	var out []byte
	out = append(out, line[:i+len(key)]...)
	out = append(out, sign(string(rest[:j]))...)
	return append(out, rest[j:]...)
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

// --- AirPlay diagnostics ---

type clipView struct {
	airplay.Variant
	URL    string
	Ready  bool
	Codecs string
	Job    *jobs.View
}

type airplayData struct {
	Rel      string
	Name     string
	Crumbs   []crumb
	Clips    []clipView
	ClipSecs int
	Any      bool // at least one clip exists or is being made
}

func (s *Server) handleAirPlay(w http.ResponseWriter, r *http.Request) {
	rel, ok := s.parsePath(w, r, "/airplay/")
	if !ok {
		return
	}
	if rel.IsRoot() {
		http.Redirect(w, r, "/browse/", http.StatusFound)
		return
	}

	data := airplayData{
		Rel: rel.String(), Name: rel.Base(),
		Crumbs:   crumbs(rel.Dir().String()),
		ClipSecs: airplay.ClipSecs,
	}
	dir := airplay.Dir(rel)
	byVariant := map[string]jobs.View{}
	for _, v := range s.queue.Snapshot() {
		if v.Variant != "" && v.Rel == rel.String() {
			byVariant[v.Variant] = v
		}
	}

	for _, v := range airplay.Variants {
		c := clipView{
			Variant: v,
			URL:     s.sign("/media/" + dir + "/" + v.File()),
			Codecs:  describeVariant(v),
		}
		if _, err := os.Stat(filepath.Join(s.cfg.OutputDir, filepath.FromSlash(dir), v.File())); err == nil {
			c.Ready = true
			data.Any = true
		}
		if jv, ok := byVariant[v.Name]; ok {
			j := jv
			c.Job = &j
			data.Any = true
		}
		data.Clips = append(data.Clips, c)
	}

	s.render(w, "airplay", "에어플레이 진단", "browse", data)
}

// describeVariant is the one-line summary under each clip's heading — the
// settings themselves, so the page can be read without opening the source.
func describeVariant(v airplay.Variant) string {
	size := "원본 크기"
	if v.MaxHeight > 0 {
		size = fmt.Sprintf("%dp 이하", v.MaxHeight)
	}
	profile := v.Profile
	if profile != "" {
		profile = strings.ToUpper(profile[:1]) + profile[1:]
	}
	return fmt.Sprintf("H.264 %s@%s · %s · %s · AAC-LC %s 2ch 48kHz · faststart",
		profile, v.Level, size, v.VideoBitrate, v.AudioBitrate)
}

func (s *Server) handleAirPlayMake(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.fail(w, http.StatusBadRequest, "요청을 읽을 수 없습니다")
		return
	}
	rel, err := s.mapper.ParseRel(r.FormValue("rel"))
	if err != nil || rel.IsRoot() {
		s.fail(w, http.StatusBadRequest, "잘못된 경로입니다")
		return
	}
	to := (&url.URL{Path: "/airplay/" + rel.String()}).String()

	if _, err := s.queue.EnqueueAirPlayProbes(rel); err != nil {
		if errors.Is(err, jobs.ErrNothingToDo) {
			http.Redirect(w, r, to, http.StatusSeeOther)
			return
		}
		s.fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// handleDiscard removes a conversion so it can be made again — the subtitles
// were the wrong ones, the sync was off, the picture was worse than expected.
// Only the output is touched; the source is in a read-only mount and could not
// be reached from here even if this were wrong.
func (s *Server) handleDiscard(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.fail(w, http.StatusBadRequest, "요청을 읽을 수 없습니다")
		return
	}
	rel, err := s.mapper.ParseRel(r.FormValue("rel"))
	if err != nil || rel.IsRoot() {
		s.fail(w, http.StatusBadRequest, "잘못된 경로입니다")
		return
	}

	// Deleting the output from under a running ffmpeg would leave it writing
	// to a file nobody can find, and the rename at the end would put the old
	// name back anyway.
	if v, ok := s.queue.ByRel(rel); ok && !v.State.Terminal() {
		s.fail(w, http.StatusConflict,
			"지금 변환 중인 파일입니다. 먼저 변환을 중단한 뒤 지우세요.")
		return
	}

	if err := os.Remove(s.mapper.Output(rel)); err != nil && !os.IsNotExist(err) {
		s.fail(w, http.StatusInternalServerError, "지우지 못했습니다: "+err.Error())
		return
	}
	log.Printf("discarded conversion: %s", rel.String())

	// Back to the same page, which now offers to convert it again.
	http.Redirect(w, r, (&url.URL{Path: "/watch/" + rel.String()}).String(), http.StatusSeeOther)
}

func redirectBack(w http.ResponseWriter, r *http.Request, def string) {
	to := r.FormValue("back")
	if to == "" || !strings.HasPrefix(to, "/") || strings.HasPrefix(to, "//") {
		to = def
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}
