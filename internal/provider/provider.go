// Package provider defines the interface each VOD site implements.
package provider

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Kind is a movie or a TV episode.
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

// Query is a title search as sent by Sonarr or Radarr.
type Query struct {
	Kind    Kind
	Title   string // the *arr's q, without a trailing year for movies
	Year    int    // 0 = unknown
	Season  int
	Episode int // 0 = whole season
}

// Item is a search hit that can be offered as a release.
type Item struct {
	ID                    string // passed to Resolve
	Kind                  Kind
	Title                 string // provider's title, for logs
	Year, Season, Episode int    // *arr numbering
	Duration              time.Duration
	Published             time.Time
}

// Stream is what the download engine fetches.
type Stream struct {
	URL       string // anything ffmpeg can open
	Header    http.Header
	Subtitles []Subtitle // saved next to the video
}

// Subtitle is a stream's subtitles in one language.
type Subtitle struct {
	URL      string // fetched with the stream's Header
	Format   string // TTML, the only one read so far
	Language string // ISO 639 code, e.g. "pol"; "" if unknown
	SDH      bool   // for the deaf and hard of hearing
}

// TTML is W3C Timed Text, a Subtitle Format.
const TTML = "ttml"

// Provider is one VOD site.
type Provider interface {
	Name() string // URL path and release group, e.g. "tvp"
	Search(ctx context.Context, q Query) ([]Item, error)
	Resolve(ctx context.Context, id string) (Stream, error) // called at download time
}

// TVDBSearcher is a Provider that can find a series by its TVDB ID alone.
// Sonarr searches that way first, and by title only when that finds nothing.
type TVDBSearcher interface {
	// SearchTVDB is Search for the series with the TVDB ID; q has no Title.
	// It also returns Sonarr's title for the series, which names the
	// releases, as Sonarr won't import a release it matched only by ID.
	SearchTVDB(ctx context.Context, tvdbID int, q Query) (title string, items []Item, err error)
}

// Release is an Item with the *arr's title, which names the release.
type Release struct {
	Title string
	Item
}

// RecentLister is a Provider that offers new releases to RSS sync, which is
// how Sonarr and Radarr find new episodes and films without searching.
type RecentLister interface {
	// Recent returns new releases of the kind. It must return quickly: RSS
	// sync has a timeout, and errors count against the indexer.
	Recent(ctx context.Context, kind Kind) ([]Release, error)
}

// ErrUnavailable marks content that can't be downloaded (DRM, geo-blocked,
// paid). It is not retried.
var ErrUnavailable = errors.New("content unavailable")

// Registry maps provider names to providers.
type Registry struct {
	byName map[string]Provider
}

// NewRegistry builds a registry. Provider names must be unique.
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

// Get returns the provider with the given name.
func (r *Registry) Get(name string) (Provider, bool) {
	p, ok := r.byName[name]
	return p, ok
}

// Names returns the registered provider names, sorted.
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

// NormalizeTitle makes titles comparable the way Sonarr/Radarr clean them:
// lowercase, no diacritics or punctuation, "&" as "and", no leading "the".
func NormalizeTitle(s string) string {
	s = foldLetters.Replace(s)
	// Not safe for concurrent use, so built per call.
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
