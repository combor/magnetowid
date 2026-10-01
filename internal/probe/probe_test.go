package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/combor/magnetowid/internal/provider"
)

func TestResolution(t *testing.T) {
	for _, tt := range []struct {
		w, h int
		want string
	}{
		{3840, 2160, "2160p"},
		{1920, 1080, "1080p"},
		{1920, 800, "1080p"}, // a wide film: the width decides
		{1800, 750, "1080p"},
		{1799, 999, "720p"},
		{1280, 720, "720p"},
		{1200, 500, "720p"},
		{1024, 576, "576p"},
		{960, 540, "480p"},
		{400, 224, "480p"},
	} {
		if got := (Info{Width: tt.w, Height: tt.h}).Resolution(); got != tt.want {
			t.Errorf("%dx%d = %s, want %s", tt.w, tt.h, got, tt.want)
		}
	}
}

func TestCodecs(t *testing.T) {
	for _, tt := range []struct {
		codecs, video, audio string
	}{
		{"avc1.640029,mp4a.40.2", "H.264", "AAC"},
		{"mp4a.40.5, hvc1.2.4.L123.B0", "H.265", "AAC"},
		{"av01.0.08M.08,ec-3", "AV1", "DDP"},
		{"vp09.00.40.08,opus", "VP9", "Opus"},
		{"avc3.4d401f,ac-3", "H.264", "DD"},
		{"", "", ""},
		{"mp4v.20.9", "", ""},
		// Two audio codecs: which rendition is downloaded is unknown.
		{"avc1.640029,ec-3,mp4a.40.2", "H.264", ""},
		{"avc1.640029,mp4a.40.2,mp4a.40.5", "H.264", "AAC"},
	} {
		i := Info{Codecs: tt.codecs}
		if v, a := i.VideoCodec(), i.AudioCodec(); v != tt.video || a != tt.audio {
			t.Errorf("%q: got %q, %q; want %q, %q", tt.codecs, v, a, tt.video, tt.audio)
		}
	}
}

func TestLanguageName(t *testing.T) {
	for _, tt := range []struct{ language, want string }{
		{"pl", "POLISH"},
		{"pl-PL", "POLISH"},
		{"PL", "POLISH"},
		{"nb", "NORWEGIAN"},
		{"en-GB", "ENGLISH"},
		{"", ""},
		{"und", ""},
		{"cs", ""}, // Sonarr and Radarr read only "CZ"
	} {
		if got := (Info{Language: tt.language}).LanguageName(); got != tt.want {
			t.Errorf("%q = %q, want %q", tt.language, got, tt.want)
		}
	}
}

const master = `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio0",LANGUAGE="pl",NAME="Polski",AUTOSELECT=YES,DEFAULT=YES,URI="a.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=3598429,RESOLUTION=1280x720,CODECS="avc1.64001f,mp4a.40.2",AUDIO="audio0"
720.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5118260,AVERAGE-BANDWIDTH=4000000,RESOLUTION=1920x1080,CODECS="avc1.640029,mp4a.40.2",AUDIO="audio0"
1080.m3u8
`

type fakeProvider struct {
	base        string
	unavailable map[string]bool
	transport   http.RoundTripper
	mu          sync.Mutex
	resolves    int
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Search(context.Context, provider.Query) ([]provider.Item, error) {
	return nil, nil
}

func (f *fakeProvider) Resolve(_ context.Context, id string) (provider.Stream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolves++
	if f.unavailable[id] {
		return provider.Stream{}, fmt.Errorf("%w: DRM-protected", provider.ErrUnavailable)
	}
	return provider.Stream{URL: f.base + "/" + id, Transport: f.transport}, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func (f *fakeProvider) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resolves
}

func newFixture(t *testing.T, flaky int) (*Prober, *fakeProvider) {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.m3u8":
			io.WriteString(w, master)
		case "/noresolution.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nv.m3u8\n")
		case "/stall.m3u8":
			<-r.Context().Done()
		case "/flaky.m3u8":
			mu.Lock()
			refuse := flaky > 0
			flaky--
			mu.Unlock()
			if refuse {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			io.WriteString(w, master)
		default:
			http.Error(w, "forbidden", http.StatusForbidden)
		}
	}))
	t.Cleanup(srv.Close)
	return &Prober{Client: srv.Client()}, &fakeProvider{base: srv.URL, unavailable: map[string]bool{"drm.m3u8": true}}
}

func TestProbe(t *testing.T) {
	pr, p := newFixture(t, 0)
	want := Info{Width: 1920, Height: 1080, Codecs: "avc1.640029,mp4a.40.2", Bandwidth: 4000000, Language: "pl"}
	for range 2 {
		if got, err := pr.Probe(context.Background(), p, "ok.m3u8"); err != nil || got != want {
			t.Fatalf("Probe = %+v, %v; want %+v", got, err, want)
		}
	}
	if n := p.count(); n != 1 {
		t.Errorf("%d resolves for two probes, want 1", n)
	}

	if _, err := pr.Probe(context.Background(), p, "film.mp4"); !errors.Is(err, ErrNotHLS) {
		t.Errorf("mp4: err = %v", err)
	}
	if _, err := pr.Probe(context.Background(), p, "noresolution.m3u8"); !errors.Is(err, errNoResolution) {
		t.Errorf("no resolution: err = %v", err)
	}
}

// The stream's transport, e.g. its site's proxy, fetches the playlist.
func TestProbeUsesStreamTransport(t *testing.T) {
	pr, p := newFixture(t, 0)
	pr.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("wrong transport")
	})}
	fetched := 0
	p.transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		fetched++
		return http.DefaultTransport.RoundTrip(req)
	})
	if _, err := pr.Probe(context.Background(), p, "ok.m3u8"); err != nil || fetched != 1 {
		t.Errorf("Probe: %v after %d requests on the stream's transport, want 1", err, fetched)
	}
}

func TestProbeRetries(t *testing.T) {
	pr, p := newFixture(t, 2)
	if got, err := pr.Probe(context.Background(), p, "flaky.m3u8"); err != nil || got.Height != 1080 {
		t.Fatalf("Probe = %+v, %v", got, err)
	}
	if n := p.count(); n != 3 {
		t.Errorf("%d resolves, want 3", n)
	}
}

// Keep failures stable within one search; do not cache caller cancellation.
func TestProbeFailureIsCachedBriefly(t *testing.T) {
	pr, p := newFixture(t, 0)
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	pr.now = func() time.Time { return now }
	for range 2 {
		if _, err := pr.Probe(context.Background(), p, "down.m3u8"); err == nil || err.Error() != "fetching playlist: HTTP 403" {
			t.Fatalf("err = %v", err)
		}
	}
	if n := p.count(); n != attempts {
		t.Errorf("%d resolves, want %d", n, attempts)
	}
	now = now.Add(failureTTL + time.Second)
	pr.Probe(context.Background(), p, "down.m3u8")
	if n := p.count(); n != 2*attempts {
		t.Errorf("%d resolves after %v, want %d", n, failureTTL, 2*attempts)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pr.Probe(ctx, p, "ok.m3u8"); err == nil {
		t.Fatal("cancelled probe succeeded")
	}
	if _, err := pr.Probe(context.Background(), p, "ok.m3u8"); err != nil {
		t.Errorf("after a cancelled probe: %v", err)
	}
}

// A probe timeout is cacheable; caller cancellation is not.
func TestProbeStallTimesOut(t *testing.T) {
	pr, p := newFixture(t, 0)
	pr.Timeout = 100 * time.Millisecond
	start := time.Now()
	if _, err := pr.Probe(context.Background(), p, "stall.m3u8"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("probe took %v", elapsed)
	}
	pr.Probe(context.Background(), p, "stall.m3u8")
	if n := p.count(); n != 1 {
		t.Errorf("%d resolves, want 1: the timeout ends the attempts and is cached", n)
	}
}

func TestStoreKeepsSuccess(t *testing.T) {
	var pr Prober
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	ok := cached{info: Info{Width: 1920, Height: 1080}, expires: now.Add(cacheTTL)}
	pr.store("fake:1", ok, now)
	failed := cached{err: errors.New("HTTP 403"), expires: now.Add(failureTTL)}
	if got := pr.store("fake:1", failed, now); got != ok {
		t.Errorf("store returned %+v, want the success", got)
	}
	if got := pr.cache["fake:1"]; got != ok {
		t.Errorf("cached %+v, want the success", got)
	}
	pr.store("fake:2", failed, now)
	if got := pr.store("fake:2", ok, now); got != ok {
		t.Errorf("store over a failure returned %+v", got)
	}
}

func TestProbeUnavailableIsCachedBriefly(t *testing.T) {
	pr, p := newFixture(t, 0)
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	pr.now = func() time.Time { return now }
	for range 2 {
		if _, err := pr.Probe(context.Background(), p, "drm.m3u8"); !errors.Is(err, provider.ErrUnavailable) {
			t.Fatalf("err = %v", err)
		}
	}
	if n := p.count(); n != 1 {
		t.Errorf("%d resolves, want 1: unavailable is final and cached", n)
	}
	now = now.Add(unavailableTTL + time.Second)
	pr.Probe(context.Background(), p, "drm.m3u8")
	if n := p.count(); n != 2 {
		t.Errorf("%d resolves after %v, want 2", n, unavailableTTL)
	}
}
