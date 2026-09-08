// Package cache stores converted output on disk.
//
// A conversion is identified by the source file's identity plus the encoding
// settings, so changing e.g. the audio bitrate naturally produces a new entry
// instead of serving a stale one. Completion is recorded with a marker file;
// a ".mkv" without its marker is an interrupted job and gets discarded.
package cache

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Cache struct {
	dir string
	mu  sync.Mutex
}

func New(dir string) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	c := &Cache{dir: dir}
	c.sweepIncomplete()
	return c, nil
}

// Key builds a stable identifier from the source file and the encoding
// parameters that will be applied to it.
func Key(srcPath string, size int64, modUnix int64, parts ...string) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s|%d|%d", srcPath, size, modUnix)
	for _, p := range parts {
		fmt.Fprintf(h, "|%s", p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (c *Cache) Path(key string) string   { return filepath.Join(c.dir, key+".mkv") }
func (c *Cache) marker(key string) string { return filepath.Join(c.dir, key+".done") }

// Complete reports whether a finished, fully written output exists.
func (c *Cache) Complete(key string) bool {
	if _, err := os.Stat(c.marker(key)); err != nil {
		return false
	}
	_, err := os.Stat(c.Path(key))
	return err == nil
}

// Size returns the current byte count of the output, which for a running job
// grows over time.
func (c *Cache) Size(key string) int64 {
	fi, err := os.Stat(c.Path(key))
	if err != nil {
		return 0
	}
	return fi.Size()
}

func (c *Cache) MarkComplete(key string) error {
	return os.WriteFile(c.marker(key), []byte(time.Now().UTC().Format(time.RFC3339)), 0o644)
}

// Touch records an access, which is what eviction orders by.
func (c *Cache) Touch(key string) {
	now := time.Now()
	os.Chtimes(c.marker(key), now, now)
}

func (c *Cache) Discard(key string) {
	os.Remove(c.Path(key))
	os.Remove(c.marker(key))
}

// sweepIncomplete removes outputs left behind by a job that died mid-write.
func (c *Cache) sweepIncomplete() {
	ents, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".mkv") {
			continue
		}
		key := strings.TrimSuffix(name, ".mkv")
		if !c.Complete(key) {
			os.Remove(filepath.Join(c.dir, name))
		}
	}
}

// Evict deletes least-recently-used entries until the total fits maxBytes.
// Only completed entries are eligible; a running job is never touched.
func (c *Cache) Evict(maxBytes int64) {
	if maxBytes <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	type ent struct {
		key  string
		size int64
		used time.Time
	}
	var ents []ent
	var total int64

	dirEnts, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	for _, e := range dirEnts {
		if !strings.HasSuffix(e.Name(), ".mkv") {
			continue
		}
		key := strings.TrimSuffix(e.Name(), ".mkv")
		fi, err := e.Info()
		if err != nil {
			continue
		}
		total += fi.Size()
		if !c.Complete(key) {
			continue // in flight
		}
		used := fi.ModTime()
		if mfi, err := os.Stat(c.marker(key)); err == nil {
			used = mfi.ModTime()
		}
		ents = append(ents, ent{key, fi.Size(), used})
	}

	if total <= maxBytes {
		return
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].used.Before(ents[j].used) })
	for _, e := range ents {
		if total <= maxBytes {
			return
		}
		c.Discard(e.key)
		total -= e.size
	}
}

// Total reports the number of bytes currently held.
func (c *Cache) Total() int64 {
	var total int64
	ents, err := os.ReadDir(c.dir)
	if err != nil {
		return 0
	}
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".mkv") {
			continue
		}
		if fi, err := e.Info(); err == nil {
			total += fi.Size()
		}
	}
	return total
}
