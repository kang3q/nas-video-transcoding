// Package library browses the source tree and decides what order to convert
// things in.
//
// The ordering is the interesting part. Conversion is slow enough that the
// queue order decides what you can watch tonight, so the file you picked is
// always converted first and the rest follow in viewing order.
package library

import (
	"sort"
	"strings"
	"time"
	"unicode"

	"nvt/ver2/internal/outpath"
)

// videoExt is the set of containers worth offering to convert. Carried over
// from v1, where the list was assembled from a real library.
var videoExt = map[string]bool{
	".avi": true, ".mkv": true, ".mp4": true, ".m4v": true, ".mov": true,
	".ts": true, ".m2ts": true, ".mts": true, ".mpg": true, ".mpeg": true,
	".wmv": true, ".asf": true, ".flv": true, ".vob": true, ".rmvb": true,
	".rm": true, ".divx": true, ".ogm": true, ".webm": true, ".3gp": true,
	".m2v": true, ".dat": true,
}

func IsVideo(r outpath.Rel) bool { return videoExt[r.Ext()] }

// systemDirs are the folders a NAS keeps for itself, scattered through every
// share. Synology puts an @eaDir beside everything it has indexed, holding
// thumbnails, metadata, and sometimes a converted preview of the video — a
// .mp4 that would otherwise turn up in the list of things to watch. Inside
// it are directories named exactly like the files they describe, so an
// @eaDir also contains something called "Show - 01.smi" that is a folder.
//
// None of it belongs to the person browsing their library.
var systemDirs = map[string]bool{
	"@eaDir":                    true, // Synology: thumbnails, metadata, previews
	"#recycle":                  true, // Synology: the share's recycle bin
	"@tmp":                      true,
	"@Recently-Snapshot":        true,
	".@__thumb":                 true, // QNAP
	"$RECYCLE.BIN":              true, // left behind by Windows clients
	"System Volume Information": true,
	".Trashes":                  true, // left behind by macOS clients
	".Spotlight-V100":           true,
	".fseventsd":                true,
}

// skip reports whether a directory entry is none of the viewer's business.
func skip(name string) bool {
	return strings.HasPrefix(name, ".") || systemDirs[name]
}

type Entry struct {
	Name    string
	Rel     outpath.Rel
	IsDir   bool
	IsVideo bool
	Size    int64
	// Mod is carried because the probe cache is keyed by it. Reading a
	// directory already costs a stat per file; making the caller stat them
	// all again to ask "have we seen this one?" would double the price of
	// walking a library.
	Mod time.Time
}

type Listing struct {
	Dir     outpath.Rel
	Entries []Entry
}

type Library struct{ m *outpath.Mapper }

func New(m *outpath.Mapper) *Library { return &Library{m: m} }

// List reads one directory. Nothing is probed here: opening every file to
// inspect it is what made v1 take half a minute to show a folder.
func (l *Library) List(dir outpath.Rel) (Listing, error) {
	ents, err := l.m.ReadDir(dir)
	if err != nil {
		return Listing{}, err
	}

	out := make([]Entry, 0, len(ents))
	for _, de := range ents {
		name := de.Name()
		if skip(name) {
			continue
		}
		rel, err := l.m.Join(dir, name)
		if err != nil {
			continue // the output directory, or a name we will not touch
		}
		e := Entry{Name: name, Rel: rel, IsDir: de.IsDir()}
		if !e.IsDir {
			if fi, err := de.Info(); err == nil {
				if !fi.Mode().IsRegular() {
					continue
				}
				e.Size = fi.Size()
				e.Mod = fi.ModTime()
			}
			e.IsVideo = IsVideo(rel)
		}
		out = append(out, e)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return NaturalLess(out[i].Name, out[j].Name)
	})
	return Listing{Dir: dir, Entries: out}, nil
}

// ListOutput reads one directory of the converted tree.
//
// It mirrors the library, so the same relative path means the same show in
// both — which is what lets a conversion be traced back to its source. The
// ".part" files of conversions still running are left out: they have no index
// and cannot be played.
func (l *Library) ListOutput(dir outpath.Rel) (Listing, error) {
	ents, err := l.m.ReadOutputDir(dir)
	if err != nil {
		return Listing{}, err
	}

	out := make([]Entry, 0, len(ents))
	for _, de := range ents {
		name := de.Name()
		if skip(name) || strings.HasSuffix(name, ".part") {
			continue
		}
		if dir.IsRoot() && name == airplayDir {
			continue
		}
		rel, err := l.m.Join(dir, name)
		if err != nil {
			continue
		}
		e := Entry{Name: name, Rel: rel, IsDir: de.IsDir()}
		if !e.IsDir {
			fi, err := de.Info()
			if err != nil || !fi.Mode().IsRegular() {
				continue
			}
			e.Size = fi.Size()
			e.Mod = fi.ModTime()
			e.IsVideo = IsVideo(rel)
		}
		out = append(out, e)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return NaturalLess(out[i].Name, out[j].Name)
	})
	return Listing{Dir: dir, Entries: out}, nil
}

// Walk limits. A library is big but not unbounded, and a tree that turns out
// to be either is a bug somewhere else — better to show most of it than to
// spend a minute finding that out.
const (
	maxWalkDepth   = 24
	maxWalkEntries = 50000
)

// WalkOutput lists every converted file under a directory, at any depth.
//
// Only directories are read. Nothing is opened, which is the whole reason
// this is affordable: a conversion existing is already proof that it plays,
// so there is nothing to inspect.
func (l *Library) WalkOutput(root outpath.Rel) []Entry {
	return l.walk(root, l.ListOutput)
}

// WalkVideos lists every video in the library, at any depth. Whether each one
// can be played without converting is a separate question, answered from the
// probe cache by the caller — it must not be answered by opening files here.
func (l *Library) WalkVideos(root outpath.Rel) []Entry {
	return l.walk(root, l.List)
}

func (l *Library) walk(root outpath.Rel, list func(outpath.Rel) (Listing, error)) []Entry {
	var out []Entry
	type step struct {
		dir   outpath.Rel
		depth int
	}
	queue := []step{{dir: root}}

	for len(queue) > 0 && len(out) < maxWalkEntries {
		cur := queue[0]
		queue = queue[1:]

		listing, err := list(cur.dir)
		if err != nil {
			continue // unreadable, or gone since the parent was read
		}
		for _, e := range listing.Entries {
			switch {
			case e.IsDir:
				if cur.depth < maxWalkDepth {
					queue = append(queue, step{dir: e.Rel, depth: cur.depth + 1})
				}
			case e.IsVideo:
				out = append(out, e)
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		di, dj := out[i].Rel.Dir().String(), out[j].Rel.Dir().String()
		if di != dj {
			return NaturalLess(di, dj)
		}
		return NaturalLess(out[i].Name, out[j].Name)
	})
	return out
}

// airplayDir held the clips of an AirPlay diagnostic that has since been
// removed. The folder is still skipped because the ones already written to a
// NAS are still there, and they are test output rather than anything anybody
// meant to keep.
const airplayDir = "_airplay"

// Videos lists the video files in one directory, in viewing order.
func (l *Library) Videos(dir outpath.Rel) ([]outpath.Rel, error) {
	listing, err := l.List(dir)
	if err != nil {
		return nil, err
	}
	var out []outpath.Rel
	for _, e := range listing.Entries {
		if !e.IsDir && e.IsVideo {
			out = append(out, e.Rel)
		}
	}
	return out, nil
}

// Scope is how much of a directory a request covers.
type Scope string

const (
	// ScopeFile converts only what was picked.
	ScopeFile Scope = "file"
	// ScopeOnwards converts from the picked file to the end of the directory.
	ScopeOnwards Scope = "onwards"
	// ScopeFolder converts everything, starting at the picked file and
	// wrapping around to the ones before it.
	ScopeFolder Scope = "folder"
)

// Order arranges a directory's files for a request. Whichever scope is asked
// for, the chosen file comes first: it is the one being waited on.
//
//	files:    ep01 ep02 ... ep24 ... ep26
//	picked:   ep08
//	file:     ep08
//	onwards:  ep08 ep09 ... ep26
//	folder:   ep08 ep09 ... ep26 ep01 ... ep07
func Order(files []outpath.Rel, picked outpath.Rel, scope Scope) []outpath.Rel {
	at := -1
	for i, f := range files {
		if f == picked {
			at = i
			break
		}
	}
	if at < 0 {
		// The pick is not in this listing — convert just it.
		return []outpath.Rel{picked}
	}

	switch scope {
	case ScopeFile:
		return []outpath.Rel{picked}
	case ScopeOnwards:
		return append([]outpath.Rel(nil), files[at:]...)
	case ScopeFolder:
		out := make([]outpath.Rel, 0, len(files))
		out = append(out, files[at:]...)
		out = append(out, files[:at]...)
		return out
	}
	return []outpath.Rel{picked}
}

// NaturalLess orders names the way episodes are numbered, comparing runs of
// digits as numbers. Plain string order would give 1, 10, 11, 2 — which for a
// queue built out of viewing order is not a cosmetic problem.
func NaturalLess(a, b string) bool {
	la, lb := strings.ToLower(a), strings.ToLower(b)
	i, j := 0, 0
	for i < len(la) && j < len(lb) {
		ca, cb := la[i], lb[j]
		if isDigit(ca) && isDigit(cb) {
			si, sj := i, j
			for i < len(la) && isDigit(la[i]) {
				i++
			}
			for j < len(lb) && isDigit(lb[j]) {
				j++
			}
			na := strings.TrimLeft(la[si:i], "0")
			nb := strings.TrimLeft(lb[sj:j], "0")
			if len(na) != len(nb) {
				return len(na) < len(nb) // more digits means a bigger number
			}
			if na != nb {
				return na < nb
			}
			continue
		}
		if ca != cb {
			return ca < cb
		}
		i++
		j++
	}
	if len(la)-i != len(lb)-j {
		return len(la)-i < len(lb)-j
	}
	// Identical ignoring case: fall back to the original so the order is total.
	return a < b
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// TrimExt is a small helper for display: the name without its extension.
func TrimExt(name string) string {
	if i := strings.LastIndexFunc(name, func(r rune) bool { return r == '.' }); i > 0 {
		if !unicode.IsSpace(rune(name[i-1])) {
			return name[:i]
		}
	}
	return name
}
