// Package tvp is the provider for TVP VOD (https://vod.tvp.pl).
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

	bolt "go.etcd.io/bbolt"

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
	titles  *titleLookup
	cache   *responseCache
	watched *watchList
	feed    feed
	log     *slog.Logger
}

// New returns a TVP provider using client for API calls. It keeps the
// series it watches for new episodes in db, or only in memory if db is nil.
func New(client *http.Client, log *slog.Logger, db *bolt.DB) (*Provider, error) {
	watched, err := loadWatchList(db)
	if err != nil {
		return nil, err
	}
	p := &Provider{
		client:  client,
		baseURL: defaultBaseURL,
		titles:  newTitleLookup(client),
		cache:   newResponseCache(),
		watched: watched,
		log:     log,
	}
	p.feed.rebuild = p.rebuildFeed
	return p, nil
}

func (p *Provider) Name() string { return "tvp" }

// product is a TVP serial, season, episode or movie.
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

func (p *Provider) searchEpisodes(ctx context.Context, q provider.Query) ([]provider.Item, error) {
	if q.Season <= 0 {
		return nil, nil
	}
	serial, ok, err := p.findSerial(ctx, q.Title)
	if err != nil || !ok {
		return nil, err
	}
	return p.serialEpisodes(ctx, serial, q, nil)
}

// findSerial returns the free TVP serial with the title.
func (p *Provider) findSerial(ctx context.Context, title string) (product, bool, error) {
	serials, err := p.search(ctx, "SERIAL", title)
	if err != nil {
		return product{}, false, err
	}
	serial, ok := p.pick(serials, title)
	return serial, ok, nil
}

// serialEpisodes maps the *arr's season/episode onto TVP's episode numbers,
// which often run across seasons (Ranczo's season 2 is 14-26). tv is the
// series' TVDB data, or nil if the search didn't come with a TVDB ID.
func (p *Provider) serialEpisodes(ctx context.Context, serial product, q provider.Query, tv *series) ([]provider.Item, error) {
	seasons, err := p.seasons(ctx, serial.ID)
	if err != nil {
		return nil, err
	}
	bs := blocks(seasons)
	if len(bs) > 0 && tv != nil {
		items, numbered, err := p.byNumber(ctx, serial, bs, q, tv)
		if err != nil || numbered {
			return items, err
		}
	}
	var episodes []product
	if season, ok := findSeason(seasons, q.Season); ok {
		if episodes, err = p.episodes(ctx, serial.ID, season.ID); err != nil {
			return nil, err
		}
		// A block's position says nothing about TVDB's seasons: M jak
		// miłość's block 20 starts at 1901, but TVDB's S20E1 is 1452. Only
		// season 1 and a block starting at 1 both count from the first
		// episode; byNumber has already used that if it had TVDB's episodes.
		if len(bs) == 0 || (q.Season == 1 && tv == nil) {
			items, err := p.bySeason(ctx, serial, seasons, q, episodes)
			if err != nil || len(items) > 0 || q.Episode == 0 {
				return items, err
			}
		}
	}
	if q.Episode == 0 {
		return nil, nil
	}
	return p.byAbsoluteNumber(ctx, serial, seasons, q, episodes)
}

// bySeason maps episode N of the season to TVP number start+N-1, so gaps
// and duplicates never shift other episodes.
func (p *Provider) bySeason(ctx context.Context, serial product, seasons []product, q provider.Query, episodes []product) ([]provider.Item, error) {
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
			continue
		}
		items = append(items, episodeItem(serial, e, q.Season, ep))
	}
	return items, nil
}

// byAbsoluteNumber handles absolute episode numbers in an arbitrary season
// (TVDB's Klan S15E2113 is TVP's number 2113, in its season 22). It needs the
// number to be above every TVP number in that season, TVP's numbering to run
// on across seasons, and exactly one match; anything unclear yields nothing.
func (p *Provider) byAbsoluteNumber(ctx context.Context, serial product, seasons []product, q provider.Query, seasonEpisodes []product) ([]provider.Item, error) {
	sorted := append([]product(nil), seasons...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Number < sorted[j].Number })

	// The requested season sets the ceiling; if TVP lacks it, the closest below.
	idx, ceil := -1, seasonEpisodes
	for i := len(sorted) - 1; i >= 0; i-- {
		if len(ceil) > 0 && sorted[i].Number == q.Season {
			idx = i
			break
		}
		if len(ceil) == 0 && sorted[i].Number < q.Season {
			eps, err := p.episodes(ctx, serial.ID, sorted[i].ID)
			if err != nil {
				return nil, err
			}
			if len(eps) > 0 {
				idx, ceil = i, eps
				break
			}
		}
	}
	if idx < 0 {
		return nil, nil
	}
	lastMax := ceil[len(ceil)-1].Number
	if q.Episode <= lastMax {
		return nil, nil
	}

	// Absolute numbering: every season starts above the previous one's last
	// number. Anything else (restarts, overlaps) ends the search.
	for i := idx - 1; i >= 0; i-- {
		prev, err := p.episodes(ctx, serial.ID, sorted[i].ID)
		if err != nil {
			return nil, err
		}
		if len(prev) == 0 {
			continue
		}
		if ceil[0].Number <= prev[len(prev)-1].Number {
			return nil, nil
		}
		break
	}
	var match []product
	for _, s := range sorted[idx+1:] {
		eps, err := p.episodes(ctx, serial.ID, s.ID)
		if err != nil {
			return nil, err
		}
		if len(eps) == 0 {
			continue
		}
		if eps[0].Number <= lastMax {
			if hasNumber(eps, q.Episode) {
				return nil, nil
			}
			break
		}
		if eps[0].Number > q.Episode {
			break
		}
		for _, e := range eps {
			if e.Number == q.Episode {
				match = append(match, e)
			}
		}
		lastMax = eps[len(eps)-1].Number
	}
	if len(match) != 1 || match[0].Payable {
		return nil, nil
	}
	p.log.Info("matched TVP episode by absolute number", "serial", serial.Title,
		"season", q.Season, "episode", q.Episode)
	return []provider.Item{episodeItem(serial, match[0], q.Season, q.Episode)}, nil
}

func hasNumber(eps []product, n int) bool {
	for _, e := range eps {
		if e.Number == n {
			return true
		}
	}
	return false
}

func episodeItem(serial, e product, season, episode int) provider.Item {
	return provider.Item{
		ID:        strconv.FormatInt(e.ID, 10),
		Kind:      provider.Episode,
		Title:     serial.Title + " – " + e.Title,
		Year:      e.Year,
		Season:    season,
		Episode:   episode,
		Duration:  time.Duration(e.Duration) * time.Second,
		Published: parseTime(e.Since),
	}
}

// seasonStart returns the TVP number of the season's first episode when it
// is certain: 1, or right after the previous season's last number.
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

// seasons returns a serial's seasons. The slice is shared: don't modify it.
func (p *Provider) seasons(ctx context.Context, serialID int64) ([]product, error) {
	path := fmt.Sprintf("vods/serials/%d/seasons", serialID)
	return cached(p.cache, path, func() ([]product, error) {
		var seasons []product
		err := p.get(ctx, path, nil, &seasons)
		return seasons, err
	})
}

// episodes returns a season's episodes sorted by number, without specials.
// The slice is shared: don't modify it.
func (p *Provider) episodes(ctx context.Context, serialID, seasonID int64) ([]product, error) {
	path := fmt.Sprintf("vods/serials/%d/seasons/%d/episodes", serialID, seasonID)
	return cached(p.cache, path, func() ([]product, error) {
		var all []product
		if err := p.get(ctx, path, nil, &all); err != nil {
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
	})
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

// pick returns the first free serial whose title matches exactly.
func (p *Provider) pick(serials []product, title string) (product, bool) {
	want := provider.NormalizeTitle(title)
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
		p.log.Warn("several TVP serials match; using the first", "title", title, "count", len(matches))
	}
	return matches[0], true
}

func titleMatches(v product, normalized string) bool {
	return provider.NormalizeTitle(v.Title) == normalized ||
		(v.OriginalTitle != "" && provider.NormalizeTitle(v.OriginalTitle) == normalized)
}

// search returns TVP's search results. The slice is shared: don't modify it.
func (p *Provider) search(ctx context.Context, kind, keyword string) ([]product, error) {
	path, params := "vods/search/"+kind, url.Values{"keyword": {keyword}}
	return cached(p.cache, path+"?"+params.Encode(), func() ([]product, error) {
		var res struct {
			Items []product `json:"items"`
		}
		err := p.get(ctx, path, params, &res)
		return res.Items, err
	})
}

// Resolve returns the current stream URL. It embeds the requester's IP and a
// date, so it is only good for an immediate download, and is never cached.
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

// permanent reports whether retrying cannot help.
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
