package web

import (
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"nvt/ver2/internal/library"
	"nvt/ver2/internal/outpath"
)

// The playable index answers "what can I watch, and where is it?" for the
// whole library at once.
//
// Building it walks every directory of both trees, which on the hardware this
// runs on takes about ten seconds the first time — the disk is spinning and
// the library is large. Nothing is opened; that would take minutes, and it is
// what made v1 need half a minute to show a single folder.
//
// Ten seconds is fine once and intolerable per page, so the result is kept.
// It is thrown away when a conversion finishes, because that is the one event
// that changes the answer, and after a while regardless, because files arrive
// on the NAS by other means than this program.
const playableTTL = 10 * time.Minute

type playableRow struct {
	Name string
	Href string
	Size int64
	// Converted separates the two ways a file gets here: we made it, or it
	// never needed making. Worth showing, because the second kind can still
	// be worth converting — to burn subtitles in.
	Converted bool
	// Guessed marks a file judged by its extension because nothing has ever
	// looked inside it. Some of those .mp4 files hold HEVC and will not play.
	Guessed bool
}

type playableDir struct {
	Name  string
	Href  string
	Count int // playable files in this branch, at any depth
}

type playableIndex struct {
	builtAt time.Time
	gen     uint64

	files   map[string][]playableRow // directory -> what is directly in it
	subdirs map[string][]playableDir // directory -> branches worth entering

	total    int
	unprobed int
}

// index returns the current index, building it if there is not a usable one.
func (s *Server) index() *playableIndex {
	gen := s.queue.Finished()

	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	if ix := s.playable; ix != nil && ix.gen == gen && time.Since(ix.builtAt) < playableTTL {
		return ix
	}
	ix := s.buildIndex(gen)
	s.playable = ix
	return ix
}

// forgetIndex drops the cached answer, for when the caller knows it is stale.
func (s *Server) forgetIndex() {
	s.indexMu.Lock()
	s.playable = nil
	s.indexMu.Unlock()
}

func (s *Server) buildIndex(gen uint64) *playableIndex {
	ix := &playableIndex{
		builtAt: time.Now(), gen: gen,
		files:   map[string][]playableRow{},
		subdirs: map[string][]playableDir{},
	}
	root := outpath.Rel{}

	// Converted output first, keyed by the source it came from, so a file
	// that was converted does not also appear as its unconverted self.
	converted := map[string]library.Entry{}
	for _, e := range s.lib.WalkOutput(root) {
		converted[library.TrimExt(e.Rel.String())] = e
	}

	seen := map[string]bool{}
	for _, e := range s.lib.WalkVideos(root) {
		stem := library.TrimExt(e.Rel.String())
		out, isConverted := converted[stem]
		if !isConverted && !s.playableEntry(e) {
			continue
		}
		seen[stem] = true

		row := playableRow{
			Name: e.Name, Size: e.Size, Converted: isConverted,
			Href: (&url.URL{Path: "/watch/" + e.Rel.String()}).String(),
		}
		if isConverted {
			row.Size = out.Size
		} else if !s.probed(e) {
			row.Guessed = true
			ix.unprobed++
		}
		ix.add(e.Rel.Dir().String(), row)
	}

	// A conversion whose source has been deleted or renamed still plays, and
	// this listing is the only place it is visible at all.
	for stem, e := range converted {
		if seen[stem] {
			continue
		}
		ix.add(e.Rel.Dir().String(), playableRow{
			Name: e.Name, Size: e.Size, Converted: true,
			Href: s.sign("/media/" + e.Rel.String()),
		})
	}

	ix.finish()
	return ix
}

// add files a row under its directory and makes every ancestor aware that
// this branch leads somewhere. Without the second part the tree would be full
// of folders that turn out to be dead ends.
func (ix *playableIndex) add(dir string, row playableRow) {
	ix.files[dir] = append(ix.files[dir], row)
	ix.total++

	segs := []string{}
	if dir != "" {
		segs = strings.Split(dir, "/")
	}
	for i := range segs {
		parent := strings.Join(segs[:i], "/")
		child := strings.Join(segs[:i+1], "/")
		ix.bump(parent, child, segs[i])
	}
}

func (ix *playableIndex) bump(parent, childPath, childName string) {
	list := ix.subdirs[parent]
	for i := range list {
		if list[i].Name == childName {
			list[i].Count++
			ix.subdirs[parent] = list
			return
		}
	}
	ix.subdirs[parent] = append(list, playableDir{
		Name:  childName,
		Href:  (&url.URL{Path: "/playable/" + childPath}).String(),
		Count: 1,
	})
}

func (ix *playableIndex) finish() {
	for dir, rows := range ix.files {
		sort.Slice(rows, func(i, j int) bool { return library.NaturalLess(rows[i].Name, rows[j].Name) })
		ix.files[dir] = rows
	}
	for dir, subs := range ix.subdirs {
		sort.Slice(subs, func(i, j int) bool { return library.NaturalLess(subs[i].Name, subs[j].Name) })
		ix.subdirs[dir] = subs
	}
}

// playableEntry answers "can this be watched as it is?" without opening it.
func (s *Server) playableEntry(e library.Entry) bool {
	if info, ok := s.prober.CachedAt(s.mapper.Source(e.Rel), e.Size, e.Mod.Unix()); ok {
		return info.BrowserReady()
	}
	// Never looked at. A .mp4 is taken at its word, which is right often
	// enough to be useful and wrong often enough to be counted and said.
	return e.Rel.Ext() == ".mp4"
}

func (s *Server) probed(e library.Entry) bool {
	_, ok := s.prober.CachedAt(s.mapper.Source(e.Rel), e.Size, e.Mod.Unix())
	return ok
}

// --- the page ---

type playableData struct {
	Dir     string
	Crumbs  []crumb
	Subdirs []playableDir
	Rows    []playableRow

	// SourceDirHref is the same folder in the library, for going the other
	// way once something here turns out to need converting after all.
	SourceDirHref string

	Total    int // in this branch
	Overall  int // in the whole library
	Unprobed int
	Stale    time.Duration // how old the answer is
}

func (s *Server) handlePlayable(w http.ResponseWriter, r *http.Request) {
	rel, ok := s.parsePath(w, r, "/playable/")
	if !ok {
		return
	}
	if r.URL.Query().Get("refresh") != "" {
		s.forgetIndex()
		http.Redirect(w, r, (&url.URL{Path: "/playable/" + rel.String()}).String(), http.StatusSeeOther)
		return
	}

	ix := s.index()
	dir := rel.String()

	data := playableData{
		Dir:           dir,
		Crumbs:        crumbsFor("/playable/", "재생 가능", dir),
		Subdirs:       ix.subdirs[dir],
		Rows:          ix.files[dir],
		SourceDirHref: (&url.URL{Path: "/browse/" + dir}).String(),
		Overall:       ix.total,
		Unprobed:      ix.unprobed,
		Stale:         time.Since(ix.builtAt).Round(time.Second),
	}
	data.Total = len(data.Rows)
	for _, sub := range data.Subdirs {
		data.Total += sub.Count
	}

	title := "재생 가능"
	if dir != "" {
		title = path.Base(dir)
	}
	s.render(w, "playable", title, "playable", data)
}
