// Command vodarr serves VOD sites to Sonarr and Radarr as Newznab indexers
// and a SABnzbd download client.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/combor/vodarr/internal/downloader"
	"github.com/combor/vodarr/internal/newznab"
	"github.com/combor/vodarr/internal/probe"
	"github.com/combor/vodarr/internal/provider"
	"github.com/combor/vodarr/internal/provider/tvp"
	"github.com/combor/vodarr/internal/sabnzbd"
	"github.com/combor/vodarr/internal/store"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	listen := flag.String("listen", envOr("VODARR_LISTEN", ":8484"), "listen address")
	apiKey := flag.String("api-key", os.Getenv("VODARR_API_KEY"), "API key for both APIs (required)")
	downloadDir := flag.String("download-dir", os.Getenv("VODARR_DOWNLOAD_DIR"), "where finished downloads go (required)")
	categories := flag.String("categories", envOr("VODARR_CATEGORIES", "tv,movies"), "comma-separated download categories")
	ffmpeg := flag.String("ffmpeg", envOr("VODARR_FFMPEG", "ffmpeg"), "ffmpeg binary")
	flag.Parse()

	if *apiKey == "" || *downloadDir == "" {
		flag.Usage()
		return errors.New("-api-key and -download-dir are required")
	}
	dir, err := filepath.Abs(*downloadDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	cats := splitList(*categories)
	// Sonarr/Radarr's health check expects the category folders to exist.
	for _, c := range cats {
		if err := os.MkdirAll(filepath.Join(dir, downloader.SanitizeName(c)), 0o777); err != nil {
			return err
		}
	}
	if _, err := exec.LookPath(*ffmpeg); err != nil {
		return fmt.Errorf("ffmpeg not found: %w", err)
	}

	// Closed last, after the server and the worker have stopped writing.
	db, err := store.Open(dir)
	if err != nil {
		return err
	}
	defer db.Close()

	// Add new sites here.
	httpClient := &http.Client{Timeout: 30 * time.Second}
	tvpProvider, err := tvp.New(httpClient, log, db)
	if err != nil {
		return err
	}
	providers := provider.NewRegistry(
		tvpProvider,
	)

	queue, err := downloader.New(dir, db, providers, &downloader.FFmpeg{Path: *ffmpeg}, log)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	prober := &probe.Prober{Client: httpClient}
	mux.Handle("/{provider}/api", &newznab.Handler{Providers: providers, APIKey: *apiKey, Probe: prober, Log: log})
	mux.Handle("/api", &sabnzbd.Handler{Queue: queue, APIKey: *apiKey, Categories: cats, Log: log})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		queue.Run(ctx)
	}()

	srv := &http.Server{Addr: *listen, Handler: logRequests(log, mux), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("vodarr listening", "version", version, "addr", *listen, "download_dir", dir, "providers", providers.Names())

	var serveErr error
	select {
	case serveErr = <-errc:
	case <-ctx.Done():
		log.Info("shutting down")
	}
	stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && serveErr == nil {
		serveErr = err
	}
	// Wait for the worker so ffmpeg isn't orphaned.
	select {
	case <-workerDone:
	case <-time.After(30 * time.Second):
		log.Warn("download worker did not stop in time")
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		return nil
	}
	return serveErr
}

// logRequests logs requests without the query string, which holds the API key.
func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		log.Debug("request", "method", r.Method, "path", r.URL.Path, "t", q.Get("t"), "mode", q.Get("mode"))
		next.ServeHTTP(w, r)
	})
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
