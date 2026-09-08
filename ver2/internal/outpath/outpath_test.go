package outpath

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newTestMapper(t *testing.T, outRel string) (*Mapper, string, string) {
	t.Helper()
	base := t.TempDir()
	src := filepath.Join(base, "media")
	out := filepath.Join(base, "out")
	if outRel != "" {
		out = filepath.Join(src, outRel)
	}
	for _, d := range []string{src, out} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m, err := NewMapper(src, out, outRel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m, src, out
}

func TestParseRelAccepts(t *testing.T) {
	m, _, _ := newTestMapper(t, "")
	cases := map[string]string{
		"plain file":              "Movie.mkv",
		"nested":                  "Anime/Show/ep01.mkv",
		"leading slash stripped":  "/Anime/ep01.mkv",
		"redundant separators":    "Anime//Show/./ep01.mkv",
		"trailing slash":          "Anime/Show/",
		"spaces and brackets":     "Future Boy Conan - 24 [1080p] [x265].mkv",
		"korean":                  "애니/미래소년 코난/1화.avi",
		"dots inside the name":    "Foo..Bar.mkv",
		"name that starts a dot":  ".hidden.mkv",
		"name ending in two dots": "weird...mkv",
	}
	want := map[string]string{
		"plain file":              "Movie.mkv",
		"nested":                  "Anime/Show/ep01.mkv",
		"leading slash stripped":  "Anime/ep01.mkv",
		"redundant separators":    "Anime/Show/ep01.mkv",
		"trailing slash":          "Anime/Show",
		"spaces and brackets":     "Future Boy Conan - 24 [1080p] [x265].mkv",
		"korean":                  "애니/미래소년 코난/1화.avi",
		"dots inside the name":    "Foo..Bar.mkv",
		"name that starts a dot":  ".hidden.mkv",
		"name ending in two dots": "weird...mkv",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := m.ParseRel(in)
			if err != nil {
				t.Fatalf("ParseRel(%q) = %v, want it accepted", in, err)
			}
			if got.String() != want[name] {
				t.Errorf("ParseRel(%q) = %q, want %q", in, got.String(), want[name])
			}
		})
	}
}

// "Foo..Bar.mkv" is the case v1 got wrong: it tested the whole path for ".."
// and so rejected an ordinary filename.
func TestParseRelAcceptsDoubleDotInsideAName(t *testing.T) {
	m, _, _ := newTestMapper(t, "")
	if _, err := m.ParseRel("Show/S01..E02.mkv"); err != nil {
		t.Errorf("a filename containing '..' was rejected: %v", err)
	}
}

func TestParseRelRejects(t *testing.T) {
	m, _, _ := newTestMapper(t, "")
	cases := map[string]string{
		"parent":               "..",
		"parent prefix":        "../etc/passwd",
		"parent in the middle": "a/../../b",
		"NUL byte":             "a\x00b",
		"backslash":            `Show\ep01.mkv`,
		"backslash traversal":  `..\windows`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if got, err := m.ParseRel(in); err == nil {
				t.Errorf("ParseRel(%q) = %q, want an error", in, got.String())
			}
		})
	}
}

// A leading slash is read as "from the library root", so an absolute-looking
// path lands inside the library rather than escaping it. That is what handlers
// need, since a URL path always starts with one.
func TestParseRelTreatsLeadingSlashAsRootRelative(t *testing.T) {
	m, _, _ := newTestMapper(t, "")
	for in, want := range map[string]string{
		"/etc/passwd":  "etc/passwd",
		"//etc/passwd": "etc/passwd",
		"/Anime/a.mkv": "Anime/a.mkv",
	} {
		got, err := m.ParseRel(in)
		if err != nil {
			t.Errorf("ParseRel(%q) = %v", in, err)
			continue
		}
		if got.String() != want {
			t.Errorf("ParseRel(%q) = %q, want %q", in, got.String(), want)
		}
	}
}

// Percent-encoding is the server's job to undo before it gets here; once
// decoded these are ordinary traversal attempts and must be refused.
func TestParseRelRejectsDecodedEncodings(t *testing.T) {
	m, _, _ := newTestMapper(t, "")
	for _, in := range []string{"../secret", "a/../../b", "/../b"} {
		if _, err := m.ParseRel(in); !errors.Is(err, ErrEscapes) && !errors.Is(err, ErrBadPath) {
			t.Errorf("ParseRel(%q) err = %v, want a rejection", in, err)
		}
	}
}

func TestExcludesTheOutputDirectory(t *testing.T) {
	m, _, _ := newTestMapper(t, "_nvt")

	for _, in := range []string{"_nvt", "_nvt/Show/ep01.mp4"} {
		if _, err := m.ParseRel(in); !errors.Is(err, ErrExcluded) {
			t.Errorf("ParseRel(%q) err = %v, want ErrExcluded", in, err)
		}
	}
	// A directory that merely shares the prefix is a different directory.
	if _, err := m.ParseRel("_nvtother/ep01.mkv"); err != nil {
		t.Errorf("a sibling with a shared prefix was excluded: %v", err)
	}
}

func TestOutputPaths(t *testing.T) {
	m, src, out := newTestMapper(t, "")
	r, err := m.ParseRel("Anime/Show/ep01.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := m.Source(r), filepath.Join(src, "Anime/Show/ep01.mkv"); got != want {
		t.Errorf("Source = %q, want %q", got, want)
	}
	if got, want := m.Output(r), filepath.Join(out, "Anime/Show/ep01.mp4"); got != want {
		t.Errorf("Output = %q, want %q", got, want)
	}
	if got, want := m.Partial(r), m.Output(r)+".part"; got != want {
		t.Errorf("Partial = %q, want %q", got, want)
	}
}

// Replacing the extension means two sources in one directory can want the same
// output. The mapper does not resolve that; callers must notice.
func TestOutputCollidesAcrossExtensions(t *testing.T) {
	m, _, _ := newTestMapper(t, "")
	a, _ := m.ParseRel("Show/ep01.mkv")
	b, _ := m.ParseRel("Show/ep01.avi")
	if m.Output(a) != m.Output(b) {
		t.Fatal("expected these to collide; if that changed, the batch builder's collision check can go")
	}
}

func TestOutputKeepsDotsInTheName(t *testing.T) {
	m, _, out := newTestMapper(t, "")
	r, err := m.ParseRel("Show.S01.E02.1080p.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := m.Output(r), filepath.Join(out, "Show.S01.E02.1080p.mp4"); got != want {
		t.Errorf("Output = %q, want %q", got, want)
	}
}

func TestRelHelpers(t *testing.T) {
	m, _, _ := newTestMapper(t, "")
	r, _ := m.ParseRel("Anime/Show/ep01.mkv")
	if got := r.Dir().String(); got != "Anime/Show" {
		t.Errorf("Dir = %q", got)
	}
	if got := r.Base(); got != "ep01.mkv" {
		t.Errorf("Base = %q", got)
	}
	if got := r.Ext(); got != ".mkv" {
		t.Errorf("Ext = %q", got)
	}
	top, _ := m.ParseRel("Anime")
	if got := top.Dir(); !got.IsRoot() {
		t.Errorf("Dir of a top-level entry = %q, want the root", got.String())
	}
	if !Root().IsRoot() || Root().Dir().IsRoot() != true {
		t.Error("the root's parent should be the root")
	}
}

// A symlink inside the library pointing outside it must not become a way out.
// path.Clean cannot see symlinks, which is why the source is opened through
// os.Root rather than by joining strings.
func TestOpenRefusesToFollowASymlinkOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ")
	}
	m, src, _ := newTestMapper(t, "")

	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(secret), filepath.Join(src, "escape")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}

	r, err := m.ParseRel("escape/secret.txt")
	if err != nil {
		t.Fatalf("ParseRel rejected a syntactically fine path: %v", err)
	}
	if f, err := m.Open(r); err == nil {
		f.Close()
		t.Fatal("opened a file outside the library through a symlink")
	}
}

func TestReadDirListsThroughTheRoot(t *testing.T) {
	m, src, _ := newTestMapper(t, "")
	if err := os.MkdirAll(filepath.Join(src, "Anime"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "Anime", "ep01.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, _ := m.ParseRel("Anime")
	ents, err := m.ReadDir(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "ep01.mkv" {
		t.Errorf("ReadDir = %v, want [ep01.mkv]", names(ents))
	}
	if _, err := m.ReadDir(Root()); err != nil {
		t.Errorf("ReadDir(root) = %v", err)
	}
}

func names(ents []os.DirEntry) []string {
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestJoinChecksTheElement(t *testing.T) {
	m, _, _ := newTestMapper(t, "")
	base, _ := m.ParseRel("Anime")
	if got, err := m.Join(base, "ep01.mkv"); err != nil || got.String() != "Anime/ep01.mkv" {
		t.Errorf("Join = %q, %v", got.String(), err)
	}
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`} {
		if _, err := m.Join(base, bad); err == nil {
			t.Errorf("Join accepted element %q", bad)
		}
	}
}

func TestParseRelRootForms(t *testing.T) {
	m, _, _ := newTestMapper(t, "")
	for _, in := range []string{"", "/", ".", "//"} {
		r, err := m.ParseRel(in)
		if err != nil {
			t.Errorf("ParseRel(%q) = %v", in, err)
			continue
		}
		if !r.IsRoot() {
			t.Errorf("ParseRel(%q) = %q, want the root", in, r.String())
		}
	}
}

// Every path handed to the filesystem must stay under its root, whatever
// ParseRel let through.
func TestMappedPathsStayUnderTheirRoots(t *testing.T) {
	m, src, out := newTestMapper(t, "")
	for _, in := range []string{"a.mkv", "a/b/c.mkv", "Foo..Bar.mkv", "애니/1화.avi"} {
		r, err := m.ParseRel(in)
		if err != nil {
			t.Fatalf("ParseRel(%q): %v", in, err)
		}
		if !strings.HasPrefix(m.Source(r), src+string(filepath.Separator)) {
			t.Errorf("Source(%q) = %q escaped %q", in, m.Source(r), src)
		}
		if !strings.HasPrefix(m.Output(r), out+string(filepath.Separator)) {
			t.Errorf("Output(%q) = %q escaped %q", in, m.Output(r), out)
		}
	}
}
