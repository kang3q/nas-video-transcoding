// Package vfs presents the media tree to a WebDAV client, substituting a
// converted file wherever the original would not play.
//
// The mapping is name-level: "Movie.avi" with an unsupported audio track is
// advertised as "Movie.mkv". Browsing therefore looks exactly like browsing
// the real share, and the player never learns that a conversion happened.
package vfs

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"

	"nvt/internal/cache"
	"nvt/internal/config"
	"nvt/internal/probe"
	"nvt/internal/transcode"
)

var errReadOnly = errors.New("nvt: read-only filesystem")

type methodKey struct{}

// WithMethod records the HTTP method so the filesystem can tell a listing from
// a playback request.
//
// This is not a convenience. x/net/webdav resolves properties by calling
// OpenFile on every file it lists, so OpenFile alone says nothing about intent
// — treating it as "the viewer pressed play" would start a conversion for
// every file in every directory a player walks, and block the PROPFIND while
// each one ran.
func WithMethod(ctx context.Context, method string) context.Context {
	return context.WithValue(ctx, methodKey{}, method)
}

// isPlayback reports whether the request wants content rather than metadata.
// HEAD does not qualify: it asks what a file is, not for the file, and a
// player issuing one must not set a conversion running.
func isPlayback(ctx context.Context) bool {
	m, _ := ctx.Value(methodKey{}).(string)
	return m == "GET"
}

// mediaExt lists containers worth probing. Anything else (subtitles, artwork,
// .nfo) is passed through untouched so external subtitle files keep working.
var mediaExt = map[string]bool{
	".avi": true, ".mkv": true, ".mp4": true, ".m4v": true, ".mov": true,
	".ts": true, ".m2ts": true, ".mts": true, ".mpg": true, ".mpeg": true,
	".wmv": true, ".asf": true, ".flv": true, ".vob": true, ".rmvb": true,
	".rm": true, ".divx": true, ".ogm": true, ".webm": true, ".3gp": true,
	".m2v": true, ".dat": true,
}

type entry struct {
	virtName string
	realPath string
	isDir    bool
	fi       os.FileInfo
	plan     probe.Plan
}

type dirSnapshot struct {
	at      time.Time
	entries []entry
}

type FS struct {
	cfg    *config.Config
	prober *probe.Prober
	cache  *cache.Cache
	tm     *transcode.Manager

	mu   sync.Mutex
	dirs map[string]*dirSnapshot
}

func New(cfg *config.Config, p *probe.Prober, c *cache.Cache, tm *transcode.Manager) *FS {
	return &FS{cfg: cfg, prober: p, cache: c, tm: tm, dirs: map[string]*dirSnapshot{}}
}

// --- webdav.FileSystem (read-only) ---

func (f *FS) Mkdir(ctx context.Context, name string, perm os.FileMode) error { return errReadOnly }
func (f *FS) RemoveAll(ctx context.Context, name string) error               { return errReadOnly }
func (f *FS) Rename(ctx context.Context, oldName, newName string) error      { return errReadOnly }

func (f *FS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	virt, err := cleanVirt(name)
	if err != nil {
		return nil, err
	}
	if virt == "/" {
		fi, err := os.Stat(f.cfg.MediaDir)
		if err != nil {
			return nil, err
		}
		return &info{name: "/", size: 0, mode: fi.Mode(), mod: fi.ModTime(), dir: true}, nil
	}

	e, err := f.lookup(ctx, virt)
	if err != nil {
		return nil, err
	}
	return f.infoFor(e), nil
}

func (f *FS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_APPEND|os.O_TRUNC) != 0 {
		return nil, errReadOnly
	}
	virt, err := cleanVirt(name)
	if err != nil {
		return nil, err
	}

	if virt == "/" {
		return f.openDir(ctx, "/")
	}
	e, err := f.lookup(ctx, virt)
	if err != nil {
		return nil, err
	}
	if e.isDir {
		return f.openDir(ctx, virt)
	}
	if !isPlayback(ctx) {
		// A listing, or a HEAD, only wants metadata. Handing back real content
		// here would open a file per entry, or worse, start converting one.
		return &metaFile{fi: f.infoFor(e), complete: f.settled(e)}, nil
	}
	if !e.plan.NeedsWork() {
		return f.openReal(e)
	}
	return f.openConverted(ctx, virt, e)
}

// --- listing ---

func (f *FS) openDir(ctx context.Context, virt string) (*dirFile, error) {
	ents, err := f.list(ctx, virt)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(f.realPath(virt))
	if err != nil {
		return nil, err
	}
	infos := make([]fs.FileInfo, 0, len(ents))
	for i := range ents {
		infos = append(infos, f.infoFor(ents[i]))
	}
	return &dirFile{
		self:    &info{name: path.Base(virt), mode: fi.Mode(), mod: fi.ModTime(), dir: true},
		entries: infos,
	}, nil
}

// list resolves one directory: it probes every media file in it (in parallel,
// bounded by the prober) and works out the name each one should be shown under.
func (f *FS) list(ctx context.Context, virt string) ([]entry, error) {
	real := f.realPath(virt)
	dirFI, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	if !dirFI.IsDir() {
		return nil, os.ErrInvalid
	}

	f.mu.Lock()
	if snap, ok := f.dirs[virt]; ok && time.Since(snap.at) < 30*time.Second {
		f.mu.Unlock()
		return snap.entries, nil
	}
	f.mu.Unlock()

	osEnts, err := os.ReadDir(real)
	if err != nil {
		return nil, err
	}

	ents := make([]entry, 0, len(osEnts))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, de := range osEnts {
		if strings.HasPrefix(de.Name(), ".") {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		rp := filepath.Join(real, de.Name())

		if de.IsDir() {
			mu.Lock()
			ents = append(ents, entry{virtName: de.Name(), realPath: rp, isDir: true, fi: fi})
			mu.Unlock()
			continue
		}
		if !fi.Mode().IsRegular() {
			continue
		}
		if !mediaExt[strings.ToLower(filepath.Ext(de.Name()))] {
			mu.Lock()
			ents = append(ents, entry{virtName: de.Name(), realPath: rp, fi: fi,
				plan: probe.Plan{Action: probe.Passthrough, Reason: "not a media file"}})
			mu.Unlock()
			continue
		}

		wg.Add(1)
		go func(name, rp string, fi os.FileInfo) {
			defer wg.Done()
			pl, err := f.prober.Plan(ctx, rp, fi)
			if err != nil {
				// An unreadable or damaged file is still listed, unchanged;
				// hiding it would be worse than letting the player try.
				log.Printf("probe failed, passing through: %s: %v", rp, err)
				pl = probe.Plan{Action: probe.Passthrough, Reason: "probe failed"}
			}
			mu.Lock()
			ents = append(ents, entry{virtName: name, realPath: rp, fi: fi, plan: pl})
			mu.Unlock()
		}(de.Name(), rp, fi)
	}
	wg.Wait()

	assignNames(ents)
	// Stable order matters: the next-file prefetch after playback walks this
	// list, and probing finishes in nondeterministic order.
	sort.Slice(ents, func(i, j int) bool {
		if ents[i].isDir != ents[j].isDir {
			return ents[i].isDir
		}
		return strings.ToLower(ents[i].virtName) < strings.ToLower(ents[j].virtName)
	})

	f.mu.Lock()
	f.dirs[virt] = &dirSnapshot{at: time.Now(), entries: ents}
	f.mu.Unlock()

	if f.cfg.PrefetchOnList {
		go f.prefetch(virt, ents)
	}
	return ents, nil
}

// assignNames rewrites the extension of anything that will be converted, and
// resolves collisions with a file that already owns the target name.
func assignNames(ents []entry) {
	taken := map[string]bool{}
	for _, e := range ents {
		taken[strings.ToLower(e.virtName)] = true
	}
	for i := range ents {
		e := &ents[i]
		if e.isDir || !e.plan.NeedsWork() {
			continue
		}
		base := strings.TrimSuffix(e.virtName, filepath.Ext(e.virtName))
		cand := base + ".mkv"
		if strings.EqualFold(cand, e.virtName) {
			continue // already .mkv, name stays put
		}
		if taken[strings.ToLower(cand)] {
			cand = base + " (nvt).mkv"
		}
		delete(taken, strings.ToLower(e.virtName))
		taken[strings.ToLower(cand)] = true
		e.virtName = cand
	}
}

// prefetch warms the cache for a browsed directory so that pressing play
// usually lands on a finished file.
//
// It is deliberately timid. A player building its library walks the entire
// share, so this runs for every directory that exists; converting all of it
// would keep the NAS busy for hours on files nobody asked for.
func (f *FS) prefetch(dir string, ents []entry) {
	// Browsing elsewhere means earlier guesses are stale.
	f.tm.DropPendingOutside(dir)

	queued := 0
	for _, e := range ents {
		if queued >= f.cfg.PrefetchMax {
			break
		}
		if e.isDir || !e.plan.NeedsWork() {
			continue
		}
		// Video re-encoding is far too expensive to start on a guess.
		if e.plan.Action == probe.FullTranscode && !f.cfg.PrefetchVideo {
			continue
		}
		key := f.key(e)
		if f.cache.Complete(key) {
			continue
		}
		if f.tm.Start(key, e.realPath, dir, e.plan, transcode.Prefetch) != nil {
			queued++
		}
	}
}

// queueNext speculates on exactly one file: the one after what is playing now.
// That is the episode the viewer is most likely to reach, and it is a single
// job rather than a directory's worth.
func (f *FS) queueNext(ctx context.Context, virt string, cur entry) {
	dir := path.Dir(virt)
	ents, err := f.list(ctx, dir)
	if err != nil {
		return
	}
	at := -1
	for i, e := range ents {
		if e.virtName == cur.virtName {
			at = i
			break
		}
	}
	if at < 0 {
		return
	}
	for _, e := range ents[at+1:] {
		if e.isDir || !e.plan.NeedsWork() {
			continue
		}
		if e.plan.Action == probe.FullTranscode && !f.cfg.PrefetchVideo {
			continue
		}
		key := f.key(e)
		if f.cache.Complete(key) {
			continue
		}
		if f.tm.Start(key, e.realPath, dir, e.plan, transcode.Prefetch) != nil {
			log.Printf("queued next after playback: %s", e.realPath)
		}
		return
	}
}

func (f *FS) lookup(ctx context.Context, virt string) (entry, error) {
	parent := path.Dir(virt)
	base := path.Base(virt)
	ents, err := f.list(ctx, parent)
	if err != nil {
		return entry{}, err
	}
	for _, e := range ents {
		if e.virtName == base {
			return e, nil
		}
	}
	return entry{}, os.ErrNotExist
}

// --- opening ---

func (f *FS) openReal(e entry) (*namedFile, error) {
	fh, err := os.Open(e.realPath)
	if err != nil {
		return nil, err
	}
	return &namedFile{File: fh, fi: f.infoFor(e)}, nil
}

func (f *FS) openConverted(ctx context.Context, virt string, e entry) (webdav.File, error) {
	key := f.key(e)
	dir := path.Dir(virt)

	// A new playback makes any guess queued for somewhere else stale.
	f.tm.DropPendingOutside(dir)

	// Line up the next file now rather than on the way out: with
	// WaitForComplete the open below can block for as long as the conversion
	// takes, and by then the viewer is already watching.
	go f.queueNext(context.WithoutCancel(ctx), virt, e)

	if f.cache.Complete(key) {
		f.cache.Touch(key)
		return f.openCached(key, e)
	}

	// Playback outranks anything speculative and will preempt it.
	job := f.tm.Start(key, e.realPath, dir, e.plan, transcode.Playback)
	if job == nil { // finished between the check and the start
		f.cache.Touch(key)
		return f.openCached(key, e)
	}

	if f.cfg.WaitForComplete {
		timer := time.NewTimer(f.cfg.WaitTimeout)
		defer timer.Stop()
		select {
		case <-job.Done():
			if err := job.Err(); err != nil {
				return nil, err
			}
			f.cache.Touch(key)
			return f.openCached(key, e)
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			log.Printf("conversion still running after %s, streaming progressively: %s",
				f.cfg.WaitTimeout, e.realPath)
		}
	}

	return f.openGrowing(key, e, job)
}

func (f *FS) openCached(key string, e entry) (*namedFile, error) {
	fh, err := os.Open(f.cache.Path(key))
	if err != nil {
		return nil, err
	}
	fi, err := fh.Stat()
	if err != nil {
		fh.Close()
		return nil, err
	}
	return &namedFile{File: fh, fi: &info{
		name: e.virtName, size: fi.Size(), mode: 0o444, mod: e.fi.ModTime(),
	}}, nil
}

func (f *FS) openGrowing(key string, e entry, job *transcode.Job) (*growingFile, error) {
	// The job may not have created the output yet.
	deadline := time.Now().Add(30 * time.Second)
	var fh *os.File
	var err error
	for {
		fh, err = os.Open(f.cache.Path(key))
		if err == nil {
			break
		}
		if job.Finished() || time.Now().After(deadline) {
			if jerr := job.Err(); jerr != nil {
				return nil, jerr
			}
			return nil, err
		}
		time.Sleep(200 * time.Millisecond)
	}
	return &growingFile{
		f:   fh,
		job: job,
		fi:  &info{name: e.virtName, size: e.plan.SrcSize, mode: 0o444, mod: e.fi.ModTime()},
	}, nil
}

// key ties a cache entry to both the source file and the settings used to
// produce it, so changing an encoding option invalidates old output.
func (f *FS) key(e entry) string {
	return cache.Key(e.realPath, e.fi.Size(), e.fi.ModTime().Unix(),
		string(e.plan.Action),
		f.cfg.AudioCodec, f.cfg.AudioBitrate,
		f.cfg.VideoCodec, f.cfg.VideoPreset, f.cfg.VideoCRF,
	)
}

// settled reports whether the advertised file exists in full, and therefore
// whether anything we say about its size is true.
func (f *FS) settled(e entry) bool {
	if e.isDir || !e.plan.NeedsWork() {
		return true
	}
	return f.cache.Complete(f.key(e))
}

func (f *FS) infoFor(e entry) *info {
	if e.isDir {
		return &info{name: e.virtName, mode: e.fi.Mode(), mod: e.fi.ModTime(), dir: true}
	}
	size := e.fi.Size()
	if e.plan.NeedsWork() {
		// Before the conversion exists, the source size is the best estimate
		// available; an audio-only remux lands within a few percent of it.
		if k := f.key(e); f.cache.Complete(k) {
			size = f.cache.Size(k)
		}
	}
	return &info{name: e.virtName, size: size, mode: 0o444, mod: e.fi.ModTime()}
}

func (f *FS) realPath(virt string) string {
	return filepath.Join(f.cfg.MediaDir, filepath.FromSlash(strings.TrimPrefix(virt, "/")))
}

func cleanVirt(name string) (string, error) {
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	c := path.Clean(name)
	if strings.Contains(c, "..") {
		return "", os.ErrPermission
	}
	return c, nil
}
