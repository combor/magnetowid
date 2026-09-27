// Package hls reads HLS master playlists and picks the variant vodarr
// downloads.
package hls

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/combor/vodarr/internal/provider"
)

// RFC 6381 codec prefixes of video codecs.
var videoCodecs = []string{"avc1", "avc3", "hvc1", "hev1", "dvh1", "dvhe", "vp09", "av01", "mp4v"}

// Variant is one #EXT-X-STREAM-INF entry.
type Variant struct {
	URI           string
	Width, Height int
	Bandwidth     int64  // BANDWIDTH, peak bits per second
	Average       int64  // AVERAGE-BANDWIDTH, 0 if not given
	Codecs        string // RFC 6381, e.g. "avc1.640029,mp4a.40.2"
	audio         string // AUDIO group ID
}

// Master is a master playlist's best video variant and its audio rendition,
// with URIs resolved against the playlist's final URL.
type Master struct {
	Video Variant
	Audio string // "" when the audio is in the variant
}

// Load fetches s and picks its best variant. ok is false when s isn't an HLS
// master playlist; URLs not ending in .m3u8 aren't fetched.
func Load(ctx context.Context, client *http.Client, s provider.Stream) (m Master, ok bool, err error) {
	base, err := url.Parse(s.URL)
	if err != nil || !strings.HasSuffix(strings.ToLower(base.Path), ".m3u8") {
		return Master{}, false, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return Master{}, false, err
	}
	for k, vs := range s.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Master{}, false, fmt.Errorf("fetching playlist: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Master{}, false, fmt.Errorf("fetching playlist: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Master{}, false, fmt.Errorf("fetching playlist: %w", err)
	}

	video, audio, ok := selectRenditions(string(body))
	if !ok {
		return Master{}, false, nil
	}
	base = resp.Request.URL // after redirects
	video.URI = resolve(base, video.URI)
	if audio != "" {
		audio = resolve(base, audio)
	}
	return Master{Video: video, Audio: audio}, true, nil
}

type rendition struct {
	group, uri          string
	isDefault, autoPick bool
}

// selectRenditions returns the best video variant and its separate audio
// rendition URI, if any. ok is false for anything but a usable master
// playlist.
func selectRenditions(playlist string) (video Variant, audio string, ok bool) {
	var variants []Variant
	var audios []rendition
	var pending map[string]string
	sc := bufio.NewScanner(strings.NewReader(playlist))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			pending = parseAttrs(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
		case strings.HasPrefix(line, "#EXT-X-MEDIA:"):
			a := parseAttrs(strings.TrimPrefix(line, "#EXT-X-MEDIA:"))
			if a["TYPE"] == "AUDIO" {
				audios = append(audios, rendition{
					group: a["GROUP-ID"], uri: a["URI"],
					isDefault: a["DEFAULT"] == "YES", autoPick: a["AUTOSELECT"] == "YES",
				})
			}
		case line == "" || strings.HasPrefix(line, "#"):
		case pending != nil:
			bw, _ := strconv.ParseInt(pending["BANDWIDTH"], 10, 64)
			avg, _ := strconv.ParseInt(pending["AVERAGE-BANDWIDTH"], 10, 64)
			w, h := dimensions(pending["RESOLUTION"])
			variants = append(variants, Variant{
				URI: line, Width: w, Height: h, Bandwidth: bw, Average: avg,
				Codecs: pending["CODECS"], audio: pending["AUDIO"],
			})
			pending = nil
		}
	}

	candidates := variants[:0]
	for _, v := range variants {
		if hasVideo(v.Codecs) {
			candidates = append(candidates, v)
		}
	}
	if len(candidates) == 0 {
		return Variant{}, "", false
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		pi, pj := candidates[i].Width*candidates[i].Height, candidates[j].Width*candidates[j].Height
		if pi != pj {
			return pi > pj
		}
		return candidates[i].Bandwidth > candidates[j].Bandwidth
	})
	best := candidates[0]
	return best, pickAudio(audios, best.audio), true
}

// pickAudio returns the URI of the group's DEFAULT, else AUTOSELECT, else
// first rendition. "" means the audio is muxed into the variant.
func pickAudio(audios []rendition, group string) string {
	if group == "" {
		return ""
	}
	var first, auto *rendition
	for i := range audios {
		a := &audios[i]
		if a.group != group {
			continue
		}
		if a.isDefault {
			return a.uri
		}
		if first == nil {
			first = a
		}
		if auto == nil && a.autoPick {
			auto = a
		}
	}
	if auto != nil {
		return auto.uri
	}
	if first != nil {
		return first.uri
	}
	return ""
}

// hasVideo reports whether CODECS names a video codec; an empty CODECS counts.
func hasVideo(codecs string) bool {
	if codecs == "" {
		return true
	}
	for _, c := range strings.Split(codecs, ",") {
		c = strings.TrimSpace(c)
		for _, v := range videoCodecs {
			if strings.HasPrefix(c, v) {
				return true
			}
		}
	}
	return false
}

// dimensions parses a RESOLUTION such as "1920x1080"; 0, 0 if absent.
func dimensions(resolution string) (width, height int) {
	w, h, ok := strings.Cut(resolution, "x")
	if !ok {
		return 0, 0
	}
	width, _ = strconv.Atoi(w)
	height, _ = strconv.Atoi(h)
	return width, height
}

// parseAttrs parses an HLS attribute list.
func parseAttrs(s string) map[string]string {
	attrs := map[string]string{}
	for s != "" {
		key, rest, ok := strings.Cut(s, "=")
		if !ok {
			break
		}
		var val string
		if strings.HasPrefix(rest, `"`) {
			val, rest, _ = strings.Cut(rest[1:], `"`)
		} else {
			val, rest, _ = strings.Cut(rest, ",")
			rest = "," + rest
		}
		attrs[strings.TrimSpace(key)] = val
		s = strings.TrimPrefix(rest, ",")
	}
	return attrs
}

func resolve(base *url.URL, ref string) string {
	u, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return base.ResolveReference(u).String()
}
