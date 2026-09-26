package downloader

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
	"time"

	"github.com/combor/vodarr/internal/provider"
)

var defaultClient = &http.Client{Timeout: 30 * time.Second}

// Codec prefixes (RFC 6381) that mark a variant as carrying video.
var videoCodecs = []string{"avc1", "avc3", "hvc1", "hev1", "dvh1", "dvhe", "vp09", "av01", "mp4v"}

// pickInputs returns the URLs ffmpeg should read. Given an HLS master
// playlist, ffmpeg would download the start of every variant before choosing
// one (seconds of sequential requests), so vodarr chooses instead: the
// highest-resolution video variant and its audio rendition, if separate.
// Anything else (media playlists, DASH, files) is passed through unchanged.
func pickInputs(ctx context.Context, client *http.Client, s provider.Stream) ([]string, error) {
	base, err := url.Parse(s.URL)
	if err != nil || !strings.HasSuffix(strings.ToLower(base.Path), ".m3u8") {
		return []string{s.URL}, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range s.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching playlist: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("fetching playlist: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("fetching playlist: %w", err)
	}

	video, audio, ok := selectRenditions(string(body))
	if !ok {
		return []string{s.URL}, nil
	}
	base = resp.Request.URL // relative URIs are relative to the playlist after redirects
	inputs := []string{resolve(base, video)}
	if audio != "" {
		inputs = append(inputs, resolve(base, audio))
	}
	return inputs, nil
}

type variant struct {
	uri       string
	bandwidth int64
	pixels    int
	audio     string // AUDIO group ID
	codecs    string
}

type rendition struct {
	group, uri          string
	isDefault, autoPick bool
}

// selectRenditions picks from a master playlist the best video variant URI
// and, when audio comes as a separate rendition, its URI. ok is false when
// the playlist is not a master playlist or has no usable variant.
func selectRenditions(playlist string) (video, audio string, ok bool) {
	var variants []variant
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
			variants = append(variants, variant{
				uri: line, bandwidth: bw, pixels: pixels(pending["RESOLUTION"]),
				audio: pending["AUDIO"], codecs: pending["CODECS"],
			})
			pending = nil
		}
	}

	candidates := variants[:0]
	for _, v := range variants {
		if hasVideo(v.codecs) {
			candidates = append(candidates, v)
		}
	}
	if len(candidates) == 0 {
		return "", "", false
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].pixels != candidates[j].pixels {
			return candidates[i].pixels > candidates[j].pixels
		}
		return candidates[i].bandwidth > candidates[j].bandwidth
	})
	best := candidates[0]
	return best.uri, pickAudio(audios, best.audio), true
}

// pickAudio returns the URI of the group's DEFAULT rendition, else its
// AUTOSELECT one, else its first. "" means audio is muxed into the variant:
// either the group is empty or the chosen rendition has no URI.
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

// hasVideo reports whether CODECS names a video codec. Variants without
// CODECS are assumed to carry video.
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

func pixels(resolution string) int {
	w, h, ok := strings.Cut(resolution, "x")
	if !ok {
		return 0
	}
	wi, _ := strconv.Atoi(w)
	hi, _ := strconv.Atoi(h)
	return wi * hi
}

// parseAttrs parses an HLS attribute list: KEY=VALUE pairs separated by
// commas, where quoted values may contain commas.
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
