// Package provider defines the interface each VOD site implements.
package provider

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

type Kind int

const (
	Movie Kind = iota + 1
	Episode
)

func (k Kind) String() string {
	switch k {
	case Movie:
		return "movie"
	case Episode:
		return "episode"
	}
	return "unknown"
}

// Query uses Sonarr/Radarr titles and numbering.
type Query struct {
	Kind    Kind
	Title   string // the *arr's q, without a trailing year for movies
	Year    int    // 0 = unknown
	Season  int    // 0 holds specials
	Episode int    // 0 = whole season
	// A daily series' local air date, e.g. "2026-09-29", instead of Season
	// and Episode.
	AirDate string
}

// Wants reports whether q asks for a TVDB episode aired on airDate, a local
// date such as "2026-09-29" or "" if unknown.
func (q Query) Wants(season, episode int, airDate string) bool {
	if q.AirDate != "" {
		return airDate == q.AirDate
	}
	return season == q.Season && (q.Episode == 0 || episode == q.Episode)
}

type Item struct {
	ID                    string // passed to Resolve
	Kind                  Kind
	Title                 string // provider's title, for logs
	Year, Season, Episode int    // *arr numbering
	Duration              time.Duration
	Published             time.Time
}

type Stream struct {
	URL    string // anything ffmpeg can open
	Header http.Header
	// The HLS master playlist at URL, if the provider has amended it, e.g. to
	// add variants the site omits. Its relative URIs resolve against URL.
	Playlist  string
	Subtitles []Subtitle // saved next to the video
}

type Subtitle struct {
	URL      string // fetched with the stream's Header
	Format   string // TTML, the only one read so far
	Language string // ISO 639 code, e.g. "pol"; "" if unknown
	SDH      bool   // for the deaf and hard of hearing
}

const TTML = "ttml"

type Provider interface {
	Name() string // URL path and release group, e.g. "tvp"
	Search(ctx context.Context, q Query) ([]Item, error)
	Resolve(ctx context.Context, id string) (Stream, error) // called at download time
}

// TVDBSearcher enables Sonarr's ID search, which precedes its title search.
type TVDBSearcher interface {
	// q has no title. Return Sonarr's title for import matching; ID-only matches cannot be imported.
	SearchTVDB(ctx context.Context, tvdbID int, q Query) (title string, items []Item, err error)
}

// Release pairs a provider item with the client's title for import matching.
type Release struct {
	Title string
	Item
}

// Key identifies the release as clients see it: the site's item under the
// client's title and numbering, which overrides can change.
func (r Release) Key() string {
	if r.Kind == Movie {
		return fmt.Sprintf("%s %d %s", r.ID, r.Year, NormalizeTitle(r.Title))
	}
	return fmt.Sprintf("%s S%02dE%02d %s", r.ID, r.Season, r.Episode, NormalizeTitle(r.Title))
}

// RecentLister supplies releases for Sonarr/Radarr RSS sync.
type RecentLister interface {
	// Return promptly: timeouts and errors count against the indexer.
	Recent(ctx context.Context, kind Kind) ([]Release, error)
}

// ErrUnavailable marks DRM, paid, or region-blocked content. It is not retried.
var ErrUnavailable = errors.New("content unavailable")

// Overridable sites accept the user's corrections to automatic matching.
// An override replaces automatic matching, and its checks, wherever it applies.
type Overridable interface {
	// SetOverrides is called at startup and after every change, possibly while
	// the site serves requests.
	SetOverrides(*Overrides)
	// ParseID returns the site ID in ref, an ID or a URL of the site's page.
	ParseID(ref string) (string, error)
}

// Overrides are one site's corrections, keyed as Sonarr and Radarr ask.
// They are never modified once made.
type Overrides struct {
	Series map[int]SeriesOverride  // by TVDB ID
	Films  map[string]FilmOverride // by FilmKey
}

// SeriesFor accepts a nil o.
func (o *Overrides) SeriesFor(tvdbID int) (SeriesOverride, bool) {
	if o == nil {
		return SeriesOverride{}, false
	}
	s, ok := o.Series[tvdbID]
	return s, ok
}

// Film accepts a nil o.
func (o *Overrides) Film(title string, year int) (FilmOverride, bool) {
	if o == nil {
		return FilmOverride{}, false
	}
	f, ok := o.Films[FilmKey(title, year)]
	return f, ok
}

// FilmPinned reports whether a film override names the site ID, which no
// other film then matches. It accepts a nil o.
func (o *Overrides) FilmPinned(id string) bool {
	if o == nil {
		return false
	}
	for _, f := range o.Films {
		if f.ID == id {
			return true
		}
	}
	return false
}

// Changes returns the TVDB IDs whose overrides differ from before, and
// whether any film's does. Either may be nil.
func (o *Overrides) Changes(before *Overrides) (series []int, films bool) {
	var now, then Overrides
	if o != nil {
		now = *o
	}
	if before != nil {
		then = *before
	}
	for id, s := range now.Series {
		if b, ok := then.Series[id]; !ok || !reflect.DeepEqual(s, b) {
			series = append(series, id)
		}
	}
	for id := range then.Series {
		if _, ok := now.Series[id]; !ok {
			series = append(series, id)
		}
	}
	return series, !maps.EqualFunc(now.Films, then.Films, func(a, b FilmOverride) bool { return reflect.DeepEqual(a, b) })
}

type SeriesOverride struct {
	Titles []string `json:"titles,omitempty"` // searched instead of automatic ones
	// The site's series, chosen from the search results for the titles.
	ID       string                   `json:"id,omitempty"`
	Seasons  []SeasonRule             `json:"seasons,omitempty"`
	Episodes map[EpisodeNumber]string `json:"episodes,omitempty"` // site IDs, which win over season rules
}

// SeasonRule places a TVDB season's episodes in the site's numbering.
type SeasonRule struct {
	Season int `json:"season"` // TVDB's
	// 0 finds the site's episode number in any season, if only one has it.
	SiteSeason int `json:"site_season"`
	Offset     int `json:"offset"` // site episode = TVDB episode + Offset
}

// EpisodeNumber is a TVDB episode, written as "S01E05".
type EpisodeNumber struct{ Season, Episode int }

var episodeNumber = regexp.MustCompile(`^[Ss](\d{1,4})[Ee](\d{1,5})$`)

func (n EpisodeNumber) String() string { return fmt.Sprintf("S%02dE%02d", n.Season, n.Episode) }

func (n EpisodeNumber) MarshalText() ([]byte, error) { return []byte(n.String()), nil }

func (n *EpisodeNumber) UnmarshalText(text []byte) error {
	m := episodeNumber.FindSubmatch(text)
	if m == nil {
		return fmt.Errorf("episode %q is not like S01E05", text)
	}
	n.Season, _ = strconv.Atoi(string(m[1]))
	n.Episode, _ = strconv.Atoi(string(m[2]))
	return nil
}

// Rule returns the season's rule, which covers all its episodes.
func (o SeriesOverride) Rule(season int) (SeasonRule, bool) {
	for _, r := range o.Seasons {
		if r.Season == season {
			return r, true
		}
	}
	return SeasonRule{}, false
}

// Target is where a TVDB episode is on the site: an ID, or a season and
// episode number there. Season 0 is any season; Episode can be below 1,
// where no episode is.
type Target struct {
	ID              string
	Season, Episode int
}

// Target returns where the override puts a TVDB episode. ok=false leaves it
// to automatic matching.
func (o SeriesOverride) Target(season, episode int) (t Target, ok bool) {
	if id, ok := o.Episodes[EpisodeNumber{season, episode}]; ok {
		return Target{ID: id}, true
	}
	if r, ok := o.Rule(season); ok {
		return Target{Season: r.SiteSeason, Episode: episode + r.Offset}, true
	}
	return Target{}, false
}

// Pinned reports whether an episode pin names the site ID.
func (o SeriesOverride) Pinned(id string) bool {
	for _, v := range o.Episodes {
		if v == id {
			return true
		}
	}
	return false
}

type FilmOverride struct {
	// Radarr's title and year.
	Title  string   `json:"title"`
	Year   int      `json:"year"`
	Titles []string `json:"titles,omitempty"` // searched instead of Radarr's
	// The site's film, found by searching the titles. Its title and year may differ.
	ID string `json:"id,omitempty"`
}

// FilmKey matches Radarr's title-and-year identity.
func FilmKey(title string, year int) string {
	return strconv.Itoa(year) + " " + NormalizeTitle(title)
}

type Registry struct {
	byName map[string]Provider
}

// Provider names must be unique.
func NewRegistry(ps ...Provider) *Registry {
	r := &Registry{byName: make(map[string]Provider, len(ps))}
	for _, p := range ps {
		if _, dup := r.byName[p.Name()]; dup {
			panic("provider: duplicate provider name " + p.Name())
		}
		r.byName[p.Name()] = p
	}
	return r
}

func (r *Registry) Get(name string) (Provider, bool) {
	p, ok := r.byName[name]
	return p, ok
}

// Names returns provider names in sorted order.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.byName))
	for n := range r.byName {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Letters that NFD does not decompose into a base letter plus a mark.
var foldLetters = strings.NewReplacer(
	"ł", "l", "Ł", "L",
	"đ", "d", "Đ", "D",
	"ø", "o", "Ø", "O",
	"&", " and ",
)

// NormalizeTitle matches Sonarr/Radarr title cleaning: lowercase, no diacritics
// or punctuation, "&" becomes "and", and leading "the" is removed.
func NormalizeTitle(s string) string {
	s = foldLetters.Replace(s)
	// Transformers are not safe for concurrent use.
	stripMarks := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	if folded, _, err := transform.String(stripMarks, s); err == nil {
		s = folded
	}
	s = strings.ToLower(s)
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\'', '.', '`', '´', '‘', '’':
			return -1
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return ' '
	}, s)
	fields := strings.Fields(s)
	if len(fields) > 1 && fields[0] == "the" {
		fields = fields[1:]
	}
	return strings.Join(fields, " ")
}
