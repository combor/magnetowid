// Package provider defines the contract between vodarr and the VOD sites it
// can search and download from. Each site lives in its own subpackage and is
// registered in main.
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

// Kind says whether a query or item is a movie or a TV episode.
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
	Title   string // q as sent by Sonarr/Radarr (trailing year stripped for movies)
	Year    int    // 0 = unknown
	Season  int    // episodes; 0 = unspecified
	Episode int    // 0 = whole season
}

// Item is a search hit that can be offered as a release.
type Item struct {
	ID                    string // provider-stable; must be enough for Resolve
	Kind                  Kind
	Title                 string        // provider's own title (logging only)
	Year, Season, Episode int           // in *arr numbering (the provider maps its own)
	Duration              time.Duration // size estimate + progress
	Published             time.Time
}

// Stream is what the download engine fetches.
type Stream struct {
	URL    string      // anything ffmpeg can open: HLS master/media, DASH MPD, file URL
	Header http.Header // optional extra request headers
}

// Provider is one VOD site.
type Provider interface {
	Name() string                                           // "tvp": URL path + release group
	Search(ctx context.Context, q Query) ([]Item, error)    // empty slice = no match
	Resolve(ctx context.Context, id string) (Stream, error) // called at download time
}

// ErrUnavailable means the content exists but cannot be downloaded (DRM,
// geo-blocked, paid, ...). Providers wrap it with the reason; the downloader
// does not retry it.
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

// NormalizeTitle reduces a title to a form that compares equal across the
// site's catalogue and the *arr query, raw or cleaned: lowercase, no
// diacritics, "&" as "and", no apostrophes or periods, no leading "the", and
// every other run of non-alphanumerics collapsed to one space.
func NormalizeTitle(s string) string {
	s = foldLetters.Replace(s)
	// A chained transformer holds state, so it can't be shared between the
	// concurrent requests that call this.
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
