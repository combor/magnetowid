// Package hls selects video and audio from HLS master playlists.
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

	"github.com/combor/magnetowid/internal/provider"
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

// Master holds the selected streams, with URLs resolved after redirects.
type Master struct {
	Video         Variant
	Audio         string // "" when the audio is in the variant
	AudioLanguage string // RFC 5646 LANGUAGE tag, e.g. "pl"; empty if absent
}

// Load selects streams from an HLS master playlist. Non-master playlists return
// ok=false; URLs without a .m3u8 suffix are not fetched.
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
	if audio.uri != "" {
		audio.uri = resolve(base, audio.uri)
	}
	return Master{Video: video, Audio: audio.uri, AudioLanguage: audio.language}, true, nil
}

type rendition struct {
	group, uri, language string
	isDefault, autoPick  bool
}

// An empty audio URI means muxed audio; ok=false means no usable master playlist.
func selectRenditions(playlist string) (video Variant, audio rendition, ok bool) {
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
					group: a["GROUP-ID"], uri: a["URI"], language: a["LANGUAGE"],
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
		return Variant{}, rendition{}, false
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

// Prefer DEFAULT, then AUTOSELECT, then the group's first rendition.
func pickAudio(audios []rendition, group string) rendition {
	if group == "" {
		return rendition{}
	}
	var first, auto *rendition
	for i := range audios {
		a := &audios[i]
		if a.group != group {
			continue
		}
		if a.isDefault {
			return *a
		}
		if first == nil {
			first = a
		}
		if auto == nil && a.autoPick {
			auto = a
		}
	}
	if auto != nil {
		return *auto
	}
	if first != nil {
		return *first
	}
	return rendition{}
}

// Missing CODECS is treated as video.
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

func dimensions(resolution string) (width, height int) {
	w, h, ok := strings.Cut(resolution, "x")
	if !ok {
		return 0, 0
	}
	width, _ = strconv.Atoi(w)
	height, _ = strconv.Atoi(h)
	return width, height
}

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
