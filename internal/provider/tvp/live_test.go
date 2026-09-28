package tvp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/combor/magnetowid/internal/hls"
	"github.com/combor/magnetowid/internal/provider"
	"github.com/combor/magnetowid/internal/subtitles"
)

// TestLive checks the real TVP API, Skyhook and Wikidata for what magnetowid
// reads from them, which the other tests here and cmd/magnetowid's fakes
// stand in for. None of them promises to stay the same. Set MAGNETOWID_LIVE
// to run it; CI runs it daily. Outside Poland TVP refuses streams, which
// counts as working.
func TestLive(t *testing.T) {
	if os.Getenv("MAGNETOWID_LIVE") == "" {
		t.Skip("MAGNETOWID_LIVE is unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 30 * time.Second}
	p, err := New(client, slog.New(slog.NewTextHandler(os.Stderr, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}

	// Czas honoru, TVDB's Days of Honor, ended in 2014 and is TVP's own.
	t.Run("series by TVDB ID", func(t *testing.T) {
		s, err := p.titles.series(ctx, 83920)
		if err != nil {
			t.Fatal(err)
		}
		if s.title != "Days of Honor" || !slices.Contains(s.polish, "Czas honoru") {
			t.Errorf("titles %q and %q, want Days of Honor and Czas honoru", s.title, s.polish)
		}
		if len(s.episodes) < 13 || s.episodes[0].aired.IsZero() {
			t.Errorf("%d episodes, the first aired %v", len(s.episodes), s.episodes[0].aired)
		}
		title, items, err := p.SearchTVDB(ctx, 83920, provider.Query{Kind: provider.Episode, Season: 1, Episode: 2})
		if err != nil || title != "Days of Honor" || len(items) != 1 || items[0].Duration < 40*time.Minute {
			t.Fatalf("SearchTVDB = %q, %+v, %v", title, items, err)
		}
		stream, ok := checkStream(ctx, t, p, client, items[0].ID)
		if !ok {
			return
		}
		// TVP's own series have subtitles for the deaf and hard of hearing.
		i := slices.IndexFunc(stream.Subtitles, func(sub provider.Subtitle) bool { return sub.Language == "pol" && sub.SDH })
		if i < 0 {
			t.Fatalf("subtitles %+v, want Polish ones for the deaf and hard of hearing", stream.Subtitles)
		}
		srt, err := subtitles.Fetch(ctx, client, stream.Subtitles[i], stream.Header)
		if n := bytes.Count(srt, []byte(" --> ")); err != nil || n < 100 {
			t.Errorf("subtitles: %d cues, %v", n, err)
		} else {
			t.Logf("subtitles: %d cues", n)
		}
	})

	t.Run("newest films", func(t *testing.T) {
		var res struct {
			Items []product `json:"items"`
		}
		params := url.Values{"sort": {"createdAt"}, "order": {"desc"}, "maxResults": {strconv.Itoa(newestProducts)}}
		if err := p.get(ctx, "vods", params, &res); err != nil {
			t.Fatal(err)
		}
		var films []product
		for _, v := range res.Items {
			if v.Type == "VOD" && !v.Payable && v.Title != "" && v.Year > 0 && !parseTime(v.Since).IsZero() {
				films = append(films, v)
			}
		}
		if len(films) < 10 {
			t.Fatalf("%d of TVP's %d newest products are free films with a title, year and date", len(films), len(res.Items))
		}
		// The search gives original titles; a film just added may not be in it yet.
		for _, v := range films[:10] {
			if _, found, err := p.originalTitle(ctx, v); err != nil {
				t.Fatal(err)
			} else if found {
				checkStream(ctx, t, p, client, strconv.FormatInt(v.ID, 10))
				return
			}
		}
		t.Error("TVP's search finds none of its 10 newest free films")
	})

	t.Run("TV guide", func(t *testing.T) {
		now := time.Now()
		params := url.Values{
			"liveId[]": guideChannels,
			"since":    {now.Add(-24 * time.Hour).UTC().Format(guideTimeLayout)},
			"till":     {now.UTC().Format(guideTimeLayout)},
		}
		var programmes []programme
		if err := p.get(ctx, "lives/programmes", params, &programmes); err != nil {
			t.Fatal(err)
		}
		episodes := 0
		for _, pr := range programmes {
			if _, err := time.Parse(time.RFC3339, pr.Since); err != nil {
				t.Errorf("%q: %v", pr.Title, err)
			}
			if programmeTitle.MatchString(pr.Title) {
				episodes++
			}
		}
		if episodes == 0 {
			t.Errorf("none of the day's %d programmes on TVP 1 and 2 names its episode like %q", len(programmes), programmeTitle)
		}
	})
}

// checkStream checks that the item resolves to an HLS stream whose audio
// has a language, and returns it, or that it is refused for being outside
// Poland.
func checkStream(ctx context.Context, t *testing.T, p *Provider, client *http.Client, id string) (provider.Stream, bool) {
	t.Helper()
	s, err := p.Resolve(ctx, id)
	if errors.Is(err, provider.ErrUnavailable) && strings.Contains(err.Error(), "GEOIP_FILTER_FAILED") {
		t.Logf("item %s is refused outside Poland, as expected there", id)
		return provider.Stream{}, false
	}
	if err != nil {
		t.Fatalf("item %s: %v", id, err)
	}
	m, ok, err := hls.Load(ctx, client, s)
	if err != nil || !ok {
		t.Fatalf("item %s: stream %s: HLS master %v, %v", id, s.URL, ok, err)
	}
	if m.Video.Height == 0 || m.AudioLanguage == "" {
		t.Errorf("item %s: best variant %+v, audio language %q", id, m.Video, m.AudioLanguage)
	}
	t.Logf("item %s: %dx%d, %s, audio in %q", id, m.Video.Width, m.Video.Height, m.Video.Codecs, m.AudioLanguage)
	return s, true
}
