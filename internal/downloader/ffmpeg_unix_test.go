//go:build unix

package downloader

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/combor/magnetowid/internal/provider"
)

// Terminal Ctrl+C reaches the whole foreground process group.
func TestFFmpegRunsInOwnProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	bin := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\necho $$ > '" + pidFile + "'\nexec sleep 60\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (&FFmpeg{Path: bin}).Download(ctx,
			provider.Stream{URL: "http://example.invalid/video.mp4"}, filepath.Join(dir, "out.mp4"), func(time.Duration, int64) {})
	}()
	defer func() {
		cancel()
		<-done
	}()

	var pid int
	for deadline := time.Now().Add(10 * time.Second); pid == 0; {
		if time.Now().After(deadline) {
			t.Fatal("fake ffmpeg never started")
		}
		b, _ := os.ReadFile(pidFile)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		time.Sleep(10 * time.Millisecond)
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatal(err)
	}
	if pgid != pid {
		t.Errorf("ffmpeg (pid %d) is in process group %d, not its own", pid, pgid)
	}
}

// fakeFFmpeg records its folder, arguments and proxy settings in log and
// creates its output.
func fakeFFmpeg(t *testing.T, dir string) (bin, log string) {
	t.Helper()
	bin, log = filepath.Join(dir, "ffmpeg"), filepath.Join(dir, "args")
	script := "#!/bin/sh\npwd > '" + log + "'\nfor a; do echo \"$a\"; out=$a; done >> '" + log + "'\n" +
		"echo \"http_proxy=$http_proxy no_proxy=$no_proxy\" >> '" + log + "'\n: > \"$out\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func TestFFmpegInputs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		media := r.URL.RawQuery == "media"
		switch {
		case r.URL.Path == "/1.ts":
			io.WriteString(w, "segment data")
		case !media:
			io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\n"+strings.TrimPrefix(r.URL.Path, "/")+"?media\n")
		case r.URL.Path == "/vod.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXTINF:4,\n1.ts\n#EXT-X-ENDLIST\n")
		default:
			io.WriteString(w, "#EXTM3U\n#EXTINF:4,\n1.ts\n")
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	_, log := fakeFFmpeg(t, dir)
	// A relative path must survive ffmpeg running in the segments folder.
	t.Chdir(dir)
	for _, tt := range []struct {
		path, dir, input string
	}{
		{"/vod.m3u8", filepath.Join(dir, segmentsDir), "v.m3u8"},
		{"/live.m3u8", dir, srv.URL + "/live.m3u8?media"},
	} {
		err := (&FFmpeg{Path: "./ffmpeg"}).Download(context.Background(), provider.Stream{URL: srv.URL + tt.path},
			filepath.Join(dir, "out.mp4"), func(time.Duration, int64) {})
		if err != nil {
			t.Fatalf("%s: %v", tt.path, err)
		}
		b, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(b), "\n")
		if lines[0] != tt.dir || !slices.Contains(lines, tt.input) {
			t.Errorf("%s: ffmpeg ran in %s with %q; want %s and input %s", tt.path, lines[0], lines[1:], tt.dir, tt.input)
		}
	}
}

// ffmpeg reads its proxy from the environment, which must match the stream's
// transport and not magnetowid's own settings.
func TestFFmpegFollowsStreamProxy(t *testing.T) {
	dir := t.TempDir()
	bin, log := fakeFFmpeg(t, dir)
	t.Setenv("http_proxy", "http://environment.example:3128")
	t.Setenv("no_proxy", "elsewhere.example")
	proxy, err := url.Parse("http://proxy.example:3128")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name      string
		transport http.RoundTripper
		want      string
	}{
		{"no transport", nil, "http_proxy=http://environment.example:3128 no_proxy=elsewhere.example"},
		{"proxy", &http.Transport{Proxy: http.ProxyURL(proxy)}, "http_proxy=http://proxy.example:3128 no_proxy="},
		{"direct", &http.Transport{}, "http_proxy= no_proxy="},
		{"no proxy for the stream", &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }},
			"http_proxy= no_proxy="},
	} {
		// Not a playlist, so ffmpeg fetches it.
		s := provider.Stream{URL: "http://example.invalid/video.mp4", Transport: tt.transport}
		if err := (&FFmpeg{Path: bin}).Download(context.Background(), s, filepath.Join(dir, "out.mp4"),
			func(time.Duration, int64) {}); err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		b, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		if lines := strings.Split(strings.TrimSpace(string(b)), "\n"); lines[len(lines)-1] != tt.want {
			t.Errorf("%s: ffmpeg ran with %q, want %q", tt.name, lines[len(lines)-1], tt.want)
		}
	}

	// An unknown proxy must not become a direct connection.
	broken := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return nil, errors.New("bad proxy") }}
	os.Remove(log)
	err = (&FFmpeg{Path: bin}).Download(context.Background(),
		provider.Stream{URL: "http://example.invalid/video.mp4", Transport: broken}, filepath.Join(dir, "out.mp4"),
		func(time.Duration, int64) {})
	if err == nil || !strings.Contains(err.Error(), "bad proxy") {
		t.Errorf("err = %v, want the proxy's", err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Errorf("ffmpeg ran without knowing its proxy: %v", err)
	}
}

// The stream's transport, e.g. its site's proxy, fetches playlists and segments.
func TestDownloadUsesStreamTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/master.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nvod.m3u8\n")
		case "/vod.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXTINF:4,\n1.ts\n#EXT-X-ENDLIST\n")
		default:
			io.WriteString(w, "segment data")
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	bin, _ := fakeFFmpeg(t, dir)
	var mu sync.Mutex
	var paths []string
	site := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		paths = append(paths, req.URL.Path)
		mu.Unlock()
		return http.DefaultTransport.RoundTrip(req)
	})
	// The engine's own client must stay unused.
	other := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Errorf("%s fetched without the stream's transport", req.URL.Path)
		return nil, errors.New("wrong transport")
	})}
	err := (&FFmpeg{Path: bin, Client: other}).Download(context.Background(),
		provider.Stream{URL: srv.URL + "/master.m3u8", Transport: site}, filepath.Join(dir, "out.mp4"),
		func(time.Duration, int64) {})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(paths)
	if want := []string{"/1.ts", "/master.m3u8", "/vod.m3u8"}; !slices.Equal(paths, want) {
		t.Errorf("the stream's transport fetched %q, want %q", paths, want)
	}
}

// ffmpeg 5.x reports unreadable inputs without "Error opening input".
func TestUnreadableSegmentsRemoved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/master.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nvod.m3u8\n")
		case "/vod.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXTINF:4,\n1.ts\n#EXT-X-ENDLIST\n")
		default:
			io.WriteString(w, "segment data")
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	bin := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\necho 'v.m3u8: Invalid data found when processing input' >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	err := (&FFmpeg{Path: bin}).Download(context.Background(), provider.Stream{URL: srv.URL + "/master.m3u8"},
		filepath.Join(dir, "out.mp4"), func(time.Duration, int64) {})
	if !errors.Is(err, errUnreadable) {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, segmentsDir)); !os.IsNotExist(err) {
		t.Errorf("unreadable segments kept: %v", err)
	}
}
