// Command magnetowid provides Newznab and SABnzbd APIs for VOD downloads.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/combor/magnetowid/internal/downloader"
	"github.com/combor/magnetowid/internal/newznab"
	"github.com/combor/magnetowid/internal/overrides"
	"github.com/combor/magnetowid/internal/probe"
	"github.com/combor/magnetowid/internal/provider"
	"github.com/combor/magnetowid/internal/provider/bbc"
	"github.com/combor/magnetowid/internal/provider/tvp"
	"github.com/combor/magnetowid/internal/sabnzbd"
	"github.com/combor/magnetowid/internal/store"
	"github.com/combor/magnetowid/internal/web"
)

// Set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	level := new(slog.LevelVar)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	if err := run(log, level); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, level *slog.LevelVar) error {
	listen := flag.String("listen", envOr("MAGNETOWID_LISTEN", ":8484"), "listen address")
	// Usage prints flag defaults, so settings that hold secrets, the API key and
	// a proxy URL's password, get theirs from the environment after parsing.
	apiKey := flag.String("api-key", "", "API key for both APIs (required)")
	downloadDir := flag.String("download-dir", os.Getenv("MAGNETOWID_DOWNLOAD_DIR"), "where finished downloads go (required)")
	categories := flag.String("categories", envOr("MAGNETOWID_CATEGORIES", "tv,movies"), "comma-separated download categories")
	ffmpeg := flag.String("ffmpeg", envOr("MAGNETOWID_FFMPEG", "ffmpeg"), "ffmpeg binary")
	logLevel := flag.String("log-level", envOr("MAGNETOWID_LOG_LEVEL", "info"), "log level: debug, info, warn or error")
	const proxyUsage = `'s HTTP proxy, e.g. http://127.0.0.1:8888, or "direct" for none (default: the environment's)`
	tvpProxy := flag.String("tvp-proxy", "", "TVP VOD"+proxyUsage)
	bbcProxy := flag.String("bbc-proxy", "", "BBC iPlayer"+proxyUsage)
	healthcheck := flag.Bool("healthcheck", false, "ask the magnetowid at the listen address whether it is healthy, and exit")
	flag.Parse()
	*apiKey = cmp.Or(*apiKey, os.Getenv("MAGNETOWID_API_KEY"))
	*tvpProxy = cmp.Or(*tvpProxy, os.Getenv("MAGNETOWID_TVP_PROXY"))
	*bbcProxy = cmp.Or(*bbcProxy, os.Getenv("MAGNETOWID_BBC_PROXY"))

	if *healthcheck {
		return checkHealth(*listen)
	}
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("-log-level: %w", err)
	}
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

	// Close after the server and worker stop writing.
	db, err := store.Open(dir)
	if err != nil {
		return err
	}
	defer db.Close()

	// Each site streams to its own country, so each has its own connection.
	tvpClient, err := siteClient(*tvpProxy)
	if err != nil {
		return fmt.Errorf("-tvp-proxy: %w", err)
	}
	bbcClient, err := siteClient(*bbcProxy)
	if err != nil {
		return fmt.Errorf("-bbc-proxy: %w", err)
	}
	tvpProvider, err := tvp.New(tvpClient, log, db)
	if err != nil {
		return err
	}
	bbcProvider, err := bbc.New(bbcClient, log, db)
	if err != nil {
		return err
	}
	providers := provider.NewRegistry(
		tvpProvider,
		bbcProvider,
	)
	overrideStore, err := overrides.Open(db, providers)
	if err != nil {
		return err
	}

	queue, err := downloader.New(dir, db, providers, &downloader.FFmpeg{Path: *ffmpeg, Log: log}, log)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	// Streams bring their site's connection.
	prober := &probe.Prober{Client: &http.Client{Timeout: siteTimeout}}
	mux.Handle("/{provider}/api", &newznab.Handler{Providers: providers, APIKey: *apiKey, Probe: prober, Log: log})
	mux.Handle("/api", &sabnzbd.Handler{Queue: queue, APIKey: *apiKey, Categories: cats, Log: log})
	(&overrides.Handler{Store: overrideStore, APIKey: *apiKey, Log: log}).Register(mux)
	(&web.Handler{Queue: queue, Overrides: overrideStore, Providers: providers, Categories: cats, APIKey: *apiKey,
		Version: version, Log: log}).Register(mux)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "OK\n") })

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
	log.Info("magnetowid listening", "version", version, "addr", *listen, "download_dir", dir, "providers", providers.Names(),
		"tvp_proxy", proxyName(*tvpProxy), "bbc_proxy", proxyName(*bbcProxy))

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

// Omit query strings to keep API keys out of logs, and health checks to reduce noise.
func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			q := r.URL.Query()
			log.Debug("request", "method", r.Method, "path", r.URL.Path, "t", q.Get("t"), "mode", q.Get("mode"))
		}
		next.ServeHTTP(w, r)
	})
}

const (
	siteTimeout = 30 * time.Second
	// Skips the environment's proxy.
	directProxy = "direct"
)

// siteClient returns the client for a site's API and streams. proxy is an HTTP
// proxy's URL or "direct". Left empty, the client has no transport of its own:
// the site and its streams follow the environment's HTTPS_PROXY and NO_PROXY,
// which choose by host, and ffmpeg applies the environment's rules itself.
func siteClient(proxy string) (*http.Client, error) {
	client := &http.Client{Timeout: siteTimeout}
	if proxy == "" {
		return client, nil
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	// Keep a connection for each of a download's segment workers.
	t.MaxIdleConnsPerHost = 4
	t.Proxy = nil
	if proxy != directProxy {
		// ffmpeg, which fetches some streams itself, tunnels through nothing else.
		u, err := url.Parse(proxy)
		if err != nil || u.Scheme != "http" || u.Host == "" {
			// The URL may hold a password.
			return nil, fmt.Errorf("want an http:// proxy URL or %q", directProxy)
		}
		t.Proxy = http.ProxyURL(u)
	}
	client.Transport = t
	return client, nil
}

// proxyName describes a valid proxy setting for the log, without its password.
func proxyName(proxy string) string {
	switch proxy {
	case "":
		return "environment"
	case directProxy:
		return proxy
	}
	u, err := url.Parse(proxy)
	if err != nil {
		return "invalid"
	}
	return u.Redacted()
}

func checkHealth(listen string) error {
	u, err := healthURL(listen)
	if err != nil {
		return err
	}
	// Bypass proxies configured for VOD sites.
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Get(u)
	if err != nil {
		return fmt.Errorf("health check: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check: HTTP %d", resp.StatusCode)
	}
	return nil
}

// Use loopback when listening on all addresses.
func healthURL(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("-listen: %w", err)
	}
	switch ip := net.ParseIP(host); {
	case host == "" || (ip != nil && ip.Equal(net.IPv4zero)):
		host = "127.0.0.1"
	case ip != nil && ip.IsUnspecified():
		host = "::1"
	}
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, port), Path: "/health"}).String(), nil
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
