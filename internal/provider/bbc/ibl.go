package bbc

import (
	"context"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// iPlayer's catalogue API (IBL) lists programmes, their episodes and versions.

// Pages hold at most 200 episodes; EastEnders has about 1,000.
const (
	pageSize = 200
	maxPages = 10
)

// programme is a search or listing entry: a container ("programme") such as a
// brand, or an episode, which includes one-offs like films.
type programme struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Title    string `json:"title"`    // the series' for episodes
	Subtitle string `json:"subtitle"` // e.g. "Series 4: 12. The Final"
	// The episode's own title, e.g. "The Final" or "Episode 3".
	OriginalTitle  string `json:"original_title"`
	TLEOType       string `json:"tleo_type"` // "episode" for one-offs
	ParentPosition int    `json:"parent_position"`
	// Midnight UTC of the UK air date, or of a film's release year.
	ReleaseTime string      `json:"release_date_time"`
	Versions    []version   `json:"versions"`
	Children    []programme `json:"initial_children"` // listings show a container's first
}

func (p programme) container() bool { return p.Type == "programme" || p.Type == "programme_large" }

// A one-off is its own top-level programme.
func (p programme) oneOff() bool { return !p.container() && p.TLEOType == "episode" }

type version struct {
	ID       string `json:"id"` // for the media selector
	Kind     string `json:"kind"`
	Duration struct {
		Value string `json:"value"` // ISO 8601, e.g. "PT1H6M40.2S"
	} `json:"duration"`
	Availability struct {
		Start string `json:"start"`
	} `json:"availability"`
	FirstBroadcast string `json:"first_broadcast_date_time"` // midnight if the time is unknown
}

// Prefer the original; audio-described and signed versions alter the picture or sound.
func mainVersion(vs []version) (version, bool) {
	var found []version
	for _, v := range vs {
		if !strings.Contains(v.Kind, "audio-described") && !strings.Contains(v.Kind, "signed") &&
			!strings.Contains(v.Kind, "subtitle") && v.ID != "" {
			found = append(found, v)
		}
	}
	for _, v := range found {
		if v.Kind == "original" {
			return v, true
		}
	}
	if len(found) == 0 {
		return version{}, false
	}
	return found[0], true
}

func (p programme) duration() time.Duration {
	v, _ := mainVersion(p.Versions)
	return parseDuration(v.Duration.Value)
}

// When iPlayer made the main version available.
func (p programme) available() time.Time {
	v, _ := mainVersion(p.Versions)
	return parseTime(v.Availability.Start)
}

// Return the year a film was made; 0 if unknown.
func (p programme) year() int {
	if t := parseTime(p.ReleaseTime); !t.IsZero() {
		return t.Year()
	}
	return 0
}

// Return the UK date of the first broadcast, e.g. "2025-05-31", or "".
func (p programme) aired() string {
	if len(p.ReleaseTime) >= 10 && !p.oneOff() {
		return p.ReleaseTime[:10]
	}
	return ""
}

var isoDuration = regexp.MustCompile(`^PT(?:(\d+)H)?(?:(\d+)M)?(?:(\d+(?:\.\d+)?)S)?$`)

func parseDuration(s string) time.Duration {
	m := isoDuration.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	h, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	sec, _ := strconv.ParseFloat(m[3], 64)
	return time.Duration(h)*time.Hour + time.Duration(min)*time.Minute + time.Duration(sec*float64(time.Second))
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// label is BBC's numbering, read from subtitles such as "Series 4: 12. The Final",
// "Season 2: Episode 12", "Specials: 1. The Star Beast" or "29/09/2026".
type label struct {
	series, episode int    // 0 if not numbered
	title           string // "" if unnamed
}

var (
	seriesPrefix = regexp.MustCompile(`(?i)^(?:.*\s)?(?:series|season)\s+(\d+)\s*:\s*(.*)$`)
	numbered     = regexp.MustCompile(`^(\d+)\.\s+(.*)$`)
	episodeOnly  = regexp.MustCompile(`(?i)^episode\s+(\d+)$`)
)

func (p programme) label() label {
	var l label
	rest := strings.TrimSpace(p.Subtitle)
	if m := seriesPrefix.FindStringSubmatch(rest); m != nil {
		l.series, _ = strconv.Atoi(m[1])
		rest = m[2]
	} else if _, after, ok := strings.Cut(rest, ": "); ok {
		rest = after // e.g. "Specials: " or "Christmas Special: "
	}
	switch m1, m2 := numbered.FindStringSubmatch(rest), episodeOnly.FindStringSubmatch(rest); {
	case m1 != nil:
		l.episode, _ = strconv.Atoi(m1[1])
		rest = m1[2]
	case m2 != nil:
		l.episode, _ = strconv.Atoi(m2[1])
		rest = ""
	case l.series > 0:
		l.episode = p.ParentPosition
	}
	l.title = strings.TrimSpace(p.OriginalTitle)
	if l.title == "" {
		l.title = strings.TrimSpace(rest)
	}
	if l.series == 0 {
		l.episode = 0 // positions among specials are not TVDB numbers
	}
	return l
}

// The returned slice is shared; do not modify it.
func (p *Provider) search(ctx context.Context, query string) ([]programme, error) {
	params := url.Values{"q": {query}, "rights": {"web"}}
	u := p.iblURL + "/new-search?" + params.Encode()
	return cached(p.cache, u, func() ([]programme, error) {
		var res struct {
			NewSearch struct {
				Results []programme `json:"results"`
			} `json:"new_search"`
		}
		err := p.get(ctx, u, &res)
		return res.NewSearch.Results, err
	})
}

// Return a container's available episodes. The returned slice is shared.
func (p *Provider) episodes(ctx context.Context, containerID string) ([]programme, error) {
	return cached(p.cache, "episodes "+containerID, func() ([]programme, error) {
		var all []programme
		for page := 1; page <= maxPages; page++ {
			params := url.Values{
				"per_page": {strconv.Itoa(pageSize)}, "page": {strconv.Itoa(page)},
				"rights": {"web"}, "availability": {"available"},
			}
			var res struct {
				Episodes struct {
					Count    int         `json:"count"`
					Elements []programme `json:"elements"`
				} `json:"programme_episodes"`
			}
			if err := p.get(ctx, p.iblURL+"/programmes/"+url.PathEscape(containerID)+"/episodes?"+params.Encode(), &res); err != nil {
				return nil, err
			}
			for _, e := range res.Episodes.Elements {
				if !e.container() {
					all = append(all, e)
				}
			}
			if len(res.Episodes.Elements) < pageSize || page*pageSize >= res.Episodes.Count {
				break
			}
		}
		return all, nil
	})
}

// Not cached: called at download time for current versions.
func (p *Provider) episode(ctx context.Context, id string) (programme, bool, error) {
	params := url.Values{"rights": {"web"}, "availability": {"available"}}
	var res struct {
		Episodes []programme `json:"episodes"`
	}
	if err := p.get(ctx, p.iblURL+"/episodes/"+url.PathEscape(id)+"?"+params.Encode(), &res); err != nil {
		return programme{}, false, err
	}
	if len(res.Episodes) == 0 {
		return programme{}, false, nil
	}
	return res.Episodes[0], true, nil
}

// Return the films iPlayer made available most recently, newest first.
func (p *Provider) newestFilms(ctx context.Context) ([]programme, error) {
	params := url.Values{"sort": {"recent"}, "per_page": {strconv.Itoa(pageSize)}, "rights": {"web"}}
	var res struct {
		Films struct {
			Elements []programme `json:"elements"`
		} `json:"category_programmes"`
	}
	if err := p.get(ctx, p.iblURL+"/categories/films/programmes?"+params.Encode(), &res); err != nil {
		return nil, err
	}
	var films []programme
	for _, e := range res.Films.Elements {
		if e.TLEOType == "episode" && len(e.Children) > 0 {
			films = append(films, e.Children[0])
		}
	}
	return films, nil
}
