package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/combor/magnetowid/internal/provider"
)

// The sites magnetowid reads, faked for TestSonarrAndRadarr: TVP's API,
// Skyhook and Wikidata over HTTPS, reached through a proxy, and a stream
// shaped like TVP's over HTTP. magnetowid uses them as it would the real
// ones, and the proxy refuses any other site.

// fakedHosts are the sites the proxy passes on to the fakes.
var fakedHosts = []string{"vod.tvp.pl", "skyhook.sonarr.tv", "www.wikidata.org"}

// tvpProduct is a product as TVP's API gives it.
type tvpProduct struct {
	ID            int64  `json:"id"`
	Type          string `json:"type"`
	Title         string `json:"title"`
	OriginalTitle string `json:"originalTitle,omitempty"`
	Year          int    `json:"year,omitempty"`
	Number        int    `json:"number,omitempty"`
	Duration      int    `json:"duration,omitempty"` // seconds
	Payable       bool   `json:"payable"`
	Since         string `json:"since,omitempty"`
}

// TVP's catalogue: Czas honoru, which is TVDB's Days of Honor, with the
// first three episodes of season 1, and the film Cube. The film Seksmisja
// (Radarr's Sexmission) comes later, when the test releases it.
const daysOfHonorTVDB = 83920

var (
	czasHonoru = tvpProduct{ID: 1001, Type: "SERIAL", Title: "Czas honoru", Year: 2008}
	// season 1 of czasHonoru.
	czasHonoru1 = tvpProduct{ID: 1002, Type: "SEASON", Title: "Sezon 1", Number: 1}
	cube        = tvpProduct{ID: 2001, Type: "VOD", Title: "Cube", OriginalTitle: "Cube", Year: 1997, Duration: 5195}
	seksmisja   = tvpProduct{ID: 2002, Type: "VOD", Title: "Seksmisja", OriginalTitle: "Seksmisja", Year: 1984, Duration: 7020}
)

// czasHonoruEpisodes are season 1's episodes on TVP.
func czasHonoruEpisodes() []tvpProduct {
	var eps []tvpProduct
	for n := 1; n <= 3; n++ {
		eps = append(eps, tvpProduct{ID: int64(1010 + n), Type: "EPISODE", Title: "odc. " + strconv.Itoa(n),
			Year: 2008, Number: n, Duration: 3000, Since: "2026-01-01T00:00:00+01:00"})
	}
	return eps
}

// fakeSites serves the fakes.
type fakeSites struct {
	t           *testing.T
	proxyURL    string // for HTTPS_PROXY
	certFile    string // for SSL_CERT_FILE: the sites' certificate, which signs itself
	streamURL   string // every item's
	subtitleURL string // every item's, for the deaf and hard of hearing

	mu    sync.Mutex
	films []tvpProduct
}

// startFakeSites starts the fakes, keeping the stream in dir.
func startFakeSites(ctx context.Context, t *testing.T, dir string) *fakeSites {
	t.Helper()
	s := &fakeSites{t: t, films: []tvpProduct{cube}}

	streams := httptest.NewServer(http.FileServer(http.Dir(makeStream(ctx, t, filepath.Join(dir, "stream")))))
	t.Cleanup(streams.Close)
	s.streamURL = streams.URL + "/master.m3u8"
	s.subtitleURL = streams.URL + "/subtitles.xml"

	cert, certPEM := selfSignedCert(t, fakedHosts)
	s.certFile = filepath.Join(dir, "sites.pem")
	if err := os.WriteFile(s.certFile, certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	sites := httptest.NewUnstartedServer(s.handler())
	sites.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	sites.StartTLS()
	t.Cleanup(sites.Close)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go s.serveProxy(ln, sites.Listener.Addr().String())
	s.proxyURL = "http://" + ln.Addr().String()
	return s
}

// release adds the film to TVP's catalogue.
func (s *fakeSites) release(film tvpProduct) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.films = append(s.films, film)
}

func (s *fakeSites) handler() http.Handler {
	mux := http.NewServeMux()
	const tvp = "GET vod.tvp.pl/api/products/"
	mux.HandleFunc(tvp+"vods/search/{kind}", s.tvpSearch)
	mux.HandleFunc(tvp+"vods/serials/{serial}/seasons", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("serial") != strconv.FormatInt(czasHonoru.ID, 10) {
			tvpNotFound(w)
			return
		}
		writeJSON(w, []tvpProduct{czasHonoru1})
	})
	mux.HandleFunc(tvp+"vods/serials/{serial}/seasons/{season}/episodes", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("serial") != strconv.FormatInt(czasHonoru.ID, 10) || r.PathValue("season") != strconv.FormatInt(czasHonoru1.ID, 10) {
			tvpNotFound(w)
			return
		}
		writeJSON(w, czasHonoruEpisodes())
	})
	// TVP's newest products, which the film feed reads.
	mux.HandleFunc(tvp+"vods", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		newest := slices.Clone(s.films)
		slices.Reverse(newest)
		writeJSON(w, map[string]any{"items": newest})
	})
	mux.HandleFunc(tvp+"{id}/videos/playlist", func(w http.ResponseWriter, r *http.Request) {
		if !s.known(r.PathValue("id")) {
			tvpNotFound(w)
			return
		}
		writeJSON(w, map[string]any{
			"sources":   map[string]any{"HLS": []any{map[string]string{"src": s.streamURL}}},
			"drm":       nil,
			"subtitles": []any{map[string]string{"url": s.subtitleURL, "language": "POLISH_DLA_NIESLYSZACYCH", "isoCode": "POL"}},
		})
	})
	mux.HandleFunc("GET skyhook.sonarr.tv/v1/tvdb/shows/en/{id}", s.skyhook)
	mux.HandleFunc("GET www.wikidata.org/w/api.php", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("gsrsearch") != "haswbstatement:P4835="+strconv.Itoa(daysOfHonorTVDB) {
			writeJSON(w, map[string]any{"batchcomplete": true}) // as Wikidata answers a search that finds nothing
			return
		}
		writeJSON(w, map[string]any{"query": map[string]any{"pages": []any{
			map[string]any{"entityterms": map[string]any{"label": []string{czasHonoru.Title}}},
		}}})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.t.Errorf("magnetowid asked a fake site for %s %s%s, which it doesn't serve", r.Method, r.Host, r.URL)
		http.NotFound(w, r)
	})
	return mux
}

// tvpSearch finds the products of the kind (SERIAL or VOD) whose title or
// original title is the keyword.
func (s *fakeSites) tvpSearch(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	products := append([]tvpProduct{czasHonoru}, s.films...)
	s.mu.Unlock()
	want := provider.NormalizeTitle(r.URL.Query().Get("keyword"))
	items := []tvpProduct{}
	for _, p := range products {
		if p.Type == r.PathValue("kind") &&
			(provider.NormalizeTitle(p.Title) == want || provider.NormalizeTitle(p.OriginalTitle) == want) {
			items = append(items, p)
		}
	}
	writeJSON(w, map[string]any{"items": items})
}

// known reports whether id is an episode or film on TVP.
func (s *fakeSites) known(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range append(czasHonoruEpisodes(), s.films...) {
		if strconv.FormatInt(p.ID, 10) == id {
			return true
		}
	}
	return false
}

// skyhook gives Days of Honor's first three episodes. The third aired
// yesterday, so the series feed offers it.
func (s *fakeSites) skyhook(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") != strconv.Itoa(daysOfHonorTVDB) {
		http.NotFound(w, r)
		return
	}
	episode := func(n int, title string, aired time.Time) map[string]any {
		return map[string]any{"seasonNumber": 1, "episodeNumber": n, "title": title, "airDateUtc": aired.UTC().Format(time.RFC3339)}
	}
	writeJSON(w, map[string]any{
		"tvdbId": daysOfHonorTVDB,
		"title":  "Days of Honor",
		"imdbId": "tt1287566",
		"episodes": []any{
			episode(1, "Jump", time.Date(2008, 9, 6, 22, 0, 0, 0, time.UTC)),
			episode(2, "On Polish soil", time.Date(2008, 9, 13, 22, 0, 0, 0, time.UTC)),
			episode(3, "Assignation", time.Now().Add(-24*time.Hour)),
		},
	})
}

func tvpNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	io.WriteString(w, `{"code":"ITEM_NOT_FOUND"}`)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// serveProxy answers HTTPS_PROXY's CONNECT requests for the faked sites
// with a tunnel to sites, and refuses any other.
func (s *fakeSites) serveProxy(ln net.Listener, sites string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go s.tunnel(c, sites)
	}
}

func (s *fakeSites) tunnel(c net.Conn, sites string) {
	defer c.Close()
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	host, _, _ := net.SplitHostPort(req.Host)
	if req.Method != http.MethodConnect || !slices.Contains(fakedHosts, host) {
		s.t.Errorf("magnetowid asked the proxy for %s %s, which isn't faked", req.Method, req.Host)
		io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
		return
	}
	up, err := net.Dial("tcp", sites)
	if err != nil {
		io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer up.Close()
	io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
	go func() {
		io.Copy(up, br) // br holds anything read past the request
		up.(*net.TCPConn).CloseWrite()
	}()
	io.Copy(c, up)
}

// selfSignedCert returns a certificate for hosts that signs itself, and
// its PEM.
func selfSignedCert(t *testing.T, hosts []string) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "magnetowid test sites"},
		DNSNames:              hosts,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// makeStream makes an 11-minute stream in dir shaped like TVP's: fMP4 HLS
// with the audio apart, tagged Polish, and TTML subtitles. Sonarr and Radarr
// take anything shorter than 10 minutes for a sample. The master playlist
// gives TVP's bandwidth, so the release's size is a real one's.
func makeStream(ctx context.Context, t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=black:s=1920x1080:r=1",
		"-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo",
		"-t", "660", "-map", "0:v", "-map", "1:a",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "60", "-c:a", "aac", "-b:a", "32k",
		"-f", "hls", "-hls_time", "60", "-hls_playlist_type", "vod", "-hls_segment_type", "fmp4",
		"-var_stream_map", "v:0 a:0",
		"-hls_fmp4_init_filename", "init_%v.mp4", "-hls_segment_filename", "seg_%v_%03d.m4s", "media_%v.m3u8")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("making the stream: %v\n%s", err, out)
	}
	master := fmt.Sprintf(`#EXTM3U
#EXT-X-VERSION:7
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio0",LANGUAGE="pl",NAME="Polski",AUTOSELECT=YES,DEFAULT=YES,CHANNELS="2",URI="media_1.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=1920x1080,FRAME-RATE=1.000,CODECS="avc1.42c028,mp4a.40.2",AUDIO="audio0"
media_0.m3u8
`, 5118260)
	if err := os.WriteFile(filepath.Join(dir, "master.m3u8"), []byte(master), 0o644); err != nil {
		t.Fatal(err)
	}
	subtitles := `<?xml version="1.0" encoding="UTF-8"?>
<tt xml:lang="pl-PL" xmlns="http://www.w3.org/ns/ttml" xmlns:tts="http://www.w3.org/ns/ttml#styling" xmlns:ttp="http://www.w3.org/ns/ttml#parameter" ttp:frameRate="25">
  <body>
    <div>
      <p begin="00:00:01.000" end="00:00:03.000">Usta, cięcie!</p>
      <p begin="00:00:04.000" end="00:00:06.000"><span tts:color="#16F158">Imion nie zmieniono.</span></p>
    </div>
  </body>
</tt>
`
	if err := os.WriteFile(filepath.Join(dir, "subtitles.xml"), []byte(subtitles), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}
