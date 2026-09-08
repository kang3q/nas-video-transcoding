package ffmpeg

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Runner runs one conversion. It is an interface so the queue can be tested
// without ffmpeg installed, which matters: the queue's job is scheduling, and
// scheduling bugs are much easier to provoke with a fake process than a real
// forty-minute encode.
type Runner interface {
	Run(ctx context.Context, spec Spec, set Settings, onProgress func(Progress)) error
}

// Exec runs the real thing.
type Exec struct {
	Bin string
	// StopGrace is how long ffmpeg is given to finish writing after being
	// asked to stop.
	StopGrace time.Duration
}

func NewExec(bin string) Exec { return Exec{Bin: bin, StopGrace: 10 * time.Second} }

func (e Exec) Run(ctx context.Context, spec Spec, set Settings, onProgress func(Progress)) error {
	cmd := exec.CommandContext(ctx, e.Bin, Args(spec, set)...)

	// CommandContext kills on cancel by default. ffmpeg killed outright leaves
	// an MP4 with no index and an HLS playlist with no end marker; interrupted,
	// it writes both and exits. WaitDelay is the backstop if it ignores us.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	grace := e.StopGrace
	if grace <= 0 {
		grace = 10 * time.Second
	}
	cmd.WaitDelay = grace

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	errTail := &tail{limit: 40}
	cmd.Stderr = errTail

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if onProgress != nil {
			ParseProgress(stdout, onProgress)
			return
		}
		io.Copy(io.Discard, stdout)
	}()
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("ffmpeg: %w: %s", err, errTail.String())
	}
	return nil
}

// tail keeps only the last few lines of stderr. A failing ffmpeg can produce a
// great deal of it, and only the end explains anything.
type tail struct {
	mu    sync.Mutex
	limit int
	lines []string
	buf   strings.Builder
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if !strings.Contains(t.buf.String(), "\n") {
		return len(p), nil
	}
	sc := bufio.NewScanner(strings.NewReader(t.buf.String()))
	t.buf.Reset()
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		t.lines = append(t.lines, line)
	}
	if len(t.lines) > t.limit {
		t.lines = t.lines[len(t.lines)-t.limit:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, "; ")
}
