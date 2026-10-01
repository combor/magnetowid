package hls

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TVP's fMP4 media playlist (2026-10-01), shortened.
const tvpMedia = `#EXTM3U
#EXT-X-TARGETDURATION:4
#EXT-X-ALLOW-CACHE:YES
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-VERSION:6
#EXT-X-MEDIA-SEQUENCE:1
#EXT-X-MAP:URI="nv-dash-init-vod4-f5-v1-x3.mp4"
#EXTINF:4.000,
nv-dash-frag-vod4-1-f5-v1-x3.m4s
#EXTINF:3.840,
nv-dash-frag-vod4-2-f5-v1-x3.m4s
#EXT-X-ENDLIST
`

// BBC's TS media playlist (2026-10-01), shortened.
const bbcMedia = `#EXTM3U
#EXT-X-VERSION:2
## Created with Unified Streaming Platform(version=1.7.32)
#EXT-X-MEDIA-SEQUENCE:1
#EXT-X-TARGETDURATION:8
#USP-X-TIMESTAMP-MAP:MPEGTS=900000,LOCAL=1970-01-01T00:00:00Z
#EXTINF:8, no desc
vf.ism.hlsv2-audio_eng_1=128000-video=12000000-1.ts
#EXTINF:7.08, no desc
vf.ism.hlsv2-audio_eng_1=128000-video=12000000-2.ts
#EXT-X-ENDLIST
`

func TestParseMedia(t *testing.T) {
	base, _ := url.Parse("https://cdn.example/token/abc/video.ism/index.m3u8?t=1")
	dir := "https://cdn.example/token/abc/video.ism/"
	tests := []struct {
		name, playlist string
		want           []Segment
	}{
		{"tvp", tvpMedia, []Segment{
			{URI: dir + "nv-dash-frag-vod4-1-f5-v1-x3.m4s", Duration: 4 * time.Second, Init: dir + "nv-dash-init-vod4-f5-v1-x3.mp4"},
			{URI: dir + "nv-dash-frag-vod4-2-f5-v1-x3.m4s", Duration: 3840 * time.Millisecond, Init: dir + "nv-dash-init-vod4-f5-v1-x3.mp4"},
		}},
		{"bbc", bbcMedia, []Segment{
			{URI: dir + "vf.ism.hlsv2-audio_eng_1=128000-video=12000000-1.ts", Duration: 8 * time.Second},
			{URI: dir + "vf.ism.hlsv2-audio_eng_1=128000-video=12000000-2.ts", Duration: 7080 * time.Millisecond},
		}},
		{"discontinuity, new map, absolute URIs, no encryption", `#EXTM3U
#EXT-X-KEY:METHOD=NONE
#EXT-X-MAP:URI="init1.mp4"
#EXTINF:2,
https://other.example/1.m4s?sig=x
#EXT-X-DISCONTINUITY
#EXT-X-MAP:URI="/init2.mp4"
#EXTINF:2.5,
2.m4s
#EXT-X-ENDLIST
`, []Segment{
			{URI: "https://other.example/1.m4s?sig=x", Duration: 2 * time.Second, Init: dir + "init1.mp4"},
			{URI: dir + "2.m4s", Duration: 2500 * time.Millisecond, Init: "https://cdn.example/init2.mp4", Discontinuity: true},
		}},
	}
	for _, tt := range tests {
		got, err := parseMedia(tt.playlist, base)
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: got %+v, %v; want %+v", tt.name, got, err, tt.want)
		}
	}

	for name, playlist := range map[string]string{
		"live":            "#EXTM3U\n#EXTINF:4,\n1.ts\n",
		"empty":           "#EXTM3U\n#EXT-X-ENDLIST\n",
		"AES-128":         "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"k\"\n#EXTINF:4,\n1.ts\n#EXT-X-ENDLIST\n",
		"SAMPLE-AES":      "#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"skd://k\"\n#EXTINF:4,\n1.ts\n#EXT-X-ENDLIST\n",
		"byte range":      "#EXTM3U\n#EXTINF:4,\n#EXT-X-BYTERANGE:100@0\nall.ts\n#EXT-X-ENDLIST\n",
		"ranged map":      "#EXTM3U\n#EXT-X-MAP:URI=\"all.mp4\",BYTERANGE=\"100@0\"\n#EXTINF:4,\n1.m4s\n#EXT-X-ENDLIST\n",
		"gap":             "#EXTM3U\n#EXTINF:4,\n#EXT-X-GAP\n1.ts\n#EXT-X-ENDLIST\n",
		"variables":       "#EXTM3U\n#EXT-X-DEFINE:NAME=\"p\",VALUE=\"x\"\n#EXTINF:4,\n{$p}.ts\n#EXT-X-ENDLIST\n",
		"master":          "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nv.m3u8\n",
		"no duration":     "#EXTM3U\n1.ts\n#EXT-X-ENDLIST\n",
		"bad duration":    "#EXTM3U\n#EXTINF:NaN,\n1.ts\n#EXT-X-ENDLIST\n",
		"not a playlist":  "<html>Sorry</html>\n",
		"negative length": "#EXTM3U\n#EXTINF:-1,\n1.ts\n#EXT-X-ENDLIST\n",
	} {
		if segs, err := parseMedia(playlist, base); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: got %+v, %v; want ErrUnsupported", name, segs, err)
		}
	}
}

func TestLoadMedia(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.UserAgent()
		switch r.URL.Path {
		case "/old/index.m3u8":
			http.Redirect(w, r, "/new/index.m3u8", http.StatusFound)
		case "/new/index.m3u8":
			io.WriteString(w, bbcMedia)
		case "/huge.m3u8":
			io.WriteString(w, strings.Repeat("#", 16<<20+1))
		default:
			http.Error(w, "forbidden", http.StatusForbidden)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	segs, err := LoadMedia(ctx, srv.Client(), srv.URL+"/old/index.m3u8", http.Header{"User-Agent": {"magnetowid-test"}})
	if err != nil || len(segs) != 2 || segs[0].URI != srv.URL+"/new/vf.ism.hlsv2-audio_eng_1=128000-video=12000000-1.ts" ||
		gotUA != "magnetowid-test" {
		t.Errorf("got %+v, %v (UA %q)", segs, err, gotUA)
	}
	if _, err := LoadMedia(ctx, srv.Client(), srv.URL+"/refused.m3u8", nil); err == nil ||
		err.Error() != "fetching media playlist: HTTP 403" {
		t.Errorf("refused: err = %v", err)
	}
	if _, err := LoadMedia(ctx, srv.Client(), srv.URL+"/huge.m3u8", nil); err == nil || errors.Is(err, ErrUnsupported) {
		t.Errorf("oversized: err = %v", err)
	}
}
