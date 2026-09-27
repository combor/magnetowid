package downloader

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/combor/vodarr/internal/provider"
)

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
