package bbc

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/combor/magnetowid/internal/hls"
	"github.com/combor/magnetowid/internal/provider"
)

// The media selector lists a version's streams and subtitles for a device
// class ("mediaset"). iptv-all offers 720p50 HLS with 128 kbit/s audio;
// pc tops out at 540p but may cover versions iptv-all lacks.
var mediaSets = []string{"iptv-all", "pc"}

// iPlayer serves 1080p50 to TVs over DASH only, but the HLS origin also
// packages it under this track name, which masters omit. get_iplayer uses
// the same rendition.
const fullHDTrack = "video=12000000"

var videoTrack = regexp.MustCompile(`video=\d+`)

// Resolve at download time: stream URLs carry tokens that expire after 6 hours.
func (p *Provider) Resolve(ctx context.Context, id string) (provider.Stream, error) {
	if !pidPattern.MatchString(id) {
		return provider.Stream{}, fmt.Errorf("bbc: invalid id %q", id)
	}
	ep, ok, err := p.episode(ctx, id)
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.permanent() {
		return provider.Stream{}, fmt.Errorf("%w: %w", provider.ErrUnavailable, err)
	}
	if err != nil {
		return provider.Stream{}, err
	}
	if !ok {
		return provider.Stream{}, fmt.Errorf("%w: bbc episode %s is not available", provider.ErrUnavailable, id)
	}
	v, ok := mainVersion(ep.Versions)
	if !ok {
		return provider.Stream{}, fmt.Errorf("%w: bbc episode %s has no main version", provider.ErrUnavailable, id)
	}
	sel, err := p.selectMedia(ctx, v.ID)
	if err != nil {
		return provider.Stream{}, err
	}
	s, err := p.hlsStream(ctx, sel)
	if err != nil {
		return provider.Stream{}, err
	}
	// BBC's subtitles name speakers by colour and describe sounds.
	if c := sel.captions(); c != "" {
		s.Subtitles = []provider.Subtitle{{URL: c, Format: provider.TTML, Language: "eng", SDH: true}}
	}
	return s, nil
}

type selection struct {
	Result string  `json:"result"` // an error, e.g. "geolocation"
	Media  []media `json:"media"`
}

type media struct {
	Kind        string       `json:"kind"` // "video", "captions", ...
	Height      string       `json:"height"`
	Bitrate     string       `json:"bitrate"` // kbit/s
	Connections []connection `json:"connection"`
}

type connection struct {
	Href           string `json:"href"`
	Protocol       string `json:"protocol"`
	TransferFormat string `json:"transferFormat"` // "hls", "dash" or "plain"
	Priority       string `json:"priority"`       // lowest first
}

// Return HTTPS URLs in priority order.
func (m media) urls(format string) []string {
	var cs []connection
	for _, c := range m.Connections {
		if c.Protocol == "https" && c.TransferFormat == format && c.Href != "" {
			cs = append(cs, c)
		}
	}
	slices.SortStableFunc(cs, func(a, b connection) int {
		pa, _ := strconv.Atoi(a.Priority)
		pb, _ := strconv.Atoi(b.Priority)
		return cmp.Compare(pa, pb)
	})
	urls := make([]string, len(cs))
	for i, c := range cs {
		urls[i] = c.Href
	}
	return urls
}

func (s selection) captions() string {
	for _, m := range s.Media {
		if m.Kind == "captions" {
			if urls := m.urls("plain"); len(urls) > 0 {
				return urls[0]
			}
		}
	}
	return ""
}

// Try each mediaset until one has HLS video.
func (p *Provider) selectMedia(ctx context.Context, vpid string) (selection, error) {
	last := ""
	for _, set := range mediaSets {
		body, err := p.fetch(ctx, p.selectorURL+"/mediaset/"+set+"/vpid/"+url.PathEscape(vpid)+"/format/json", 1<<20)
		var apiErr *apiError
		if err != nil && !errors.As(err, &apiErr) {
			return selection{}, err
		}
		// Refusals explain themselves in JSON.
		var sel selection
		jsonErr := json.Unmarshal(body, &sel)
		switch {
		case sel.Result == "geolocation" || sel.Result == "notukerror":
			return selection{}, fmt.Errorf("%w: BBC iPlayer streams only to the UK (%s)", provider.ErrUnavailable, sel.Result)
		case sel.Result != "":
			last = sel.Result // e.g. selectionunavailable
			continue
		case err != nil && apiErr.permanent():
			last = err.Error()
			continue
		case err != nil:
			return selection{}, err
		case jsonErr != nil:
			return selection{}, fmt.Errorf("bbc: decoding media selection: %w", jsonErr)
		}
		if _, ok := sel.video(); ok {
			return sel, nil
		}
		last = "no HLS video"
	}
	return selection{}, fmt.Errorf("%w: bbc version %s: %s", provider.ErrUnavailable, vpid, last)
}

// Return the tallest video with HLS.
func (s selection) video() (media, bool) {
	var best media
	found := false
	for _, m := range s.Media {
		if m.Kind != "video" || len(m.urls("hls")) == 0 {
			continue
		}
		if !found || atoi(m.Height) > atoi(best.Height) {
			best, found = m, true
		}
	}
	return best, found
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// Fall back to other CDNs if the first refuses the master playlist.
func (p *Provider) hlsStream(ctx context.Context, sel selection) (provider.Stream, error) {
	video, _ := sel.video()
	var err error
	for _, u := range video.urls("hls") {
		var body []byte
		var final string
		if body, final, err = p.fetchFinal(ctx, u, 1<<20); err != nil {
			if ctx.Err() != nil {
				break
			}
			continue
		}
		s := provider.Stream{URL: final, Header: http.Header{"User-Agent": {userAgent}}, Playlist: string(body)}
		if atoi(video.Height) >= 1080 && atoi(video.Bitrate) > 0 {
			s.Playlist = p.addFullHD(ctx, s, atoi(video.Bitrate)*1000)
		}
		return s, nil
	}
	return provider.Stream{}, fmt.Errorf("bbc: no CDN serves the playlist: %w", err)
}

// Add the 1080p rendition to the master if the origin serves it, keeping the
// best listed variant's audio. bitrate is the media selector's for 1080p video.
// Failures keep the master as it was.
func (p *Provider) addFullHD(ctx context.Context, s provider.Stream, bitrate int) string {
	m, ok, err := hls.Load(ctx, p.client, s)
	if err != nil || !ok || m.Video.Height < 720 || !videoTrack.MatchString(m.Video.URI) {
		return s.Playlist
	}
	uri := videoTrack.ReplaceAllString(m.Video.URI, fullHDTrack)
	body, err := p.fetch(ctx, uri, 4<<20)
	if err != nil || !strings.Contains(string(body), "#EXTINF") {
		p.log.Debug("BBC's 1080p rendition is missing; offering the best listed", "err", err)
		return s.Playlist
	}
	// Add the listed variant's audio and overhead to the video bitrate.
	listed, _ := strconv.Atoi(strings.TrimPrefix(videoTrack.FindString(m.Video.URI), "video="))
	bandwidth := int64(bitrate) + max(m.Video.Bandwidth-int64(listed), 0)
	codecs := ""
	if m.Video.Codecs != "" {
		codecs = fmt.Sprintf(`,CODECS="%s"`, m.Video.Codecs)
	}
	return strings.TrimRight(s.Playlist, "\n") + fmt.Sprintf(
		"\n#EXT-X-STREAM-INF:BANDWIDTH=%d%s,RESOLUTION=1920x1080,FRAME-RATE=50\n%s\n", bandwidth, codecs, uri)
}
