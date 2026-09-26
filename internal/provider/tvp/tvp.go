// Package tvp is the provider for TVP VOD (https://vod.tvp.pl), Telewizja
// Polska's video-on-demand site. Its catalogue API needs no login.
package tvp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/combor/vodarr/internal/provider"
)

const (
	defaultBaseURL = "https://vod.tvp.pl/api/products"
	userAgent      = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130 Safari/537.36"
)

// Provider searches and resolves TVP VOD content.
type Provider struct {
	client  *http.Client
	baseURL string
	log     *slog.Logger
}

// New returns a TVP provider using client for API calls.
func New(client *http.Client, log *slog.Logger) *Provider {
	return &Provider{client: client, baseURL: defaultBaseURL, log: log}
}

func (p *Provider) Name() string { return "tvp" }

// product is the subset of TVP's product JSON (serials, seasons, episodes,
// movies) that vodarr uses.
type product struct {
	ID            int64  `json:"id"`
	Type          string `json:"type"`
	Title         string `json:"title"`
	OriginalTitle string `json:"originalTitle"`
	Year          int    `json:"year"`
	Number        int    `json:"number"`
	Duration      int    `json:"duration"` // seconds
	Payable       bool   `json:"payable"`
	Since         string `json:"since"`
}

func (p *Provider) Search(ctx context.Context, q provider.Query) ([]provider.Item, error) {
	switch q.Kind {
	case provider.Episode:
		return p.searchEpisodes(ctx, q)
	case provider.Movie:
		return p.searchMovies(ctx, q)
	}
	return nil, nil
}

// searchEpisodes finds the serial by exact title, then maps the *arr's
// season/episode onto TVP's numbers. TVP often numbers episodes across
// seasons (Ranczo's season 2 is 14-26), so episode N of a season is TVP
// number start+N-1, where start is where the season begins. Gaps and
// duplicate numbers never shift other episodes: a missing number yields no
// result rather than the next episode under the wrong name.
func (p *Provider) searchEpisodes(ctx context.Context, q provider.Query) ([]provider.Item, error) {
	if q.Season <= 0 {
		return nil, nil
	}
	serials, err := p.search(ctx, "SERIAL", q.Title)
	if err != nil {
		return nil, err
	}
	serial, ok := p.pick(serials, q)
	if !ok {
		return nil, nil
	}

	var seasons []product
	if err := p.get(ctx, fmt.Sprintf("vods/serials/%d/seasons", serial.ID), nil, &seasons); err != nil {
		return nil, err
	}
	season, ok := findSeason(seasons, q.Season)
	if !ok {
		return nil, nil
	}
	episodes, err := p.episodes(ctx, serial.ID, season.ID)
	if err != nil {
		return nil, err
	}
	start, ok, err := p.seasonStart(ctx, serial.ID, seasons, q.Season, episodes)
	if err != nil {
		return nil, err
	}
	if !ok {
		p.log.Warn("can't tell where a TVP season's numbering starts; skipping",
			"serial", serial.Title, "season", q.Season)
		return nil, nil
	}

	count := map[int]int{}
	for _, e := range episodes {
		count[e.Number]++
	}
	var items []provider.Item
	for _, e := range episodes {
		ep := e.Number - start + 1
		if count[e.Number] > 1 || ep < 1 || (q.Episode != 0 && ep != q.Episode) || e.Payable {
			continue // duplicate numbers are ambiguous
		}
		items = append(items, provider.Item{
			ID:        strconv.FormatInt(e.ID, 10),
			Kind:      provider.Episode,
			Title:     serial.Title + " – " + e.Title,
			Year:      e.Year,
			Season:    q.Season,
			Episode:   ep,
			Duration:  time.Duration(e.Duration) * time.Second,
			Published: parseTime(e.Since),
		})
	}
	return items, nil
}

// seasonStart returns the TVP number of the season's first episode, when it
// can be told for sure: 1 if the season's numbers start at 1 (per-season
// numbering, or the first season), or the number right after the previous
// season's last one if the season starts there. Otherwise the first episode
// may be missing, or the previous season may be incomplete, so ok is false.
func (p *Provider) seasonStart(ctx context.Context, serialID int64, seasons []product, number int, episodes []product) (start int, ok bool, err error) {
	if len(episodes) == 0 {
		return 0, false, nil
	}
	first := episodes[0].Number
	if first == 1 {
		return 1, true, nil
	}
	prev, found := findSeason(seasons, number-1)
	if !found {
		return 0, false, nil
	}
	prevEpisodes, err := p.episodes(ctx, serialID, prev.ID)
	if err != nil || len(prevEpisodes) == 0 {
		return 0, false, err
	}
	if first == prevEpisodes[len(prevEpisodes)-1].Number+1 {
		return first, true, nil
	}
	return 0, false, nil
}

func findSeason(seasons []product, number int) (product, bool) {
	for _, s := range seasons {
		if s.Number == number {
			return s, true
		}
	}
	return product{}, false
}

// episodes returns a season's numbered episodes sorted by number; specials
// (numbered 0 or -1) are left out.
func (p *Provider) episodes(ctx context.Context, serialID, seasonID int64) ([]product, error) {
	var all []product
	if err := p.get(ctx, fmt.Sprintf("vods/serials/%d/seasons/%d/episodes", serialID, seasonID), nil, &all); err != nil {
		return nil, err
	}
	numbered := all[:0]
	for _, e := range all {
		if e.Number > 0 {
			numbered = append(numbered, e)
		}
	}
	sort.SliceStable(numbered, func(i, j int) bool { return numbered[i].Number < numbered[j].Number })
	return numbered, nil
}

func (p *Provider) searchMovies(ctx context.Context, q provider.Query) ([]provider.Item, error) {
	vods, err := p.search(ctx, "VOD", q.Title)
	if err != nil {
		return nil, err
	}
	want := provider.NormalizeTitle(q.Title)
	var items []provider.Item
	for _, v := range vods {
		if v.Payable || !titleMatches(v, want) {
			continue
		}
		if q.Year > 0 && v.Year > 0 && abs(v.Year-q.Year) > 1 {
			continue
		}
		items = append(items, provider.Item{
			ID:        strconv.FormatInt(v.ID, 10),
			Kind:      provider.Movie,
			Title:     v.Title,
			Year:      v.Year,
			Duration:  time.Duration(v.Duration) * time.Second,
			Published: parseTime(v.Since),
		})
	}
	return items, nil
}

// pick returns the first free serial whose title matches exactly; TVP's
// search is fuzzy and ranks by relevance.
func (p *Provider) pick(serials []product, q provider.Query) (product, bool) {
	want := provider.NormalizeTitle(q.Title)
	var matches []product
	for _, s := range serials {
		if !s.Payable && titleMatches(s, want) {
			matches = append(matches, s)
		}
	}
	if len(matches) == 0 {
		return product{}, false
	}
	if len(matches) > 1 {
		p.log.Warn("several TVP serials match; using the first", "title", q.Title, "count", len(matches))
	}
	return matches[0], true
}

func titleMatches(v product, normalized string) bool {
	return provider.NormalizeTitle(v.Title) == normalized ||
		(v.OriginalTitle != "" && provider.NormalizeTitle(v.OriginalTitle) == normalized)
}

func (p *Provider) search(ctx context.Context, kind, keyword string) ([]product, error) {
	var res struct {
		Items []product `json:"items"`
	}
	err := p.get(ctx, "vods/search/"+kind, url.Values{"keyword": {keyword}}, &res)
	return res.Items, err
}

// Resolve asks TVP for the current stream. Stream URLs embed the requester's
// IP and a date, so they are only valid for an immediate download.
func (p *Provider) Resolve(ctx context.Context, id string) (provider.Stream, error) {
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return provider.Stream{}, fmt.Errorf("tvp: invalid id %q", id)
	}
	var pl struct {
		Sources map[string][]struct {
			Src string `json:"src"`
		} `json:"sources"`
		DRM json.RawMessage `json:"drm"`
	}
	err := p.get(ctx, id+"/videos/playlist", url.Values{"videoType": {"MOVIE"}}, &pl)
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.permanent() {
		// e.g. GEOIP_FILTER_FAILED, ITEM_NOT_PAID
		return provider.Stream{}, fmt.Errorf("%w: tvp %s", provider.ErrUnavailable, apiErr.reason())
	}
	if err != nil {
		return provider.Stream{}, err
	}
	if len(pl.DRM) > 0 && string(pl.DRM) != "null" {
		return provider.Stream{}, fmt.Errorf("%w: DRM-protected", provider.ErrUnavailable)
	}
	hls := pl.Sources["HLS"]
	if len(hls) == 0 || hls[0].Src == "" {
		return provider.Stream{}, fmt.Errorf("%w: no HLS source", provider.ErrUnavailable)
	}
	return provider.Stream{URL: hls[0].Src, Header: http.Header{"User-Agent": {userAgent}}}, nil
}

type apiError struct {
	status int
	code   string
}

func (e *apiError) Error() string { return "tvp: " + e.reason() }

// permanent reports whether retrying cannot help: client errors except a
// timeout or rate limit.
func (e *apiError) permanent() bool {
	return e.status >= 400 && e.status < 500 &&
		e.status != http.StatusRequestTimeout && e.status != http.StatusTooManyRequests
}

func (e *apiError) reason() string {
	if e.code != "" {
		return fmt.Sprintf("%s (HTTP %d)", e.code, e.status)
	}
	return fmt.Sprintf("HTTP %d", e.status)
}

func (p *Provider) get(ctx context.Context, path string, params url.Values, out any) error {
	if params == nil {
		params = url.Values{}
	}
	params.Set("lang", "pl")
	params.Set("platform", "BROWSER")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/"+path+"?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("tvp: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("tvp: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Code string `json:"code"`
		}
		json.Unmarshal(body, &e)
		return &apiError{status: resp.StatusCode, code: e.Code}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("tvp: decoding %s: %w", path, err)
	}
	return nil
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
