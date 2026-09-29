package bbc

import (
	"cmp"
	"regexp"
	"slices"
	"time"

	"github.com/combor/magnetowid/internal/provider"
)

// pair is a TVDB episode and the BBC episode matched to it.
type pair struct {
	tvdb tvdbEpisode
	bbc  programme
}

// Remakes' originals predate them; acquired series air in the UK later.
const remakeGap = 7 * 24 * time.Hour

// match pairs TVDB episodes with BBC's, one to one, in passes of decreasing
// certainty:
//
//  1. titles unique on both sides, e.g. "The Reality War" or "29/09/2026";
//  2. UK air dates, in order when several episodes share a day;
//  3. air dates a day apart, when only one episode on each side qualifies;
//  4. BBC's series and episode numbers, e.g. "Series 4: Episode 3".
//
// Passes 3 and 4 reject pairs with different titles. Pass 4 rejects BBC
// episodes that aired over a week before TVDB's, such as a remake's original.
func match(tv []tvdbEpisode, eps []programme) []pair {
	ts := make([]side, len(tv))
	for i, e := range tv {
		ts[i] = side{title: comparable(e.title), aired: day(e.aired), season: e.season, episode: e.episode, index: i}
	}
	bs := make([]side, len(eps))
	for i, e := range eps {
		l := e.label()
		v, _ := mainVersion(e.Versions)
		bs[i] = side{title: comparable(l.title), aired: day(e.aired()), season: l.series, episode: l.episode,
			broadcast: parseTime(v.FirstBroadcast), index: i}
	}
	m := matcher{tv: ts, bbc: bs}
	m.byTitle()
	m.bySameDay()
	m.byNearDay()
	m.byNumber()

	var pairs []pair
	for i, t := range m.tv {
		if t.match >= 0 {
			pairs = append(pairs, pair{tvdb: tv[i], bbc: eps[t.match]})
		}
	}
	return pairs
}

type side struct {
	title           string    // comparable, "" if generic
	aired           time.Time // zero if unknown
	season, episode int       // BBC's series and position; 0 if none
	broadcast       time.Time // BBC's, to order episodes on the same day
	index           int
	matched         bool
	match           int // for TVDB episodes, the BBC episode's index
}

type matcher struct {
	tv, bbc []side
}

func (m *matcher) link(i, j int) {
	m.tv[i].matched, m.tv[i].match = true, j
	m.bbc[j].matched = true
}

func (m *matcher) byTitle() {
	for i := range m.tv {
		m.tv[i].match = -1
	}
	tvTitles, bbcTitles := titles(m.tv), titles(m.bbc)
	for t, is := range tvTitles {
		if js := bbcTitles[t]; len(is) == 1 && len(js) == 1 {
			m.link(is[0], js[0])
		}
	}
}

func titles(ss []side) map[string][]int {
	byTitle := map[string][]int{}
	for i, s := range ss {
		if s.title != "" {
			byTitle[s.title] = append(byTitle[s.title], i)
		}
	}
	return byTitle
}

func (m *matcher) bySameDay() {
	tvDays, bbcDays := days(m.tv), days(m.bbc)
	for d, is := range tvDays {
		js := bbcDays[d]
		if len(is) != len(js) {
			continue
		}
		slices.SortFunc(is, func(a, b int) int {
			return cmp.Or(cmp.Compare(m.tv[a].season, m.tv[b].season), cmp.Compare(m.tv[a].episode, m.tv[b].episode))
		})
		slices.SortFunc(js, func(a, b int) int {
			x, y := m.bbc[a], m.bbc[b]
			return cmp.Or(cmp.Compare(x.season, y.season), cmp.Compare(x.episode, y.episode),
				x.broadcast.Compare(y.broadcast), cmp.Compare(x.index, y.index))
		})
		for k := range is {
			m.link(is[k], js[k])
		}
	}
}

// Group unmatched episodes by air date.
func days(ss []side) map[time.Time][]int {
	byDay := map[time.Time][]int{}
	for i, s := range ss {
		if !s.matched && !s.aired.IsZero() {
			byDay[s.aired] = append(byDay[s.aired], i)
		}
	}
	return byDay
}

func (m *matcher) byNearDay() {
	near := func(a, b side) bool {
		d := a.aired.Sub(b.aired)
		return !a.matched && !b.matched && !a.aired.IsZero() && !b.aired.IsZero() &&
			d <= 24*time.Hour && d >= -24*time.Hour && !contradicts(a, b)
	}
	var links [][2]int
	for i, t := range m.tv {
		j, ok := only(m.bbc, func(b side) bool { return near(t, b) })
		if !ok {
			continue
		}
		if back, ok := only(m.tv, func(t side) bool { return near(t, m.bbc[j]) }); ok && back == i {
			links = append(links, [2]int{i, j})
		}
	}
	for _, l := range links {
		m.link(l[0], l[1])
	}
}

// Return the index of the only element satisfying f.
func only(ss []side, f func(side) bool) (int, bool) {
	found := -1
	for i, s := range ss {
		if f(s) {
			if found >= 0 {
				return 0, false
			}
			found = i
		}
	}
	return found, found >= 0
}

func (m *matcher) byNumber() {
	type number struct{ season, episode int }
	byNumber := map[number][]int{}
	for j, b := range m.bbc {
		if !b.matched && b.season > 0 && b.episode > 0 {
			n := number{b.season, b.episode}
			byNumber[n] = append(byNumber[n], j)
		}
	}
	for i, t := range m.tv {
		js := byNumber[number{t.season, t.episode}]
		if t.matched || len(js) != 1 {
			continue
		}
		b := m.bbc[js[0]]
		if b.matched || contradicts(t, b) || (!t.aired.IsZero() && !b.aired.IsZero() && b.aired.Before(t.aired.Add(-remakeGap))) {
			continue
		}
		m.link(i, js[0])
	}
}

func contradicts(a, b side) bool {
	return a.title != "" && b.title != "" && a.title != b.title
}

var (
	// TVDB numbers parts, e.g. "Wish World (1)"; BBC doesn't.
	partNumber = regexp.MustCompile(`\s*\(\d+\)$`)
	// Titles such as "Episode 3" name no particular episode.
	genericTitle = regexp.MustCompile(`^(?:(?:episode|part|chapter|show|programme|week|day)\s*)?\d+$|^(?:tba|tbc|tbd)$`)
)

func comparable(title string) string {
	t := provider.NormalizeTitle(partNumber.ReplaceAllString(title, ""))
	if genericTitle.MatchString(t) {
		return ""
	}
	return t
}

func day(date string) time.Time {
	t, _ := time.Parse(time.DateOnly, date)
	return t
}
