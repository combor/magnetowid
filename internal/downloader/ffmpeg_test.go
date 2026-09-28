package downloader

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/combor/magnetowid/internal/provider"
)

// makeHLS writes a TVP-like HLS stream into dir: fMP4 video variants
// stream_0 (160x120) and stream_1 (80x60), audio rendition stream_2.
func makeHLS(t *testing.T, dir string) {
	t.Helper()
	encoders, _ := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
	if !strings.Contains(string(encoders), "libx264") {
		t.Skip("ffmpeg lacks libx264")
	}
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=10:duration=3",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-filter_complex", "[0:v]split=2[hi][lo];[lo]scale=80:60[lo2]",
		"-map", "[hi]", "-map", "[lo2]", "-map", "1:a",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-g", "10", "-c:a", "aac",
		"-f", "hls", "-hls_time", "1", "-hls_playlist_type", "vod", "-hls_segment_type", "fmp4",
		"-hls_fmp4_init_filename", "init_%v.mp4",
		"-var_stream_map", "v:0,agroup:aud v:1,agroup:aud a:0,agroup:aud",
		"-master_pl_name", "master.m3u8",
		filepath.Join(dir, "stream_%v.m3u8"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating HLS: %v\n%s", err, out)
	}
}

func TestFFmpegDownloadsHLS(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	src := t.TempDir()
	makeHLS(t, src)
	var mu sync.Mutex
	var paths []string
	badUA := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		if r.UserAgent() != "magnetowid-test" {
			badUA = r.UserAgent()
		}
		mu.Unlock()
		http.FileServer(http.Dir(src)).ServeHTTP(w, r)
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "out.mp4")
	var lastDone time.Duration
	var lastBytes int64
	err := (&FFmpeg{}).Download(context.Background(),
		provider.Stream{URL: srv.URL + "/master.m3u8", Header: http.Header{"User-Agent": {"magnetowid-test"}}},
		out, func(done time.Duration, bytes int64) { lastDone, lastBytes = done, bytes })
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if badUA != "" {
		t.Errorf("request with User-Agent %q", badUA)
	}
	// Only the best variant and the audio rendition are fetched.
	for _, p := range paths {
		if strings.Contains(p, "_1") {
			t.Errorf("lower variant fetched: %s", p)
		}
	}
	if !strings.Contains(strings.Join(paths, " "), "/stream_2.m3u8") {
		t.Errorf("audio rendition not fetched: %v", paths)
	}
	if lastDone < 2*time.Second || lastBytes == 0 {
		t.Errorf("progress done = %v, bytes = %d", lastDone, lastBytes)
	}
	probe, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type",
		"-of", "csv=p=0", out).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(probe)); len(got) != 2 {
		t.Errorf("streams = %v, want video and audio", got)
	}
}

func TestFFmpegReportsErrors(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	out := filepath.Join(t.TempDir(), "out.mp4")
	noop := func(time.Duration, int64) {}

	// A missing playlist fails in magnetowid, anything else in ffmpeg.
	err := (&FFmpeg{}).Download(context.Background(), provider.Stream{URL: srv.URL + "/missing.m3u8"}, out, noop)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("missing playlist: err = %v", err)
	}
	err = (&FFmpeg{}).Download(context.Background(), provider.Stream{URL: srv.URL + "/missing.mp4"}, out, noop)
	if err == nil || !strings.Contains(err.Error(), "ffmpeg") {
		t.Errorf("missing file: err = %v", err)
	}
}

// segmentPath returns the URL path of the n-th segment in playlist dir/name.
func segmentPath(t *testing.T, dir, name string, n int) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	var segs []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			segs = append(segs, l)
		}
	}
	if n >= len(segs) {
		t.Fatalf("%s has %d segments", name, len(segs))
	}
	return "/" + segs[n]
}

// ffmpeg skips a missing segment and still exits 0.
func TestFFmpegFailsOnMissingSegment(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	src := t.TempDir()
	makeHLS(t, src)
	missing := segmentPath(t, src, "stream_0.m3u8", 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == missing {
			http.NotFound(w, r)
			return
		}
		http.FileServer(http.Dir(src)).ServeHTTP(w, r)
	}))
	defer srv.Close()
	err := (&FFmpeg{}).Download(context.Background(), provider.Stream{URL: srv.URL + "/master.m3u8"},
		filepath.Join(t.TempDir(), "out.mp4"), func(time.Duration, int64) {})
	if err == nil || !strings.Contains(err.Error(), "incomplete download") {
		t.Fatalf("err = %v", err)
	}
}

// A CDN that accepts a request and then goes silent must not hang the job.
func TestFFmpegStallTimeout(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	src := t.TempDir()
	makeHLS(t, src)
	stalled := segmentPath(t, src, "stream_0.m3u8", 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == stalled {
			<-r.Context().Done()
			return
		}
		http.FileServer(http.Dir(src)).ServeHTTP(w, r)
	}))
	defer srv.Close()
	start := time.Now()
	err := (&FFmpeg{StallTimeout: time.Second}).Download(context.Background(),
		provider.Stream{URL: srv.URL + "/master.m3u8"}, filepath.Join(t.TempDir(), "out.mp4"), func(time.Duration, int64) {})
	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("err = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("took %v", elapsed)
	}
}

// ffmpeg logs "Packet corrupt" for a damaged TS segment and still exits 0.
func TestFFmpegFailsOnCorruptTSSegment(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	encoders, _ := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
	if !strings.Contains(string(encoders), "libx264") {
		t.Skip("ffmpeg lacks libx264")
	}
	src := t.TempDir()
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=10:duration=6",
		"-f", "lavfi", "-i", "sine=duration=6",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-g", "10", "-c:a", "aac",
		"-f", "hls", "-hls_time", "1", "-hls_playlist_type", "vod",
		filepath.Join(src, "index.m3u8"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating HLS: %v\n%s", err, out)
	}
	seg := filepath.Join(src, strings.TrimPrefix(segmentPath(t, src, "index.m3u8", 2), "/"))
	b, err := os.ReadFile(seg)
	if err != nil {
		t.Fatal(err)
	}
	const ts = 188 // TS packet size
	if len(b) < 40*ts {
		t.Fatalf("segment too small: %d bytes", len(b))
	}
	if err := os.WriteFile(seg, append(b[:20*ts:20*ts], b[40*ts:]...), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(src)))
	defer srv.Close()
	err = (&FFmpeg{}).Download(context.Background(), provider.Stream{URL: srv.URL + "/index.m3u8"},
		filepath.Join(t.TempDir(), "out.mp4"), func(time.Duration, int64) {})
	if err == nil || !strings.Contains(err.Error(), "incomplete download") {
		t.Fatalf("err = %v", err)
	}
}

func TestPickInputs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/split.m3u8":
			io.WriteString(w, `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="pl",DEFAULT=YES,URI="audio.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=5118260,RESOLUTION=1920x1080,CODECS="avc1.640029,mp4a.40.2",AUDIO="a"
video.m3u8
`)
		case "/muxed.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720\nhigh.m3u8\n")
		case "/media.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXTINF:4.0,\nseg1.ts\n")
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	for _, tt := range []struct {
		path string
		want []string
	}{
		// ffmpeg gets the best variant and, if separate, its audio.
		{"/split.m3u8", []string{srv.URL + "/video.m3u8", srv.URL + "/audio.m3u8"}},
		{"/muxed.m3u8", []string{srv.URL + "/high.m3u8"}},
		// Anything else is passed through.
		{"/media.m3u8", []string{srv.URL + "/media.m3u8"}},
		{"/film.mp4", []string{srv.URL + "/film.mp4"}},
	} {
		got, err := pickInputs(ctx, srv.Client(), provider.Stream{URL: srv.URL + tt.path})
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: got %v, %v; want %v", tt.path, got, err, tt.want)
		}
	}
}
