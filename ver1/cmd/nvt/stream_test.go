package main

import (
	"bytes"
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type stubInfo struct{ name string }

func (s stubInfo) Name() string       { return s.name }
func (s stubInfo) Size() int64        { return 0 }
func (s stubInfo) Mode() fs.FileMode  { return 0o444 }
func (s stubInfo) ModTime() time.Time { return time.Time{} }
func (s stubInfo) IsDir() bool        { return false }
func (s stubInfo) Sys() any           { return nil }
func (s stubInfo) ContentType(context.Context) (string, error) {
	return "video/x-matroska", nil
}

// The whole point of this path: a file whose size is not yet known must not
// announce one, because the announced size is what a player trusts.
func TestStreamUnsizedSendsNoContentLength(t *testing.T) {
	body := bytes.Repeat([]byte("abcdefgh"), 4096) // 32KB, larger than one read
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Movie.mkv", nil)

	streamUnsized(rec, r, stubInfo{"Movie.mkv"}, bytes.NewReader(body))

	res := rec.Result()
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
	if got := res.Header.Get("Content-Length"); got != "" {
		t.Errorf("Content-Length = %q, want it absent", got)
	}
	if got := res.Header.Get("Accept-Ranges"); got != "none" {
		t.Errorf("Accept-Ranges = %q, want none", got)
	}
	if got := res.Header.Get("Content-Type"); got != "video/x-matroska" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := rec.Body.Len(); got != len(body) {
		t.Errorf("delivered %d bytes, want %d", got, len(body))
	}
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Error("delivered bytes differ from the source")
	}
}

func TestStreamUnsizedHeadSendsNoBody(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodHead, "/Movie.mkv", nil)

	streamUnsized(rec, r, stubInfo{"Movie.mkv"}, strings.NewReader("payload"))

	if rec.Body.Len() != 0 {
		t.Errorf("HEAD returned %d bytes of body", rec.Body.Len())
	}
	if got := rec.Result().Header.Get("Content-Length"); got != "" {
		t.Errorf("Content-Length = %q, want it absent", got)
	}
}
