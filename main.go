package main

import (
	"context"
	"flag"
	"log/slog"
)

func main() {
	var (
		httpAddr   string
		configPath string
	)
	flag.StringVar(&httpAddr, "http", ":8080", "HTTP server address")
	flag.StringVar(&configPath, "config", "config.toml", "config path")
	flag.Parse()

	cfg, err := NewConfig(configPath)
	if err != nil {
		slog.Error("failed parsing config", slog.Any("err", err), slog.String("path", configPath))
		return
	}

	mediaLib := NewMediaLibrary(NewS3Storage(cfg.S3))
	ix := NewTagIndex(mediaLib.store, cfg.Index.indexConfig())
	mediaLib.SetIndex(ix)

	// Warm the tag index in the background so the first album search is fast
	// without delaying startup. The index falls back to per-request scans if
	// this fails.
	go func() {
		stats, err := ix.Refresh(context.Background())
		if err != nil {
			slog.Error("initial tag index scan failed", slog.Any("err", err))
			return
		}
		slog.Info("tag index ready",
			slog.Int("tracks", stats.Scanned),
			slog.Int("tagged", stats.Tagged),
			slog.Int("problems", stats.ProblemCount()),
			slog.Bool("truncated", stats.Truncated),
			slog.Duration("duration", stats.Duration),
		)
	}()

	slog.Info("started HTTP server", slog.String("address", httpAddr))
	err = StartServer(mediaLib, httpAddr)
	slog.Error("failed starting HTTP server", slog.Any("err", err))
}
