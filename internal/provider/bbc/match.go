package bbc

import (
	"cmp"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/combor/magnetowid/internal/provider"
)

// pair is a TVDB episode and the BBC episode matched to it.
type pair struct {
	tvdb tvdbEpisode
	bbc  programme
}

const (
	// A remake's original aired years before it. iPlayer dates box sets by
	// their release, weeks or months before TVDB's weekly air dates.
	remakeGap = 365 * 24 * time.Hour
	// Acquired series reach iPlayer within a few years; a same-titled series
	// from long after, such as a remake, is another series.
	acquiredGap = 5 * 365 * 24 * time.Hour
)

// match pairs TVDB episodes with BBC's, one to one. The user's override
// decides the episodes it covers, even if it finds no BBC episode for them,
// and passes of decreasing certainty pair the rest:
//
//  1. titles unique on both sides, e.g. "The Reality War" or "29/09/2026";
//  2. UK air dates, in order when several episodes share a day and each side
//     has as many that day;
//  3. air dates a day apart, when only one episode on each side qualifies;
//  4. BBC's series and episode numbers, e.g. "Series 4: Episode 3".
//
// Passes 3 and 4 reject pairs with different titles. Passes 1 and 4 reject BBC
// episodes that aired over a year before TVDB's, such as a remake's original,
// and pass 4 those that aired over five years after, such as a remake.
func match(tv []tvdbEpisode, eps []programme, ov provider.SeriesOverride) []pair {
	ts := make([]side, len(tv))
	for i, e := range tv {
		title, n := titleKey(e.title)
		ts[i] = side{title: title, titleNumber: n, aired: day(e.aired), season: e.season, episode: e.episode, index: i, match: -1}
	}
	bs := make([]side, len(eps))
	for i, e := range eps {
		l := e.label()
		v, _ := mainVersion(e.Versions)
		title, n := titleKey(l.title)
		bs[i] = side{title: title, titleNumber: n, aired: day(e.aired()), season: l.series, episode: l.episode,
			position: e.ParentPosition, broadcast: parseTime(v.FirstBroadcast), index: i}
	}
	m := matcher{tv: ts, bbc: bs}
	m.byOverride(tv, eps, ov)
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
	titleNumber     int       // N of a generic title such as "Episode N", else 0
	aired           time.Time // zero if unknown
	season, episode int       // BBC's series and position; 0 if none
	// BBC's, to order episodes on the same day: iPlayer lists EastEnders'
	// "Part 2" first, and some broadcasts have no time.
	position  int
	broadcast time.Time
	index     int
	matched   bool
	match     int // for TVDB episodes, the BBC episode's index
}

type matcher struct {
	tv, bbc []side
}

func (m *matcher) link(i, j int) {
	m.tv[i].matched, m.tv[i].match = true, j
	m.bbc[j].matched = true
}

// Pinned BBC episodes are reserved for their pins. Season rules find BBC
// episodes by BBC's numbering, which must name one of the unpinned, whatever
// other rules have matched.
func (m *matcher) byOverride(tv []tvdbEpisode, eps []programme, ov provider.SeriesOverride) {
	pinned := make([]bool, len(eps))
	for j, e := range eps {
		if ov.Pinned(e.ID) {
			pinned[j] = true
			m.bbc[j].matched = true
		}
	}
	for i, e := range tv {
		t, ok := ov.Target(e.season, e.episode)
		if !ok {
			continue
		}
		m.tv[i].matched = true
		var j int
		if t.ID != "" {
			j = slices.IndexFunc(eps, func(b programme) bool { return b.ID == t.ID })
			ok = j >= 0
		} else {
			j, ok = only(m.bbc, func(b side) bool {
				return !pinned[b.index] && t.Episode > 0 && b.episode == t.Episode && (t.Season == 0 || b.season == t.Season)
			})
			ok = ok && !m.bbc[j].matched // another rule's
		}
		if ok {
			m.link(i, j)
		}
	}
}

func (m *matcher) byTitle() {
	tvTitles, bbcTitles := titles(m.tv), titles(m.bbc)
	for t, is := range tvTitles {
		js := bbcTitles[t]
		if len(is) == 1 && len(js) == 1 && !m.tv[is[0]].matched && !m.bbc[js[0]].matched && !predates(m.bbc[js[0]], m.tv[is[0]]) {
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

// Whole days must agree: a box set shares one date, so with an episode
// missing, the day's remaining counts could agree by chance.
func (m *matcher) bySameDay() {
	tvDays, bbcDays := days(m.tv), days(m.bbc)
	tvTotal, bbcTotal := dayTotals(m.tv), dayTotals(m.bbc)
	for d, is := range tvDays {
		js := bbcDays[d]
		if len(is) != len(js) || tvTotal[d] != bbcTotal[d] {
			continue
		}
		slices.SortFunc(is, func(a, b int) int {
			return cmp.Or(cmp.Compare(m.tv[a].season, m.tv[b].season), cmp.Compare(m.tv[a].episode, m.tv[b].episode))
		})
		// Positions order the day only if every episode has one.
		positioned := !slices.ContainsFunc(js, func(j int) bool { return m.bbc[j].position == 0 })
		slices.SortFunc(js, func(a, b int) int {
			x, y := m.bbc[a], m.bbc[b]
			byPosition := 0
			if positioned {
				byPosition = cmp.Compare(x.position, y.position)
			}
			return cmp.Or(cmp.Compare(x.season, y.season), cmp.Compare(x.episode, y.episode),
				byPosition, x.broadcast.Compare(y.broadcast), cmp.Compare(x.index, y.index))
		})
		// A box set's release date can match TVDB's first episode alone.
		agree := true
		for k := range is {
			agree = agree && !renumbered(m.tv[is[k]], m.bbc[js[k]])
		}
		if !agree {
			continue
		}
		for k := range is {
			m.link(is[k], js[k])
		}
	}
}

func dayTotals(ss []side) map[time.Time]int {
	totals := map[time.Time]int{}
	for _, s := range ss {
		if !s.aired.IsZero() {
			totals[s.aired]++
		}
	}
	return totals
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
		// Same-day pairs are pass 2's to accept or refuse.
		if back, ok := only(m.tv, func(t side) bool { return near(t, m.bbc[j]) }); ok && back == i && !t.aired.Equal(m.bbc[j].aired) {
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
		if b.matched || contradicts(t, b) || predates(b, t) || postdates(b, t) {
			continue
		}
		m.link(i, js[0])
	}
}

// Report whether BBC's episode b aired too long before TVDB's t to be it.
func predates(b, t side) bool {
	return !t.aired.IsZero() && !b.aired.IsZero() && b.aired.Before(t.aired.Add(-remakeGap))
}

// Report whether BBC's episode b aired too long after TVDB's t to be it.
func postdates(b, t side) bool {
	return !t.aired.IsZero() && !b.aired.IsZero() && b.aired.After(t.aired.Add(acquiredGap))
}

func contradicts(a, b side) bool {
	return (a.title != "" && b.title != "" && a.title != b.title) || renumbered(a, b)
}

// Generic titles still disagree when their numbers do: "Episode 1" is not
// "Episode 2", though TVDB's S07E03 can be "Episode 5" when it lacks episodes.
func renumbered(a, b side) bool {
	return a.titleNumber > 0 && b.titleNumber > 0 && a.titleNumber != b.titleNumber
}

var (
	// TVDB numbers parts, e.g. "Wish World (1)"; BBC doesn't.
	partNumber = regexp.MustCompile(`\s*\(\d+\)$`)
	// Titles such as "Episode 3" name no particular episode.
	genericTitle = regexp.MustCompile(`^(?:(?:episode|part|chapter|show|programme|week|day)\s*)?\d+$|^(?:tba|tbc|tbd)$`)
)

// Return the title for comparison, or for generic titles, "" and their number.
func titleKey(title string) (string, int) {
	t := provider.NormalizeTitle(partNumber.ReplaceAllString(title, ""))
	if !genericTitle.MatchString(t) {
		return t, 0
	}
	n, _ := strconv.Atoi(strings.TrimLeft(t, "abcdefghijklmnopqrstuvwxyz "))
	return "", n
}

func day(date string) time.Time {
	t, _ := time.Parse(time.DateOnly, date)
	return t
}
