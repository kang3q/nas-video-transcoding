package vfs

import (
	"testing"
	"time"
)

// Watching one file, however many requests it takes, is never a sweep.
func TestScanDetectorIgnoresRepeatsOfOneFile(t *testing.T) {
	d := newScanDetector(time.Minute, 3)
	for i := 0; i < 10; i++ {
		if d.Touch("/Show/ep01.mkv") {
			t.Fatalf("one file called a sweep on request %d", i+1)
		}
	}
}

func TestScanDetectorFlagsBreadth(t *testing.T) {
	d := newScanDetector(time.Minute, 3)
	files := []string{"/S/1.mkv", "/S/2.mkv", "/S/3.mkv", "/S/4.mkv", "/S/5.mkv"}
	var flagged int
	for _, f := range files {
		if d.Touch(f) {
			flagged++
		}
	}
	// The first three are within the limit; everything past it is a sweep.
	if flagged != 2 {
		t.Errorf("flagged %d of %d files, want 2", flagged, len(files))
	}
}

// A sweep that has gone quiet must not keep a later playback out.
func TestScanDetectorForgetsOldTraffic(t *testing.T) {
	d := newScanDetector(30*time.Millisecond, 2)
	for _, f := range []string{"/S/1.mkv", "/S/2.mkv", "/S/3.mkv", "/S/4.mkv"} {
		d.Touch(f)
	}
	time.Sleep(60 * time.Millisecond)
	if d.Touch("/S/9.mkv") {
		t.Error("a lone request after the window still counted as a sweep")
	}
}

func TestScanDetectorDisabled(t *testing.T) {
	d := newScanDetector(time.Minute, 0)
	for i, f := range []string{"/a", "/b", "/c", "/d", "/e", "/f"} {
		if d.Touch(f) {
			t.Fatalf("detection should be off, flagged at %d", i)
		}
	}
}
