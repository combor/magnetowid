package hls

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrUnsupported marks media playlists whose segments can't simply be fetched
// one by one, e.g. live or encrypted ones.
var ErrUnsupported = errors.New("unsupported media playlist")

// Segment is one media segment of a VOD playlist.
type Segment struct {
	URI      string // absolute
	Duration time.Duration
	// Init is the absolute URI of the #EXT-X-MAP initialization section, "" if none.
	Init string
	// Timestamps may jump from the previous segment.
	Discontinuity bool
}

// LoadMedia fetches a VOD media playlist with header h and lists its segments.
func LoadMedia(ctx context.Context, client *http.Client, uri string, h http.Header) ([]Segment, error) {
	playlist, base, err := fetch(ctx, client, uri, h)
	if err != nil {
		return nil, fmt.Errorf("fetching media playlist: %w", err)
	}
	return parseMedia(playlist, base)
}

func parseMedia(playlist string, base *url.URL) ([]Segment, error) {
	unsupported := func(why string) ([]Segment, error) {
		return nil, fmt.Errorf("%w: %s", ErrUnsupported, why)
	}
	var segs []Segment
	var next Segment
	var init string
	var timed, ended bool
	sc := bufio.NewScanner(strings.NewReader(playlist))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		tag, value, _ := strings.Cut(line, ":")
		switch {
		case line == "":
		case tag == "#EXTINF":
			d, _, _ := strings.Cut(value, ",")
			secs, err := strconv.ParseFloat(strings.TrimSpace(d), 64)
			if err != nil || !(secs >= 0) || math.IsInf(secs, 1) {
				return unsupported(fmt.Sprintf("duration %q", d))
			}
			next.Duration = time.Duration(math.Round(secs * float64(time.Second)))
			timed = true
		case tag == "#EXT-X-MAP":
			a := parseAttrs(value)
			if a["URI"] == "" || a["BYTERANGE"] != "" {
				return unsupported("initialization section " + value)
			}
			init = resolve(base, a["URI"])
		case tag == "#EXT-X-KEY":
			if a := parseAttrs(value); a["METHOD"] != "NONE" {
				return unsupported("encrypted with " + a["METHOD"])
			}
		case tag == "#EXT-X-DISCONTINUITY":
			next.Discontinuity = true
		case tag == "#EXT-X-ENDLIST":
			ended = true
		case tag == "#EXT-X-BYTERANGE", tag == "#EXT-X-GAP", tag == "#EXT-X-DEFINE", tag == "#EXT-X-STREAM-INF":
			return unsupported(tag)
		case strings.HasPrefix(line, "#"):
		case !timed:
			return unsupported("segment without a duration")
		default:
			next.URI, next.Init = resolve(base, line), init
			segs = append(segs, next)
			next, timed = Segment{}, false
		}
	}
	switch {
	case !ended:
		return unsupported("live playlist")
	case len(segs) == 0:
		return unsupported("no segments")
	}
	return segs, nil
}
