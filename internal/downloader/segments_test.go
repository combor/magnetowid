package downloader

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/combor/magnetowid/internal/hls"
	"github.com/combor/magnetowid/internal/provider"
)

func needFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
}

func noRetryDelay(int) time.Duration { return 0 }

// requests counts requests by path.
type requests struct {
	mu sync.Mutex
	n  map[string]int
}

func (r *requests) add(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == nil {
		r.n = map[string]int{}
	}
	r.n[path]++
}

func (r *requests) get(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n[path]
}

func savedFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestSegmentsResumeAfterCancel(t *testing.T) {
	needFFmpeg(t)
	src := t.TempDir()
	makeHLS(t, src)
	blocked := segmentPath(t, src, "stream_0.m3u8", 2)
	var block atomic.Bool
	block.Store(true)
	reached := make(chan struct{}, 1)
	var reqs requests
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.add(r.URL.Path)
		if r.URL.Path == blocked && block.Load() {
			reached <- struct{}{}
			<-r.Context().Done()
			return
		}
		http.FileServer(http.Dir(src)).ServeHTTP(w, r)
	}))
	defer srv.Close()
	work := t.TempDir()
	out := filepath.Join(work, "out.mp4")
	s := provider.Stream{URL: srv.URL + "/master.m3u8"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- (&FFmpeg{}).Download(ctx, s, out, func(time.Duration, int64) {}) }()
	<-reached
	// Let the other workers finish what they hold.
	time.Sleep(200 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled download: err = %v", err)
	}
	var saved []string
	for _, f := range savedFiles(t, filepath.Join(work, segmentsDir)) {
		if strings.HasSuffix(f, ".part") {
			t.Errorf("partial file left: %s", f)
		}
		if !strings.HasSuffix(f, ".m3u8") {
			saved = append(saved, f)
		}
	}
	if len(saved) < 3 {
		t.Fatalf("saved %v, want at least the initialization sections and the first video segments", saved)
	}

	block.Store(false)
	before := map[string]int{}
	for _, p := range []string{"/init_0.mp4", segmentPath(t, src, "stream_0.m3u8", 0), segmentPath(t, src, "stream_0.m3u8", 1)} {
		before[p] = reqs.get(p)
	}
	var first []int64
	err := (&FFmpeg{}).Download(context.Background(), s, out, func(done time.Duration, bytes int64) {
		if first == nil {
			first = []int64{int64(done), bytes}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	for p, n := range before {
		if got := reqs.get(p); got != n {
			t.Errorf("%s fetched again", p)
		}
	}
	if first == nil || first[0] < int64(2*time.Second) || first[1] == 0 {
		t.Errorf("first progress (done, bytes) = %v, want the saved segments", first)
	}
	probe, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type",
		"-of", "csv=p=0", out).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(probe)); len(got) != 2 {
		t.Errorf("streams = %v, want video and audio", got)
	}
	checkAudioLanguage(t, out, "pol")
}

func TestSegmentsRestartForAnotherRendition(t *testing.T) {
	needFFmpeg(t)
	src := t.TempDir()
	makeHLS(t, src)
	master, err := os.ReadFile(filepath.Join(src, "master.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	// Offer only the low variant from now on.
	low := strings.Replace(string(master), "stream_0.m3u8", "stream_1.m3u8", 1)
	var reqs requests
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.add(r.URL.Path)
		if r.URL.Path == "/low.m3u8" {
			w.Write([]byte(low))
			return
		}
		http.FileServer(http.Dir(src)).ServeHTTP(w, r)
	}))
	defer srv.Close()
	work := t.TempDir()
	out := filepath.Join(work, "out.mp4")
	noop := func(time.Duration, int64) {}
	if err := (&FFmpeg{}).Download(context.Background(), provider.Stream{URL: srv.URL + "/master.m3u8"}, out, noop); err != nil {
		t.Fatal(err)
	}
	if err := (&FFmpeg{}).Download(context.Background(), provider.Stream{URL: srv.URL + "/low.m3u8"}, out, noop); err != nil {
		t.Fatal(err)
	}
	if n := reqs.get("/init_2.mp4"); n != 2 {
		t.Errorf("audio fetched %d times, want again for the new video", n)
	}
	probe, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width",
		"-of", "csv=p=0", out).Output()
	if err != nil || strings.TrimSpace(string(probe)) != "80" {
		t.Errorf("width = %q, %v; want the low variant's 80", probe, err)
	}
}

func TestSegmentsRetry(t *testing.T) {
	needFFmpeg(t)
	src := t.TempDir()
	makeHLS(t, src)
	seg := segmentPath(t, src, "stream_0.m3u8", 1)
	for _, tt := range []struct {
		name     string
		statuses []int // then 200
		requests int
		err      string
	}{
		{"not found", []int{404}, 1, "video segment 2 of 3: HTTP 404"},
		{"server errors", []int{503, 503, 503, 503, 503, 503}, segmentTries, "video segment 2 of 3: HTTP 503"},
		{"one server error", []int{500}, 2, ""},
		{"rate limited", []int{429}, 2, ""},
		{"error page", []int{200}, 2, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var reqs requests
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == seg {
					reqs.add(seg)
					if n := reqs.get(seg); n <= len(tt.statuses) {
						w.Header().Set("Content-Type", "text/html; charset=utf-8")
						w.WriteHeader(tt.statuses[n-1])
						w.Write([]byte("<html>Try again later</html>"))
						return
					}
				}
				http.FileServer(http.Dir(src)).ServeHTTP(w, r)
			}))
			defer srv.Close()
			err := (&FFmpeg{retryDelay: noRetryDelay}).Download(context.Background(),
				provider.Stream{URL: srv.URL + "/master.m3u8"}, filepath.Join(t.TempDir(), "out.mp4"), func(time.Duration, int64) {})
			if tt.err == "" && err != nil || tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)) {
				t.Errorf("err = %v, want %q", err, tt.err)
			}
			if n := reqs.get(seg); n != tt.requests {
				t.Errorf("%d requests, want %d", n, tt.requests)
			}
		})
	}
}

func TestSegmentsStall(t *testing.T) {
	needFFmpeg(t)
	src := t.TempDir()
	makeHLS(t, src)
	stalled := segmentPath(t, src, "stream_0.m3u8", 1)
	var reqs requests
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == stalled {
			reqs.add(stalled)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("x"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		http.FileServer(http.Dir(src)).ServeHTTP(w, r)
	}))
	defer srv.Close()
	start := time.Now()
	err := (&FFmpeg{segmentTimeout: 200 * time.Millisecond, retryDelay: noRetryDelay}).Download(context.Background(),
		provider.Stream{URL: srv.URL + "/master.m3u8"}, filepath.Join(t.TempDir(), "out.mp4"), func(time.Duration, int64) {})
	if err == nil || !strings.Contains(err.Error(), "video segment 2 of 3: stalled: no data for 200ms") {
		t.Errorf("err = %v", err)
	}
	if n := reqs.get(stalled); n != segmentTries {
		t.Errorf("%d requests, want %d", n, segmentTries)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %v", elapsed)
	}
}

func TestSegmentsInParallel(t *testing.T) {
	needFFmpeg(t)
	src := t.TempDir()
	makeHLS(t, src)
	var running, most atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m4s") {
			n := running.Add(1)
			defer running.Add(-1)
			for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
			}
			time.Sleep(100 * time.Millisecond)
		}
		http.FileServer(http.Dir(src)).ServeHTTP(w, r)
	}))
	defer srv.Close()
	err := (&FFmpeg{}).Download(context.Background(), provider.Stream{URL: srv.URL + "/master.m3u8"},
		filepath.Join(t.TempDir(), "out.mp4"), func(time.Duration, int64) {})
	if err != nil {
		t.Fatal(err)
	}
	if n := most.Load(); n < 2 || n > segmentWorkers {
		t.Errorf("%d segments at once, want 2 to %d", n, segmentWorkers)
	}
}

// makeTS writes index.m3u8 with muxed TS segments, and master.m3u8 for it.
func makeTS(t *testing.T, dir string) {
	t.Helper()
	encoders, _ := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
	if !strings.Contains(string(encoders), "libx264") {
		t.Skip("ffmpeg lacks libx264")
	}
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=10:duration=6",
		"-f", "lavfi", "-i", "sine=duration=6",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-g", "10", "-c:a", "aac",
		"-f", "hls", "-hls_time", "1", "-hls_playlist_type", "vod",
		filepath.Join(dir, "index.m3u8"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating HLS: %v\n%s", err, out)
	}
	master := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=300000,RESOLUTION=160x120,CODECS=\"avc1.64000c,mp4a.40.2\"\nindex.m3u8\n"
	if err := os.WriteFile(filepath.Join(dir, "master.m3u8"), []byte(master), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSegmentsTS(t *testing.T) {
	needFFmpeg(t)
	src := t.TempDir()
	makeTS(t, src)
	srv := httptest.NewServer(http.FileServer(http.Dir(src)))
	defer srv.Close()
	work := t.TempDir()
	out := filepath.Join(work, "out.mp4")
	var bytes int64
	err := (&FFmpeg{}).Download(context.Background(), provider.Stream{URL: srv.URL + "/master.m3u8"},
		out, func(_ time.Duration, b int64) { bytes = b })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(work, segmentsDir, "v", "000005.ts")); err != nil {
		t.Error(err)
	}
	fi, err := os.Stat(out)
	if err != nil || bytes != fi.Size() {
		t.Errorf("last progress %d bytes, file %v, %v", bytes, fi, err)
	}
	probe, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type",
		"-of", "csv=p=0", out).Output()
	if got := strings.Fields(string(probe)); err != nil || len(got) != 2 {
		t.Errorf("streams = %v, %v; want video and audio", got, err)
	}
}

// A damaged segment fails the remux; fetching it again may help.
func TestSegmentsCorruptTS(t *testing.T) {
	needFFmpeg(t)
	src := t.TempDir()
	makeTS(t, src)
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
	work := t.TempDir()
	err = (&FFmpeg{}).Download(context.Background(), provider.Stream{URL: srv.URL + "/master.m3u8"},
		filepath.Join(work, "out.mp4"), func(time.Duration, int64) {})
	if err == nil || !strings.Contains(err.Error(), "incomplete download") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, segmentsDir)); !os.IsNotExist(err) {
		t.Errorf("damaged segments kept: %v", err)
	}
}

func TestSegmentExt(t *testing.T) {
	for uri, want := range map[string]string{
		"https://cdn.example/a/seg-1.ts?token=x.mp4": ".ts",
		"https://cdn.example/seg.AAC":                ".aac",
		"https://cdn.example/seg.ec3":                ".eac3",
		"https://cdn.example/seg.m4s":                ".mp4",
		"https://cdn.example/seg.php?id=1":           ".ts",
		"https://cdn.example/seg":                    ".ts",
	} {
		if got := segmentExt(hls.Segment{URI: uri}); got != want {
			t.Errorf("%s: %q, want %q", uri, got, want)
		}
	}
	if got := segmentExt(hls.Segment{URI: "https://cdn.example/seg.ts", Init: "https://cdn.example/init"}); got != ".mp4" {
		t.Errorf("segment with an initialization section: %q, want .mp4", got)
	}
}

// Files are saved by position, so renamed ones are another stream's.
func TestSegmentsRestartForRenamedFiles(t *testing.T) {
	needFFmpeg(t)
	src := t.TempDir()
	makeHLS(t, src)
	media, err := os.ReadFile(filepath.Join(src, "stream_0.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	var reqs requests
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.add(r.URL.Path)
		name := filepath.Base(r.URL.Path)
		switch {
		case r.URL.Path == "/v2/stream_0.m3u8":
			w.Write([]byte(strings.ReplaceAll(string(media), "stream_0", "fresh_0")))
			return
		case strings.HasPrefix(name, "fresh_0"):
			name = strings.Replace(name, "fresh_0", "stream_0", 1)
		}
		http.ServeFile(w, r, filepath.Join(src, name))
	}))
	defer srv.Close()
	out := filepath.Join(t.TempDir(), "out.mp4")
	noop := func(time.Duration, int64) {}
	for _, v := range []string{"/v1/", "/v2/"} {
		if err := (&FFmpeg{}).Download(context.Background(), provider.Stream{URL: srv.URL + v + "master.m3u8"}, out, noop); err != nil {
			t.Fatal(err)
		}
	}
	if reqs.get("/v2/fresh_00.m4s") != 1 || reqs.get("/v2/init_2.mp4") != 1 {
		t.Error("kept the files of a stream whose segments are named differently")
	}
}

// A damaged initialization section fails the remux; it mustn't fail every retry.
func TestSegmentsRemoveFilesAfterFailedRemux(t *testing.T) {
	needFFmpeg(t)
	src := t.TempDir()
	makeHLS(t, src)
	bad := "/init_0.mp4"
	var broken atomic.Bool
	broken.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == bad && broken.Load() {
			w.Header().Set("Content-Type", "video/mp4")
			w.Write([]byte("<html><body>Service temporarily unavailable</body></html>"))
			return
		}
		http.FileServer(http.Dir(src)).ServeHTTP(w, r)
	}))
	defer srv.Close()
	work := t.TempDir()
	out := filepath.Join(work, "out.mp4")
	s := provider.Stream{URL: srv.URL + "/master.m3u8"}
	noop := func(time.Duration, int64) {}
	if err := (&FFmpeg{}).Download(context.Background(), s, out, noop); err == nil {
		t.Fatal("remuxed an error page")
	}
	if _, err := os.Stat(filepath.Join(work, segmentsDir)); !os.IsNotExist(err) {
		t.Fatalf("damaged segments kept: %v", err)
	}
	broken.Store(false)
	if err := (&FFmpeg{}).Download(context.Background(), s, out, noop); err != nil {
		t.Fatalf("after the site recovered: %v", err)
	}
}

func TestMissingFFmpegFailsFirst(t *testing.T) {
	var reqs requests
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.add("any")
		http.NotFound(w, r)
	}))
	defer srv.Close()
	err := (&FFmpeg{Path: filepath.Join(t.TempDir(), "ffmpeg")}).Download(context.Background(),
		provider.Stream{URL: srv.URL + "/master.m3u8"}, filepath.Join(t.TempDir(), "out.mp4"), func(time.Duration, int64) {})
	if err == nil || !strings.HasPrefix(err.Error(), "ffmpeg: ") || reqs.get("any") != 0 {
		t.Errorf("err = %v after %d requests", err, reqs.get("any"))
	}
}

// The output's failures leave the downloaded segments alone.
func TestSegmentsKeptAfterOutputFailure(t *testing.T) {
	needFFmpeg(t)
	src := t.TempDir()
	makeHLS(t, src)
	srv := httptest.NewServer(http.FileServer(http.Dir(src)))
	defer srv.Close()
	work := t.TempDir()
	out := filepath.Join(work, "out.mp4")
	if err := os.Mkdir(out, 0o777); err != nil { // unwritable as a file
		t.Fatal(err)
	}
	err := (&FFmpeg{}).Download(context.Background(), provider.Stream{URL: srv.URL + "/master.m3u8"}, out, func(time.Duration, int64) {})
	if err == nil {
		t.Fatal("wrote over a folder")
	}
	if _, err := os.Stat(filepath.Join(work, segmentsDir, "v", "000002.mp4")); err != nil {
		t.Errorf("segments removed after %v", err)
	}
}

// Renditions may differ only in bitrate and share every file name.
func TestSegmentsRestartForAnotherBitrate(t *testing.T) {
	needFFmpeg(t)
	src := t.TempDir()
	makeHLS(t, src)
	var reqs requests
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.add(r.URL.Path)
		if bw, ok := strings.CutPrefix(r.URL.Path, "/master-"); ok {
			dir := map[string]string{"200000.m3u8": "hi", "100000.m3u8": "lo"}[bw]
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=%s,RESOLUTION=160x120,CODECS=\"avc1.64000c,mp4a.40.2\"\n%s/stream_0.m3u8\n",
				strings.TrimSuffix(bw, ".m3u8"), dir)
			return
		}
		http.ServeFile(w, r, filepath.Join(src, filepath.Base(r.URL.Path)))
	}))
	defer srv.Close()
	out := filepath.Join(t.TempDir(), "out.mp4")
	noop := func(time.Duration, int64) {}
	for _, master := range []string{"/master-200000.m3u8", "/master-100000.m3u8"} {
		if err := (&FFmpeg{}).Download(context.Background(), provider.Stream{URL: srv.URL + master}, out, noop); err != nil {
			t.Fatal(err)
		}
	}
	if reqs.get("/lo/stream_00.m4s") != 1 {
		t.Error("kept the files of a rendition at another bitrate")
	}
}
