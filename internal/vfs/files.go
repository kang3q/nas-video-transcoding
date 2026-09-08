package vfs

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// info is a synthetic FileInfo: the name and size a client should see, which
// for a converted file differ from anything on disk.
type info struct {
	name string
	size int64
	mode fs.FileMode
	mod  time.Time
	dir  bool
}

// ContentType satisfies webdav.ContentTyper, and that matters far more than
// it looks. Without it, webdav resolves getcontenttype by *opening* the file —
// one more open per listed entry, on top of the one props() already does.
//
// The table exists because Go's mime package has no built-in entry for the
// container formats we serve, and a minimal container image carries no
// /etc/mime.types to fall back on.
func (i *info) ContentType(context.Context) (string, error) {
	ext := strings.ToLower(filepath.Ext(i.name))
	if ct, ok := contentTypes[ext]; ok {
		return ct, nil
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct, nil
	}
	return "application/octet-stream", nil
}

var contentTypes = map[string]string{
	".mkv": "video/x-matroska", ".mp4": "video/mp4", ".m4v": "video/x-m4v",
	".avi": "video/x-msvideo", ".mov": "video/quicktime", ".webm": "video/webm",
	".ts": "video/mp2t", ".m2ts": "video/mp2t", ".mts": "video/mp2t",
	".mpg": "video/mpeg", ".mpeg": "video/mpeg", ".m2v": "video/mpeg",
	".wmv": "video/x-ms-wmv", ".asf": "video/x-ms-asf", ".flv": "video/x-flv",
	".vob": "video/dvd", ".3gp": "video/3gpp", ".rmvb": "application/vnd.rn-realmedia-vbr",

	".srt": "application/x-subrip", ".ass": "text/x-ssa", ".ssa": "text/x-ssa",
	".vtt": "text/vtt", ".sub": "text/plain", ".idx": "text/plain",

	".mp3": "audio/mpeg", ".flac": "audio/flac", ".m4a": "audio/mp4",
	".aac": "audio/aac", ".ogg": "audio/ogg", ".wav": "audio/wav",

	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png",
	".nfo": "text/plain",
}

func (i *info) Name() string       { return i.name }
func (i *info) Size() int64        { return i.size }
func (i *info) Mode() fs.FileMode  { return i.mode }
func (i *info) ModTime() time.Time { return i.mod }
func (i *info) IsDir() bool        { return i.dir }
func (i *info) Sys() any           { return nil }

// namedFile serves a real file on disk under a different advertised name.
type namedFile struct {
	*os.File
	fi fs.FileInfo
}

func (n *namedFile) Stat() (fs.FileInfo, error) { return n.fi, nil }
func (n *namedFile) Write([]byte) (int, error)  { return 0, errReadOnly }

// metaFile answers property lookups without touching content. WebDAV opens
// every file it lists; this is what it gets.
type metaFile struct {
	fi fs.FileInfo
}

func (m *metaFile) Close() error               { return nil }
func (m *metaFile) Stat() (fs.FileInfo, error) { return m.fi, nil }
func (m *metaFile) Write([]byte) (int, error)  { return 0, errReadOnly }
func (m *metaFile) Read([]byte) (int, error) {
	return 0, errors.New("nvt: metadata handle carries no content")
}
func (m *metaFile) Readdir(int) ([]fs.FileInfo, error) {
	return nil, errors.New("nvt: not a directory")
}

func (m *metaFile) Seek(offset int64, whence int) (int64, error) {
	if whence == io.SeekEnd {
		return m.fi.Size() + offset, nil
	}
	return offset, nil
}

// dirFile answers directory listings from a pre-resolved snapshot.
type dirFile struct {
	self    fs.FileInfo
	entries []fs.FileInfo
	pos     int
}

func (d *dirFile) Close() error               { return nil }
func (d *dirFile) Read([]byte) (int, error)   { return 0, errors.New("nvt: is a directory") }
func (d *dirFile) Write([]byte) (int, error)  { return 0, errReadOnly }
func (d *dirFile) Stat() (fs.FileInfo, error) { return d.self, nil }
func (d *dirFile) Seek(int64, int) (int64, error) {
	return 0, errors.New("nvt: cannot seek a directory")
}

func (d *dirFile) Readdir(count int) ([]fs.FileInfo, error) {
	if count <= 0 {
		rest := d.entries[d.pos:]
		d.pos = len(d.entries)
		return rest, nil
	}
	if d.pos >= len(d.entries) {
		return nil, io.EOF
	}
	end := d.pos + count
	if end > len(d.entries) {
		end = len(d.entries)
	}
	out := d.entries[d.pos:end]
	d.pos = end
	return out, nil
}

// Incomplete marks a file whose content is still being produced. Its final
// size is unknown, so it must not be served with a Content-Length: guessing
// low truncates playback, guessing high hangs the client waiting for bytes
// that never arrive.
type Incomplete interface {
	Incomplete() bool
}

// growingFile streams a conversion that is still being written. Reads block at
// the write frontier instead of reporting EOF, which works because ffmpeg runs
// far faster than playback. This is the fallback path: the normal one waits
// for the job and serves a finished file, which seeks properly.
type growingFile struct {
	f   *os.File
	job interface {
		Finished() bool
		Err() error
		Done() <-chan struct{}
	}
	fi  *info
	pos int64
}

const growPoll = 250 * time.Millisecond

func (g *growingFile) Read(p []byte) (int, error) {
	for {
		n, err := g.f.Read(p)
		if n > 0 {
			g.pos += int64(n)
			return n, nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if g.job.Finished() {
			// Catch anything flushed between the read and the check.
			n, err = g.f.Read(p)
			if n > 0 {
				g.pos += int64(n)
				return n, nil
			}
			if jerr := g.job.Err(); jerr != nil {
				return 0, jerr
			}
			return 0, io.EOF
		}
		select {
		case <-g.job.Done():
		case <-time.After(growPoll):
		}
	}
}

func (g *growingFile) Seek(offset int64, whence int) (int64, error) {
	if whence == io.SeekEnd {
		// The real end is unknown until the job finishes; fall back to the
		// estimated size so callers that probe for length get an answer.
		if g.job.Finished() {
			if fi, err := g.f.Stat(); err == nil {
				g.fi.size = fi.Size()
			}
		}
		pos, err := g.f.Seek(g.fi.size+offset, io.SeekStart)
		g.pos = pos
		return pos, err
	}
	pos, err := g.f.Seek(offset, whence)
	if err != nil {
		return 0, err
	}
	g.pos = pos
	return pos, nil
}

func (g *growingFile) Incomplete() bool           { return !g.job.Finished() }
func (g *growingFile) Close() error               { return g.f.Close() }
func (g *growingFile) Write([]byte) (int, error)  { return 0, errReadOnly }
func (g *growingFile) Stat() (fs.FileInfo, error) { return g.fi, nil }
func (g *growingFile) Readdir(int) ([]fs.FileInfo, error) {
	return nil, errors.New("nvt: not a directory")
}
