// Command nvtweb converts NAS video to MP4, driven by a web page rather than
// by guessing what a media player is about to do.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"

	"nvt/ver2/internal/config"
	"nvt/ver2/internal/ffmpeg"
	"nvt/ver2/internal/history"
	"nvt/ver2/internal/jobs"
	"nvt/ver2/internal/library"
	"nvt/ver2/internal/mediainfo"
	"nvt/ver2/internal/notify"
	"nvt/ver2/internal/outpath"
	"nvt/ver2/internal/subs"
	"nvt/ver2/internal/web"
)

// version is stamped at build time (-X main.version=...). It answers "is the
// container running the code I just pushed?", which is not a question the
// behaviour on screen can be trusted to answer.
var version = "dev"

// buildVersion prefers the stamp and falls back to whatever the toolchain
// recorded, so a plain "go build" still says something useful.
func buildVersion() string {
	if version != "dev" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return version
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("nvt2 ")

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	liveRoot := filepath.Join(cfg.StateDir, "live")
	// Anything half-written belongs to a process that is no longer running.
	jobs.Sweep(cfg.OutputDir, liveRoot)
	os.RemoveAll(filepath.Join(cfg.StateDir, "subs"))

	mapper, err := outpath.NewMapper(cfg.SourceDir, cfg.OutputDir, cfg.OutputRel)
	if err != nil {
		log.Fatalf("library root: %v", err)
	}
	defer mapper.Close()

	prober := mediainfo.NewProber(cfg.FFprobeBin, cfg.ProbeTimeout, 4, cfg.StateDir)
	defer prober.Flush()

	subFinder := subs.NewFinder(mapper, prober)
	subResolver := &subs.Resolver{
		Finder: subFinder,
		Preparer: &subs.Preparer{
			Mapper:  mapper,
			FFmpeg:  cfg.FFmpegBin,
			TempDir: filepath.Join(cfg.StateDir, "subs"),
		},
	}

	queue := jobs.NewQueue(jobs.Deps{
		Mapper: mapper,
		Prober: prober,
		Runner: ffmpeg.NewExec(cfg.FFmpegBin),
		Subs:   subResolver,
		Settings: ffmpeg.Settings{
			Threads:       cfg.Threads,
			Preset:        cfg.Preset,
			VideoBitrate:  cfg.VideoBitrate,
			AudioBitrate:  cfg.AudioBitrate,
			AudioChannels: cfg.AudioChannels,
			AudioRate:     cfg.AudioRate,
		},
		Workers:      cfg.Workers,
		LiveRoot:     liveRoot,
		SegmentSecs:  cfg.SegmentSecs,
		StateDir:     cfg.StateDir,
		CheckpointAt: float64(cfg.CheckpointPercent) / 100,
	})
	// Whatever was still outstanding when this last stopped. A batch is a
	// night's work; a restart should not mean reconstructing it by hand.
	queue.Restore()

	tg := notify.NewTelegram(cfg.TelegramToken, cfg.TelegramChat)
	watcher := notify.NewWatcher(queue, tg, cfg.PublicBaseURL)
	notifyCtx, stopNotify := context.WithCancel(context.Background())
	defer stopNotify()
	go watcher.Run(notifyCtx)

	watched := history.New(cfg.StateDir)
	srv, err := web.New(cfg, mapper, library.New(mapper), queue, prober, subFinder, watched)
	if err != nil {
		log.Fatalf("web: %v", err)
	}

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: responses here are whole films and event streams
		// that stay open for hours.
	}

	log.Printf("build %s", buildVersion())
	log.Printf("source=%s output=%s state=%s listen=%s",
		cfg.SourceDir, cfg.OutputDir, cfg.StateDir, cfg.Listen)
	if cfg.OutputRel != "" {
		log.Printf("output sits inside the library at %q and is hidden from browsing", cfg.OutputRel)
	}
	log.Printf("workers=%d threads=%d preset=%s v=%s a=%s checkpoint=%d%%",
		cfg.Workers, cfg.Threads, cfg.Preset, cfg.VideoBitrate, cfg.AudioBitrate, cfg.CheckpointPercent)
	log.Printf("live preview=%v segment=%ds", cfg.Live, cfg.SegmentSecs)
	if cfg.User != "" {
		log.Printf("basic auth: on (user %q). Media links are signed so AirPlay "+
			"still works: the Apple TV fetches the file itself and has no "+
			"password to send.", cfg.User)
	} else {
		log.Print("basic auth: off")
	}
	if tg.Enabled() {
		log.Printf("telegram: on, public url=%q", cfg.PublicBaseURL)
		if cfg.PublicBaseURL == "" {
			log.Print("telegram: NVT2_PUBLIC_URL is unset, so notifications carry no link")
		}
	} else {
		log.Print("telegram: off — NVT2_TELEGRAM_TOKEN and NVT2_TELEGRAM_CHAT_ID are unset. " +
			"With compose they come from a .env in the same folder as the compose file.")
	}

	go flushPeriodically(prober, watched)

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Print("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stopNotify()
	httpSrv.Shutdown(ctx)
	// Write down what is left before interrupting anything, so a conversion
	// that was running is recorded as still to do rather than as cancelled.
	queue.Shutdown(ctx)
	prober.Flush()
	watched.Flush()
}

func flushPeriodically(p *mediainfo.Prober, h *history.Store) {
	for range time.Tick(60 * time.Second) {
		p.Flush()
		h.Flush()
	}
}
