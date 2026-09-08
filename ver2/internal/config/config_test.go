package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestLoadDefaults(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "media")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	withEnv(t, map[string]string{
		"NVT2_SOURCE_DIR": src,
		"NVT2_OUTPUT_DIR": filepath.Join(base, "out"),
		"NVT2_STATE_DIR":  filepath.Join(base, "state"),
	})

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Workers != 1 {
		t.Errorf("Workers = %d, want 1: a second encode only makes both late", c.Workers)
	}
	if c.CheckpointPercent != 10 {
		t.Errorf("CheckpointPercent = %d, want 10", c.CheckpointPercent)
	}
	if c.OutputRel != "" {
		t.Errorf("OutputRel = %q, want empty when the output is outside the library", c.OutputRel)
	}
	// The output and state directories are created; the source is not, because
	// creating an empty one would hide a mistyped mount.
	for _, d := range []string{c.OutputDir, c.StateDir} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Errorf("%s was not created", d)
		}
	}
}

// Putting the output inside the share is the natural thing to do on a NAS, so
// it is allowed — but it has to be recorded, or the library lists its own
// output and offers to convert it again.
func TestLoadRecordsOutputNestedInsideSource(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "downloads")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	withEnv(t, map[string]string{
		"NVT2_SOURCE_DIR": src,
		"NVT2_OUTPUT_DIR": filepath.Join(src, "_nvt"),
		"NVT2_STATE_DIR":  filepath.Join(base, "state"),
	})

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.OutputRel != "_nvt" {
		t.Errorf("OutputRel = %q, want %q", c.OutputRel, "_nvt")
	}
}

func TestLoadRejectsBadDirectoryRelationships(t *testing.T) {
	t.Run("same directory", func(t *testing.T) {
		base := t.TempDir()
		dir := filepath.Join(base, "media")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		withEnv(t, map[string]string{
			"NVT2_SOURCE_DIR": dir,
			"NVT2_OUTPUT_DIR": dir,
			"NVT2_STATE_DIR":  filepath.Join(base, "state"),
		})
		if _, err := Load(); err == nil {
			t.Error("source and output pointing at one directory was accepted")
		}
	})

	// Reading from inside the output tree would feed one job's result to the
	// next as input.
	t.Run("source inside output", func(t *testing.T) {
		base := t.TempDir()
		out := filepath.Join(base, "out")
		src := filepath.Join(out, "media")
		if err := os.MkdirAll(src, 0o755); err != nil {
			t.Fatal(err)
		}
		withEnv(t, map[string]string{
			"NVT2_SOURCE_DIR": src,
			"NVT2_OUTPUT_DIR": out,
			"NVT2_STATE_DIR":  filepath.Join(base, "state"),
		})
		_, err := Load()
		if err == nil {
			t.Fatal("a source nested inside the output was accepted")
		}
		if !strings.Contains(err.Error(), "inside output") {
			t.Errorf("err = %v, want it to name the nesting", err)
		}
	})
}

func TestLoadRejectsAMissingSource(t *testing.T) {
	base := t.TempDir()
	withEnv(t, map[string]string{
		"NVT2_SOURCE_DIR": filepath.Join(base, "does-not-exist"),
		"NVT2_OUTPUT_DIR": filepath.Join(base, "out"),
		"NVT2_STATE_DIR":  filepath.Join(base, "state"),
	})
	if _, err := Load(); err == nil {
		t.Error("a missing source directory was accepted")
	}
}

func TestLoadRejectsASourceThatIsAFile(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	withEnv(t, map[string]string{
		"NVT2_SOURCE_DIR": file,
		"NVT2_OUTPUT_DIR": filepath.Join(base, "out"),
		"NVT2_STATE_DIR":  filepath.Join(base, "state"),
	})
	if _, err := Load(); err == nil {
		t.Error("a regular file was accepted as the library root")
	}
}

func TestLoadClampsNonsenseValues(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "media")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	withEnv(t, map[string]string{
		"NVT2_SOURCE_DIR":         src,
		"NVT2_OUTPUT_DIR":         filepath.Join(base, "out"),
		"NVT2_STATE_DIR":          filepath.Join(base, "state"),
		"NVT2_WORKERS":            "0",
		"NVT2_THREADS":            "-4",
		"NVT2_CHECKPOINT_PERCENT": "500",
	})
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Workers != 1 || c.Threads != 0 || c.CheckpointPercent != 10 {
		t.Errorf("Workers=%d Threads=%d Checkpoint=%d, want 1/0/10",
			c.Workers, c.Threads, c.CheckpointPercent)
	}
}
