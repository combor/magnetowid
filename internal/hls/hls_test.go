package hls

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/combor/magnetowid/internal/provider"
)

// TVP's master playlist (2026-09-26), variants reordered to test sorting.
const tvpMaster = `#EXTM3U

#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio0",LANGUAGE="pl",NAME="Polski",AUTOSELECT=YES,DEFAULT=YES,CHANNELS="2",URI="nv-hlsfmp4-index-vod4-f1-a1.m3u8"

#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=3598429,RESOLUTION=1280x720,FRAME-RATE=25.000,CODECS="avc1.64001f,mp4a.40.2",AUDIO="audio0"
nv-hlsfmp4-index-vod4-f6-v1.m3u8
#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=5118260,RESOLUTION=1920x1080,FRAME-RATE=25.000,CODECS="avc1.640029,mp4a.40.2",AUDIO="audio0"
nv-hlsfmp4-index-vod4-f7-v1.m3u8
#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=527008,RESOLUTION=400x224,FRAME-RATE=25.000,CODECS="avc1.42c01f,mp4a.40.2",AUDIO="audio0"
nv-hlsfmp4-index-vod4-f1-v1.m3u8
`

func TestSelectRenditions(t *testing.T) {
	tests := []struct {
		name, playlist  string
		video           Variant
		audio, language string
		ok              bool
	}{
		{"tvp", tvpMaster, Variant{
			URI: "nv-hlsfmp4-index-vod4-f7-v1.m3u8", Width: 1920, Height: 1080, Bandwidth: 5118260,
			Codecs: "avc1.640029,mp4a.40.2", audio: "audio0",
		}, "nv-hlsfmp4-index-vod4-f1-a1.m3u8", "pl", true},
		{"muxed audio, average bandwidth", `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360
low.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2000000,AVERAGE-BANDWIDTH=1500000,RESOLUTION=1280x720
high.m3u8
`, Variant{URI: "high.m3u8", Width: 1280, Height: 720, Bandwidth: 2000000, Average: 1500000}, "", "", true},
		{"audio-only variant and no resolutions", `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="en",LANGUAGE="en",URI="en.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="pl",LANGUAGE="pl",AUTOSELECT=YES,URI="pl.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=900000,CODECS="mp4a.40.2",AUDIO="a"
audio-only.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=300000,CODECS="avc1.4d401f,mp4a.40.2",AUDIO="a"
video.m3u8
`, Variant{URI: "video.m3u8", Bandwidth: 300000, Codecs: "avc1.4d401f,mp4a.40.2", audio: "a"}, "pl.m3u8", "pl", true},
		{"default audio muxed into the variant", `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="pl",LANGUAGE="pl",DEFAULT=YES,AUTOSELECT=YES
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="en",LANGUAGE="en",URI="en.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720,AUDIO="a"
video.m3u8
`, Variant{URI: "video.m3u8", Width: 1280, Height: 720, Bandwidth: 2000000, audio: "a"}, "", "pl", true},
		{"no codecs means video, bandwidth breaks ties", `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=1
a.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2
b.m3u8
`, Variant{URI: "b.m3u8", Bandwidth: 2}, "", "", true},
		{"media playlist", `#EXTM3U
#EXT-X-TARGETDURATION:4
#EXT-X-MAP:URI="init.mp4"
#EXTINF:4.0,
seg1.m4s
#EXT-X-ENDLIST
`, Variant{}, "", "", false},
	}
	for _, tt := range tests {
		video, audio, ok := selectRenditions(tt.playlist)
		if video != tt.video || audio.uri != tt.audio || audio.language != tt.language || ok != tt.ok {
			t.Errorf("%s: got (%+v, %q, %q, %v), want (%+v, %q, %q, %v)", tt.name,
				video, audio.uri, audio.language, ok, tt.video, tt.audio, tt.language, tt.ok)
		}
	}
}

func TestParseAttrs(t *testing.T) {
	got := parseAttrs(`PROGRAM-ID=1,BANDWIDTH=527008,CODECS="avc1.42c01f,mp4a.40.2",AUDIO="audio0",FRAME-RATE=25.000`)
	want := map[string]string{"PROGRAM-ID": "1", "BANDWIDTH": "527008", "CODECS": "avc1.42c01f,mp4a.40.2",
		"AUDIO": "audio0", "FRAME-RATE": "25.000"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseAttrs = %v", got)
	}
}

func TestLoad(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.UserAgent()
		switch r.URL.Path {
		case "/redirect/video-fmp4.m3u8":
			http.Redirect(w, r, "/token/abc/video.ism/video-fmp4.m3u8", http.StatusFound)
		case "/token/abc/video.ism/video-fmp4.m3u8":
			io.WriteString(w, tvpMaster)
		case "/media.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXTINF:4.0,\nseg1.ts\n")
		case "/refused.m3u8":
			http.Error(w, "forbidden", http.StatusForbidden)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	h := http.Header{"User-Agent": {"magnetowid-test"}}

	m, ok, err := Load(ctx, srv.Client(), provider.Stream{URL: srv.URL + "/token/abc/video.ism/video-fmp4.m3u8", Header: h})
	if err != nil || !ok {
		t.Fatalf("master: ok = %v, err = %v", ok, err)
	}
	want := Master{
		Video: Variant{
			URI:   srv.URL + "/token/abc/video.ism/nv-hlsfmp4-index-vod4-f7-v1.m3u8",
			Width: 1920, Height: 1080, Bandwidth: 5118260, Codecs: "avc1.640029,mp4a.40.2", audio: "audio0",
		},
		Audio:         srv.URL + "/token/abc/video.ism/nv-hlsfmp4-index-vod4-f1-a1.m3u8",
		AudioLanguage: "pl",
	}
	if m != want || gotUA != "magnetowid-test" {
		t.Errorf("master: got %+v (UA %q), want %+v", m, gotUA, want)
	}

	m, ok, err = Load(ctx, srv.Client(), provider.Stream{URL: srv.URL + "/redirect/video-fmp4.m3u8"})
	if err != nil || !ok || m != want {
		t.Errorf("redirected master: got %+v, %v, %v; want %+v", m, ok, err, want)
	}

	// A provider's amended playlist is not fetched again.
	amended := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=9000000,RESOLUTION=1920x1080\nhidden-1080p.m3u8\n"
	m, ok, err = Load(ctx, srv.Client(), provider.Stream{URL: srv.URL + "/token/abc/gone.m3u8", Playlist: amended})
	if want := (Variant{URI: srv.URL + "/token/abc/hidden-1080p.m3u8", Width: 1920, Height: 1080, Bandwidth: 9000000}); err != nil || !ok || m.Video != want {
		t.Errorf("amended playlist: got %+v, %v, %v; want %+v", m.Video, ok, err, want)
	}

	for _, u := range []string{srv.URL + "/media.m3u8", srv.URL + "/video.mpd", srv.URL + "/film.mp4"} {
		if _, ok, err := Load(ctx, srv.Client(), provider.Stream{URL: u}); ok || err != nil {
			t.Errorf("%s: ok = %v, err = %v", u, ok, err)
		}
	}

	if _, _, err := Load(ctx, srv.Client(), provider.Stream{URL: srv.URL + "/refused.m3u8"}); err == nil || err.Error() != "fetching playlist: HTTP 403" {
		t.Errorf("refused: err = %v", err)
	}
}
