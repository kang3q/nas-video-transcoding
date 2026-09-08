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
	"syscall"
	"time"

	"nvt/ver2/internal/config"
	"nvt/ver2/internal/ffmpeg"
	"nvt/ver2/internal/jobs"
	"nvt/ver2/internal/library"
	"nvt/ver2/internal/mediainfo"
	"nvt/ver2/internal/outpath"
	"nvt/ver2/internal/web"
)

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

	mapper, err := outpath.NewMapper(cfg.SourceDir, cfg.OutputDir, cfg.OutputRel)
	if err != nil {
		log.Fatalf("library root: %v", err)
	}
	defer mapper.Close()

	prober := mediainfo.NewProber(cfg.FFprobeBin, cfg.ProbeTimeout, 4, cfg.StateDir)
	defer prober.Flush()

	queue := jobs.NewQueue(jobs.Deps{
		Mapper: mapper,
		Prober: prober,
		Runner: ffmpeg.NewExec(cfg.FFmpegBin),
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
		CheckpointAt: float64(cfg.CheckpointPercent) / 100,
	})

	srv, err := web.New(cfg, mapper, library.New(mapper), queue, prober)
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

	log.Printf("source=%s output=%s state=%s listen=%s",
		cfg.SourceDir, cfg.OutputDir, cfg.StateDir, cfg.Listen)
	if cfg.OutputRel != "" {
		log.Printf("output sits inside the library at %q and is hidden from browsing", cfg.OutputRel)
	}
	log.Printf("workers=%d threads=%d preset=%s v=%s a=%s checkpoint=%d%%",
		cfg.Workers, cfg.Threads, cfg.Preset, cfg.VideoBitrate, cfg.AudioBitrate, cfg.CheckpointPercent)

	go flushPeriodically(prober)

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Print("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpSrv.Shutdown(ctx)
	prober.Flush()
}

func flushPeriodically(p *mediainfo.Prober) {
	for range time.Tick(60 * time.Second) {
		p.Flush()
	}
}
