package bbc

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/combor/magnetowid/internal/hls"
	"github.com/combor/magnetowid/internal/provider"
	"github.com/combor/magnetowid/internal/subtitles"
)

// Set MAGNETOWID_LIVE to check iPlayer and Skyhook; CI runs this daily.
// iPlayer refusing streams outside the UK is an expected result.
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

	// The Traitors: "Series 4: 12. The Final" and "Series 4: Episode 3", with
	// TVDB's "The Final" also in season 3. iPlayer keeps past series.
	t.Run("series by TVDB ID", func(t *testing.T) {
		for ep, subtitle := range map[int]string{3: "Series 4: Episode 3", 12: "Series 4: 12. The Final"} {
			title, items, err := p.SearchTVDB(ctx, 420543, provider.Query{Kind: provider.Episode, Season: 4, Episode: ep})
			if err != nil || title != "The Traitors" || len(items) != 1 || items[0].Duration < 40*time.Minute {
				t.Fatalf("S04E%02d: SearchTVDB = %q, %+v, %v", ep, title, items, err)
			}
			if !strings.HasSuffix(items[0].Title, subtitle) {
				t.Errorf("S04E%02d is %q, want %q", ep, items[0].Title, subtitle)
			}
		}
		_, items, err := p.SearchTVDB(ctx, 420543, provider.Query{Kind: provider.Episode, Season: 4})
		if err != nil || len(items) != 12 {
			t.Fatalf("season 4: %d items, %v", len(items), err)
		}
		s, ok := checkStream(ctx, t, p, client, items[11].ID)
		if !ok {
			return
		}
		// One copy per CDN; each must work.
		if len(s.Subtitles) == 0 {
			t.Fatal("no subtitles")
		}
		for _, sub := range s.Subtitles {
			srt, err := subtitles.Fetch(ctx, client, sub, s.Header)
			if n := bytes.Count(srt, []byte(" --> ")); err != nil || n < 100 {
				t.Errorf("subtitles: %d cues, %v", n, err)
			} else {
				t.Logf("subtitles: %d cues", n)
			}
		}
	})

	// Sonarr's title searches drop parentheses; "Doctor Who" is the classic series.
	t.Run("series by title", func(t *testing.T) {
		for title, want := range map[string]int{"Doctor Who 2023": 449991, "Doctor Who": 76107} {
			if id, ok, err := p.titles.resolve(ctx, title); err != nil || !ok || id != want {
				t.Errorf("%q is TVDB %d, %v, %v; want %d", title, id, ok, err, want)
			}
		}
		items, err := p.Search(ctx, provider.Query{Kind: provider.Episode, Title: "Doctor Who 2023", Season: 1, Episode: 1})
		if err != nil || len(items) != 1 {
			t.Errorf("Doctor Who 2023 S01E01: %+v, %v", items, err)
		}
	})

	// EastEnders is numbered by year on TVDB and dated on iPlayer.
	t.Run("soap by air date", func(t *testing.T) {
		s, err := p.titles.series(ctx, 71753)
		if err != nil {
			t.Fatal(err)
		}
		matched, found, err := p.matchSeries(ctx, s, provider.SeriesOverride{})
		if err != nil || !found || len(matched) < 100 {
			t.Fatalf("%d EastEnders episodes matched, found %v, %v", len(matched), found, err)
		}
		// Titles vary: TVDB's "04/07/2022 (1)" is BBC's "04/07/2022 - Part 1".
		for _, m := range matched {
			bt, bn := titleKey(m.bbc.OriginalTitle)
			tt, tn := titleKey(m.tvdb.title)
			if m.bbc.aired() != m.tvdb.aired && (bt != tt || bn != tn) {
				t.Errorf("TVDB S%dE%d %q (%s) matched %q (%s)", m.tvdb.season, m.tvdb.episode,
					m.tvdb.title, m.tvdb.aired, m.bbc.Subtitle, m.bbc.aired())
			}
		}
		eps, _ := p.episodes(ctx, "b006m86d")
		t.Logf("matched %d of iPlayer's %d EastEnders episodes", len(matched), len(eps))
	})

	t.Run("newest films", func(t *testing.T) {
		films, err := p.newestFilms(ctx)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, f := range films {
			if f.oneOff() && f.year() > 1900 && !f.available().IsZero() && f.duration() > time.Hour {
				n++
			}
		}
		if n < 50 {
			t.Fatalf("%d of iPlayer's %d newest films have a year, date and duration", n, len(films))
		}
		// Search must find a recent film by title and year.
		for _, f := range films[:10] {
			items, err := p.Search(ctx, provider.Query{Kind: provider.Movie, Title: f.Title, Year: f.year()})
			if err != nil {
				t.Fatal(err)
			}
			for _, it := range items {
				if it.ID == f.ID {
					checkStream(ctx, t, p, client, it.ID)
					return
				}
			}
		}
		t.Error("search finds none of iPlayer's 10 newest films")
	})
}

// A region block is expected outside the UK.
func checkStream(ctx context.Context, t *testing.T, p *Provider, client *http.Client, id string) (provider.Stream, bool) {
	t.Helper()
	s, err := p.Resolve(ctx, id)
	if errors.Is(err, provider.ErrUnavailable) && strings.Contains(err.Error(), "only to the UK") {
		t.Logf("item %s is refused outside the UK, as expected there", id)
		return provider.Stream{}, false
	}
	if err != nil {
		t.Fatalf("item %s: %v", id, err)
	}
	m, ok, err := hls.Load(ctx, client, s)
	if err != nil || !ok {
		t.Fatalf("item %s: stream %s: HLS master %v, %v", id, s.URL, ok, err)
	}
	if m.Video.Height < 720 {
		t.Errorf("item %s: best variant %+v", id, m.Video)
	}
	t.Logf("item %s: %dx%d, %s, %d bit/s", id, m.Video.Width, m.Video.Height, m.Video.Codecs, m.Video.Bandwidth)
	return s, true
}
