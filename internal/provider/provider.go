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
	Season  int
	Episode int // 0 = whole season
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

// RecentLister supplies releases for Sonarr/Radarr RSS sync.
type RecentLister interface {
	// Return promptly: timeouts and errors count against the indexer.
	Recent(ctx context.Context, kind Kind) ([]Release, error)
}

// ErrUnavailable marks DRM, paid, or region-blocked content. It is not retried.
var ErrUnavailable = errors.New("content unavailable")

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
