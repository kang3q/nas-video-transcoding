// Package subs finds subtitles for a video and gets them into a state ffmpeg
// can draw into the picture.
//
// Three things make this fiddlier than it sounds:
//
//   - Korean .smi files are almost always CP949, not UTF-8. Handed to ffmpeg
//     as-is they come out as mojibake, or the demuxer simply gives up.
//   - ffmpeg's filter syntax reads ":" as an option separator and "[" as a
//     stream label, and a real library is full of names like
//     "Show - 24 [1080p].ass". Rather than escape that, everything is copied
//     to a plain temporary name first.
//   - Bitmap subtitles are pictures. They cannot become a sidecar or a WebVTT
//     track, and drawing them needs a different filter entirely.
package subs

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"

	"nvt/ver2/internal/mediainfo"
	"nvt/ver2/internal/outpath"
)

type Kind string

const (
	Embedded Kind = "embedded" // a track inside the video file
	Sidecar  Kind = "sidecar"  // a separate file next to it
)

type Track struct {
	ID     string // "embedded:2" or "sidecar:Show.ko.smi"
	Kind   Kind
	Index  int    // stream index, for embedded
	File   string // filename relative to the video's directory, for sidecars
	Lang   string
	Title  string
	Codec  string
	Bitmap bool
	Label  string
}

// Korean reports whether a track is the one this library is normally watched
// with. Language tags are the reliable signal; filenames are the fallback,
// because a sidecar downloaded separately rarely carries a tag.
func (t Track) Korean() bool {
	switch strings.ToLower(t.Lang) {
	case "ko", "kor", "kore":
		return true
	}
	name := strings.ToLower(t.File + " " + t.Title)
	for _, hint := range []string{"korean", "kor", ".ko.", "_ko_", "-ko-", "한글", "한국"} {
		if strings.Contains(name, hint) {
			return true
		}
	}
	return false
}

// Burnable reports whether this track can be drawn in. Bitmap subtitles need
// an overlay filter rather than libass, which is not wired up.
func (t Track) Burnable() bool { return !t.Bitmap }

// Format is what to hand ffmpeg at the end.
//
// The two are not interchangeable, and which one is right depends on what
// happens next. libass draws ASS, so burning needs it. A track inside an MP4
// is tx3g, and tx3g carries the position of the text on screen — which means
// ffmpeg has to invent those coordinates from whatever it was given. Handing
// it an ASS file means handing it PlayResX/PlayResY and margins from a
// subtitle that knew nothing about this video's dimensions, and the result
// lands wherever that arithmetic puts it. SRT has no geometry at all, so the
// player falls back to its own idea of where subtitles go, which is the one
// place viewers expect them.
type Format string

const (
	// FormatASS is for burning: libass needs it and honours its styling.
	FormatASS Format = "ass"
	// FormatSRT is for a subtitle track: plain lines and timings, no
	// positioning for anything downstream to get wrong.
	FormatSRT Format = "srt"
)

func (f Format) ext() string {
	if f == FormatSRT {
		return ".srt"
	}
	return ".ass"
}

func (f Format) codec() string {
	if f == FormatSRT {
		return "srt"
	}
	return "ass"
}

// sidecarExt are subtitle files worth looking for beside a video.
var sidecarExt = map[string]bool{
	".srt": true, ".ass": true, ".ssa": true, ".smi": true,
	".sami": true, ".vtt": true, ".sub": true,
}

type Finder struct {
	mapper *outpath.Mapper
	prober interface {
		Cached(path string, fi os.FileInfo) (mediainfo.Info, bool)
	}
}

func NewFinder(m *outpath.Mapper, prober interface {
	Cached(path string, fi os.FileInfo) (mediainfo.Info, bool)
}) *Finder {
	return &Finder{mapper: m, prober: prober}
}

// Find lists everything that could be burned into this video: the tracks
// inside it, and the subtitle files sitting beside it.
func (f *Finder) Find(rel outpath.Rel) []Track {
	var out []Track
	out = append(out, f.embedded(rel)...)
	out = append(out, f.sidecars(rel)...)

	// Korean first, then anything else. For a whole-folder conversion nobody
	// is picking per file, so this order is the rule that gets applied.
	sort.SliceStable(out, func(i, j int) bool {
		ki, kj := out[i].Korean(), out[j].Korean()
		if ki != kj {
			return ki
		}
		// Pictures last: they cannot be burned in yet.
		return !out[i].Bitmap && out[j].Bitmap
	})
	return out
}

func (f *Finder) embedded(rel outpath.Rel) []Track {
	src := f.mapper.Source(rel)
	fi, err := os.Stat(src)
	if err != nil {
		return nil
	}
	info, ok := f.prober.Cached(src, fi)
	if !ok {
		return nil
	}

	var out []Track
	for _, s := range info.Subtitles() {
		t := Track{
			ID: fmt.Sprintf("embedded:%d", s.Index), Kind: Embedded,
			Index: s.Index, Lang: s.Lang, Title: s.Title,
			Codec: s.Codec, Bitmap: s.Bitmap(),
		}
		t.Label = label(t)
		out = append(out, t)
	}
	return out
}

// sidecars looks beside the video and in the Subs/<name>/ layout some
// releases use.
func (f *Finder) sidecars(rel outpath.Rel) []Track {
	base := strings.TrimSuffix(rel.Base(), path.Ext(rel.Base()))
	dir := rel.Dir()

	var out []Track
	collect := func(searchDir outpath.Rel, requirePrefix bool) {
		ents, err := f.mapper.ReadDir(searchDir)
		if err != nil {
			return
		}
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !sidecarExt[strings.ToLower(path.Ext(name))] {
				continue
			}
			stem := strings.TrimSuffix(name, path.Ext(name))
			if requirePrefix && !strings.HasPrefix(stem, base) {
				continue
			}
			r, err := f.mapper.Join(searchDir, name)
			if err != nil {
				continue
			}
			t := Track{
				ID: "sidecar:" + r.String(), Kind: Sidecar,
				File: name, Codec: strings.TrimPrefix(strings.ToLower(path.Ext(name)), "."),
			}
			// A ".sub" is two unrelated formats sharing an extension.
			// MicroDVD is text and converts like any other; VobSub is a
			// pair of files holding bitmap images, which cannot become a
			// text track at all and needs a different filter to draw. It
			// has to be recognised here or the conversion fails an hour
			// later with nothing useful said.
			if t.Bitmap = f.isBitmapSidecar(r, searchDir, stem); t.Bitmap {
				t.Codec = "vobsub"
			}
			t.Lang = langFromName(stem, base)
			if t.Lang == "" && !t.Bitmap && f.sniffKorean(r) {
				t.Lang = "kor"
			}
			t.Label = label(t)
			out = append(out, t)
		}
	}

	collect(dir, true)
	if subsDir, err := f.mapper.Join(dir, "Subs"); err == nil {
		collect(subsDir, false)
		if perTitle, err := f.mapper.Join(subsDir, base); err == nil {
			collect(perTitle, false)
		}
	}
	return out
}

// isBitmapSidecar reports whether a ".sub" is VobSub rather than MicroDVD.
//
// The two share an extension and have nothing else in common. MicroDVD is
// lines of text, each prefixed with the frame it starts and ends on.
// VobSub is the subtitle stream lifted straight off a DVD: bitmap images,
// with the timings and palette in a separate ".idx" beside it. There is no
// text in it to extract, and drawing it needs an overlay rather than libass.
//
// The .idx settles it when there is one, and the bytes settle it when there
// is not — MicroDVD begins with "{".
func (f *Finder) isBitmapSidecar(r outpath.Rel, dir outpath.Rel, stem string) bool {
	if r.Ext() != ".sub" {
		return false
	}
	if idx, err := f.mapper.Join(dir, stem+".idx"); err == nil {
		if _, err := f.mapper.Stat(idx); err == nil {
			return true
		}
	}
	fh, err := f.mapper.Open(r)
	if err != nil {
		return false
	}
	defer fh.Close()
	head := make([]byte, 16)
	n, _ := fh.Read(head)
	// MicroDVD's first character is the opening brace of a frame number.
	// Anything else at the start of a .sub is not text we can read.
	return !bytes.Contains(head[:n], []byte("{"))
}

// sniffKorean reads the subtitle and looks for Hangul.
//
// Naming conventions do not survive contact with a real library. The common
// case here is a file named exactly like the video —
// "DEATH NOTE 데스노트 02 (704x396 DivX).smi" beside the .avi of the same name —
// which carries no language tag at all, because there is only one subtitle and
// nothing to distinguish it from. Guessing from the name finds nothing, and
// the viewer is then offered "굽지 않음" for a file whose subtitles are the
// reason they are converting it.
//
// Opening the file settles it. Subtitles are a few tens of kilobytes, the
// first chunk is enough, and Hangul is unmistakable — this cannot mistake an
// English track for a Korean one the way a filename rule can.
func (f *Finder) sniffKorean(r outpath.Rel) bool {
	fh, err := f.mapper.Open(r)
	if err != nil {
		return false
	}
	defer fh.Close()

	buf := make([]byte, 64<<10)
	n, err := fh.Read(buf)
	if n == 0 && err != nil {
		return false
	}
	// Cut back to the last newline so the chunk ends on a whole character.
	// A byte read half way through a CP949 pair decodes to nothing useful and
	// makes the decoder report failure for a file that is perfectly fine.
	// 0x0A cannot be the trailing byte of a CP949 pair or of a UTF-8
	// sequence, so a newline is always a safe place to stop.
	chunk := buf[:n]
	if i := bytes.LastIndexByte(chunk, '\n'); i > 0 {
		chunk = chunk[:i]
	}
	text, err := toUTF8(chunk)
	if err != nil {
		return false // neither UTF-8 nor CP949; nothing to read
	}
	return hasHangul(text)
}

// hasHangul reports whether the text holds Korean letters — precomposed
// syllables or the Jamo they are built from.
func hasHangul(b []byte) bool {
	for _, r := range string(b) {
		switch {
		case r >= 0xAC00 && r <= 0xD7A3, // 가 … 힣
			r >= 0x1100 && r <= 0x11FF, // conjoining Jamo
			r >= 0x3130 && r <= 0x318F: // compatibility Jamo
			return true
		}
	}
	return false
}

// langFromName reads the tag some releases leave between the name and the
// extension, as in "Show.S01E01.ko.srt".
func langFromName(stem, base string) string {
	suffix := strings.TrimPrefix(stem, base)
	suffix = strings.Trim(suffix, ". _-")
	if suffix == "" || len(suffix) > 12 {
		return ""
	}
	return strings.ToLower(suffix)
}

func label(t Track) string {
	var b strings.Builder
	if t.Kind == Embedded {
		fmt.Fprintf(&b, "내장 #%d", t.Index)
	} else {
		b.WriteString(t.File)
	}
	if t.Lang != "" {
		fmt.Fprintf(&b, " · %s", strings.ToUpper(t.Lang))
	}
	if t.Title != "" {
		fmt.Fprintf(&b, " · %s", t.Title)
	}
	if t.Korean() {
		b.WriteString(" · 한글")
	}
	if t.Bitmap {
		if t.Codec == "vobsub" {
			b.WriteString(" (VobSub — 그림이라 넣을 수 없습니다)")
		} else {
			b.WriteString(" (그림 자막 — 넣을 수 없습니다)")
		}
	}
	return b.String()
}

// PickLang finds a burnable track in one language. Used for the files after
// the first in a batch: the viewer chose a language once, and burning a
// different one into episode 17 because that is all it had would be a
// permanent surprise.
func PickLang(tracks []Track, lang string) (Track, bool) {
	if lang == "" {
		return Track{}, false
	}
	korean := lang == "kor" || lang == "ko"
	for _, t := range tracks {
		if !t.Burnable() {
			continue
		}
		if korean && t.Korean() {
			return t, true
		}
		if !korean && strings.EqualFold(t.Lang, lang) {
			return t, true
		}
	}
	return Track{}, false
}

// Pick chooses a track when there is nothing to go on but a preference for
// Korean, which is what this library is normally watched with.
func Pick(tracks []Track) (Track, bool) {
	for _, t := range tracks {
		if t.Korean() && t.Burnable() {
			return t, true
		}
	}
	for _, t := range tracks {
		if t.Burnable() {
			return t, true
		}
	}
	return Track{}, false
}

// Language is what to look for in the rest of a batch once this track has
// been chosen.
func (t Track) Language() string {
	if t.Korean() {
		return "kor"
	}
	return strings.ToLower(t.Lang)
}

// Find looks up one track by the id the form submitted.
func FindByID(tracks []Track, id string) (Track, bool) {
	for _, t := range tracks {
		if t.ID == id {
			return t, true
		}
	}
	return Track{}, false
}

// --- preparing ---

// Preparer turns a chosen track into a file ffmpeg can draw from: UTF-8, in a
// format libass understands, at a path with no characters the filter parser
// would misread.
type Preparer struct {
	Mapper  *outpath.Mapper
	FFmpeg  string
	TempDir string
}

// Prepare writes the subtitle somewhere safe and returns its path along with
// a cleanup function.
func (p *Preparer) Prepare(ctx context.Context, jobID string, rel outpath.Rel, id string, f Format) (string, func(), error) {
	noop := func() {}
	if id == "" {
		return "", noop, nil
	}
	if err := os.MkdirAll(p.TempDir, 0o755); err != nil {
		return "", noop, err
	}

	kind, arg, ok := strings.Cut(id, ":")
	if !ok {
		return "", noop, fmt.Errorf("subs: malformed track id %q", id)
	}

	var out string
	var err error
	switch Kind(kind) {
	case Embedded:
		out, err = p.extract(ctx, jobID, rel, arg, f)
	case Sidecar:
		out, err = p.convertSidecar(ctx, jobID, arg, f)
	default:
		err = fmt.Errorf("subs: unknown track kind %q", kind)
	}
	if err != nil {
		return "", noop, err
	}
	return out, func() { os.Remove(out) }, nil
}

// extract pulls an embedded track out into its own file. Referring to the
// video directly with subtitles=<src>:si=N would work, but only after escaping
// a filename full of brackets and colons — this avoids the question.
// extract pulls an embedded track out to a file of its own. It shares the
// empty-output trap with convertSidecar, and the same guard.
func (p *Preparer) extract(ctx context.Context, jobID string, rel outpath.Rel, arg string, f Format) (string, error) {
	idx, err := strconv.Atoi(arg)
	if err != nil {
		return "", fmt.Errorf("subs: bad stream index %q", arg)
	}
	out := filepath.Join(p.TempDir, jobID+f.ext())

	cmd := exec.CommandContext(ctx, p.FFmpeg,
		"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-i", p.Mapper.Source(rel),
		"-map", fmt.Sprintf("0:%d", idx),
		"-c:s", f.codec(),
		out,
	)
	said, err := cmd.CombinedOutput()
	if err != nil {
		os.Remove(out)
		return "", fmt.Errorf("subs: extracting stream %d: %w: %s", idx, err, strings.TrimSpace(string(said)))
	}
	if err := checkHasDialogue(out, f, fmt.Sprintf("stream %d of %s", idx, rel.Base())); err != nil {
		os.Remove(out)
		return "", withFFmpegOutput(err, said, "")
	}
	if err := repair(out, f, rel.Base()); err != nil {
		os.Remove(out)
		return "", err
	}
	return out, nil
}

// convertSidecar reads a subtitle file, fixes its encoding, and normalises it
// to ASS so libass has something it definitely understands.
func (p *Preparer) convertSidecar(ctx context.Context, jobID, relPath string, f Format) (string, error) {
	rel, err := p.Mapper.ParseRel(relPath)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(p.Mapper.Source(rel))
	if err != nil {
		return "", err
	}

	text, err := toUTF8(raw)
	if err != nil {
		return "", fmt.Errorf("subs: %s: %w", rel.Base(), err)
	}

	// Keep the original extension so ffmpeg picks the right demuxer.
	staged := filepath.Join(p.TempDir, jobID+"-in"+strings.ToLower(path.Ext(rel.Base())))
	if err := os.WriteFile(staged, text, 0o644); err != nil {
		return "", err
	}
	defer os.Remove(staged)

	out := filepath.Join(p.TempDir, jobID+f.ext())
	// warning, not error: a demuxer that reads the file and finds nothing it
	// recognises says so at warning level and exits zero. That message is the
	// only account of what went wrong, and throwing it away leaves "produced
	// no subtitle lines" with nothing behind it.
	cmd := exec.CommandContext(ctx, p.FFmpeg,
		"-nostdin", "-hide_banner", "-loglevel", "warning", "-y",
		"-i", staged, "-c:s", f.codec(), out,
	)
	said, err := cmd.CombinedOutput()
	if err != nil {
		os.Remove(out)
		return "", fmt.Errorf("subs: converting %s: %w: %s", rel.Base(), err, strings.TrimSpace(string(said)))
	}
	if err := checkHasDialogue(out, f, rel.Base()); err != nil {
		os.Remove(out)
		return "", withFFmpegOutput(err, said, firstLine(text))
	}
	if err := repair(out, f, rel.Base()); err != nil {
		os.Remove(out)
		return "", err
	}
	return out, nil
}

// repair fixes what a muxer downstream would refuse. Only SRT needs it: an
// unbounded cue is a SAMI habit, and it is on the way to an MP4 that the
// duration becomes fatal.
func repair(path string, f Format, name string) error {
	if f != FormatSRT {
		return nil
	}
	n, err := repairSRT(path)
	if err != nil {
		return fmt.Errorf("subs: repairing %s: %w", name, err)
	}
	if n > 0 {
		log.Printf("subtitle timings repaired in %s: %d cue(s) had no usable end", name, n)
	}
	return nil
}

// withFFmpegOutput attaches whatever ffmpeg had to say, and failing that the
// first line of what it was handed. One of the two usually names the problem:
// a file that is not really SAMI, or one whose text came out as mojibake.
func withFFmpegOutput(err error, said []byte, head string) error {
	if s := strings.TrimSpace(string(said)); s != "" {
		return fmt.Errorf("%w — ffmpeg said: %s", err, short(s))
	}
	if head != "" {
		return fmt.Errorf("%w — 파일 첫 줄: %q", err, head)
	}
	return err
}

func firstLine(b []byte) string {
	if i := bytes.IndexAny(b, "\r\n"); i >= 0 {
		b = b[:i]
	}
	return short(strings.TrimSpace(string(b)))
}

func short(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// checkHasDialogue refuses a subtitle file with nothing in it to draw.
//
// ffmpeg exits zero after writing an ASS file that has a style block and no
// events at all — a real outcome with SAMI, where the markup is often broken
// enough that the demuxer reads the header and finds no cues. Burning that in
// succeeds: it draws nothing, over an hour, and the only way to discover it is
// to watch the result. Failing here costs a minute and says why.
func checkHasDialogue(path string, f Format, name string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// An ASS file lists its cues as "Dialogue:"; an SRT marks each one with
	// its timing arrow. Either way, none means nothing to show.
	marker := []byte("\nDialogue:")
	if f == FormatSRT {
		marker = []byte(" --> ")
	}
	if !bytes.Contains(b, marker) {
		return fmt.Errorf("subs: %s produced no subtitle lines — "+
			"ffmpeg read the file but found nothing to show", name)
	}
	return nil
}

// toUTF8 makes a subtitle file readable. Korean .smi and .srt files are
// usually CP949; ffmpeg assumes UTF-8 and produces mojibake from them.
// Decoding here rather than with -sub_charenc keeps the guess in one place and
// out of a command line.
func toUTF8(raw []byte) ([]byte, error) {
	// UTF-16 has to be caught before anything else, because it slips through
	// every later check. Its ASCII text is one byte of content and one zero
	// byte per character, and zero is a perfectly valid UTF-8 byte — so
	// "<SAMI>" saved as UTF-16 is valid UTF-8 as far as Go is concerned, gets
	// passed through untouched, and reaches ffmpeg as "<\x00S\x00A\x00…",
	// which no demuxer recognises. It then reads the file, finds nothing, and
	// exits successfully.
	if enc, ok := utf16Encoding(raw); ok {
		decoded, _, err := transform.Bytes(enc.NewDecoder(), raw)
		if err != nil {
			return nil, fmt.Errorf("looks like UTF-16 but will not decode: %w", err)
		}
		return trimBOM(decoded), nil
	}

	raw = trimBOM(raw)
	if utf8.Valid(raw) {
		return raw, nil
	}
	// korean.EUCKR covers CP949/UHC, which is what these files really are.
	decoded, _, err := transform.Bytes(korean.EUCKR.NewDecoder(), raw)
	if err != nil {
		return nil, fmt.Errorf("not UTF-8 and not CP949: %w", err)
	}
	return decoded, nil
}

// utf16Encoding reports whether this is UTF-16, and which way round.
//
// A byte-order mark settles it. Without one, the giveaway is that half the
// bytes are zero: Korean and Latin text both sit in the low half of the
// plane, so every character carries a zero byte, and which side it falls on
// says the endianness.
func utf16Encoding(b []byte) (encoding.Encoding, bool) {
	if len(b) >= 2 {
		switch {
		case b[0] == 0xFF && b[1] == 0xFE:
			return unicode.UTF16(unicode.LittleEndian, unicode.ExpectBOM), true
		case b[0] == 0xFE && b[1] == 0xFF:
			return unicode.UTF16(unicode.BigEndian, unicode.ExpectBOM), true
		}
	}

	head := b
	if len(head) > 4096 {
		head = head[:4096]
	}
	if len(head) < 16 {
		return nil, false
	}
	var evenZero, oddZero int
	for i, c := range head {
		if c != 0 {
			continue
		}
		if i%2 == 0 {
			evenZero++
		} else {
			oddZero++
		}
	}
	// A third of one side being zero is not something text in any single-byte
	// encoding does.
	switch quarter := len(head) / 6; {
	case oddZero > quarter && evenZero <= quarter:
		return unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM), true
	case evenZero > quarter && oddZero <= quarter:
		return unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM), true
	}
	return nil, false
}

func trimBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}

// --- resolving ---

// Resolver answers the only question the converter asks: given this video and
// possibly a track the viewer chose, what file should be drawn into it?
//
// A folder conversion cannot ask per file, so only the first job carries a
// choice; the rest fall back to the Korean-first rule.
type Resolver struct {
	Finder   *Finder
	Preparer *Preparer
}

func (r *Resolver) Resolve(ctx context.Context, jobID string, rel outpath.Rel, preferredID, preferLang string, f Format) (string, func(), error) {
	noop := func() {}
	tracks := r.Finder.Find(rel)
	if len(tracks) == 0 {
		log.Printf("no subtitles found for %s, converting without", rel.Base())
		return "", noop, nil
	}

	var chosen Track
	var ok bool
	switch {
	case preferredID != "":
		chosen, ok = FindByID(tracks, preferredID)
	case preferLang != "":
		// Stay in the language that was chosen. Finding nothing here means
		// burning nothing, which is recoverable; burning the wrong language
		// is not.
		chosen, ok = PickLang(tracks, preferLang)
	default:
		chosen, ok = Pick(tracks)
	}
	if !ok {
		log.Printf("no usable subtitle for %s among %d track(s), converting without",
			rel.Base(), len(tracks))
		return "", noop, nil
	}
	if !chosen.Burnable() {
		return "", noop, fmt.Errorf("subs: %s cannot be burned in", chosen.Label)
	}
	// Which subtitle went into the picture is the thing most often wrong and
	// the thing hardest to check afterwards: it is drawn into the frames, so
	// the only other way to tell is to watch the file. Say it here.
	if f == FormatSRT {
		log.Printf("adding a subtitle track to %s: %s", rel.Base(), chosen.Label)
	} else {
		log.Printf("burning subtitles into %s: %s", rel.Base(), chosen.Label)
	}
	return r.Preparer.Prepare(ctx, jobID, rel, chosen.ID, f)
}
