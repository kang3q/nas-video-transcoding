// Package outpath maps a path in the source library to its place in the output
// tree, and is the only part of the program allowed to turn user input into a
// filesystem path.
//
// v1 was a read-only server. v2 accepts paths from a browser and writes files,
// which makes path handling the largest security change in the project. The
// defence is a type: [Rel] holds an unexported string and [Mapper.ParseRel] is
// its only constructor, so a path that has not been validated cannot be passed
// to anything that opens or creates a file — it will not compile.
package outpath

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

var (
	ErrBadPath  = errors.New("outpath: invalid path")
	ErrEscapes  = errors.New("outpath: path escapes the library root")
	ErrExcluded = errors.New("outpath: path is inside the output directory")
)

// Rel is a validated location inside the source library, relative to its root
// and always slash-separated. The zero value is the root itself.
type Rel struct{ s string }

// Root is the library root.
func Root() Rel { return Rel{} }

func (r Rel) String() string { return r.s }
func (r Rel) IsRoot() bool   { return r.s == "" }
func (r Rel) Base() string {
	if r.s == "" {
		return ""
	}
	return path.Base(r.s)
}

// Dir is the containing directory, or the root when there is none.
func (r Rel) Dir() Rel {
	if r.s == "" {
		return Rel{}
	}
	d := path.Dir(r.s)
	if d == "." {
		return Rel{}
	}
	return Rel{s: d}
}

// Ext is the source file's extension, lowercased, including the dot.
func (r Rel) Ext() string { return strings.ToLower(path.Ext(r.s)) }

type Mapper struct {
	srcRoot string
	outRoot string
	// outRel is where the output tree sits inside the source tree, or "" when
	// it sits elsewhere. Putting the output inside the share is convenient, so
	// it has to be hidden rather than forbidden.
	outRel string
	root   *os.Root
}

// NewMapper opens the source root for traversal-proof access. Both directories
// must already be absolute and symlink-resolved; config.Load does that.
func NewMapper(srcRoot, outRoot, outRel string) (*Mapper, error) {
	root, err := os.OpenRoot(srcRoot)
	if err != nil {
		return nil, err
	}
	return &Mapper{srcRoot: srcRoot, outRoot: outRoot, outRel: outRel, root: root}, nil
}

func (m *Mapper) Close() error { return m.root.Close() }

func (m *Mapper) SourceRoot() string { return m.srcRoot }
func (m *Mapper) OutputRoot() string { return m.outRoot }

// ParseRel validates a path supplied by a client. It is deliberately the only
// way to build a Rel.
func (m *Mapper) ParseRel(input string) (Rel, error) {
	if strings.ContainsRune(input, 0) {
		return Rel{}, fmt.Errorf("%w: contains NUL", ErrBadPath)
	}
	// A backslash is a legal filename character on Linux but means "separator"
	// elsewhere, so `..\x` would be one filename here and traversal there.
	// Refusing it keeps the meaning of a path from depending on the host.
	if strings.ContainsRune(input, '\\') {
		return Rel{}, fmt.Errorf("%w: contains a backslash", ErrBadPath)
	}
	// A leading slash means "from the library root", not "from /". Handlers
	// pass the tail of a URL path, which starts with one.
	s := strings.Trim(input, "/")
	if s == "" || s == "." {
		return Rel{}, nil
	}

	// Clean first so "a/./b" and "a//b" normalise, then judge the result by
	// its elements. Testing the whole string for ".." would reject the
	// perfectly ordinary filename "Foo..Bar.mkv" — a bug v1 shipped.
	s = path.Clean(s)
	if s == ".." || strings.HasPrefix(s, "../") {
		return Rel{}, ErrEscapes
	}
	if path.IsAbs(s) {
		return Rel{}, fmt.Errorf("%w: absolute", ErrBadPath)
	}
	for _, el := range strings.Split(s, "/") {
		if el == "" || el == "." || el == ".." {
			return Rel{}, ErrEscapes
		}
	}

	r := Rel{s: s}
	if m.Excluded(r) {
		return Rel{}, ErrExcluded
	}
	return r, nil
}

// Join extends a validated path with one more element, which is checked the
// same way. Used for directory listings, where the element comes from the
// filesystem rather than from a client, but the check costs nothing.
func (m *Mapper) Join(base Rel, name string) (Rel, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return Rel{}, fmt.Errorf("%w: bad element %q", ErrBadPath, name)
	}
	return m.ParseRel(path.Join(base.s, name))
}

// Excluded reports whether a path is the output directory, or inside it.
func (m *Mapper) Excluded(r Rel) bool {
	if m.outRel == "" || r.s == "" {
		return false
	}
	return r.s == m.outRel || strings.HasPrefix(r.s, m.outRel+"/")
}

// Source is the absolute path of the original file.
func (m *Mapper) Source(r Rel) string {
	return filepath.Join(m.srcRoot, filepath.FromSlash(r.s))
}

// Output is where the converted MP4 belongs. The extension is always replaced,
// so Show.mkv and Show.avi in one directory would both want Show.mp4; callers
// building a batch must reject that collision rather than let one overwrite
// the other.
func (m *Mapper) Output(r Rel) string {
	rel := strings.TrimSuffix(r.s, path.Ext(r.s))
	return filepath.Join(m.outRoot, filepath.FromSlash(rel)) + ".mp4"
}

// Partial is where a conversion is written while it runs. Renaming it into
// place on success makes "the output exists" mean "the output is complete",
// with no marker file and no window where a half-written file looks finished.
func (m *Mapper) Partial(r Rel) string { return m.Output(r) + ".part" }

// OutputDir is the mirrored directory for a source directory.
func (m *Mapper) OutputDir(r Rel) string {
	return filepath.Join(m.outRoot, filepath.FromSlash(r.s))
}

// Open opens a source file through the root, which refuses to follow a symlink
// out of the library even if one exists inside it.
func (m *Mapper) Open(r Rel) (*os.File, error) {
	if r.IsRoot() {
		return m.root.Open(".")
	}
	return m.root.Open(r.s)
}

func (m *Mapper) Stat(r Rel) (fs.FileInfo, error) {
	if r.IsRoot() {
		return m.root.Stat(".")
	}
	return m.root.Stat(r.s)
}

// ReadDir lists a source directory through the root.
func (m *Mapper) ReadDir(r Rel) ([]os.DirEntry, error) {
	f, err := m.Open(r)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}
