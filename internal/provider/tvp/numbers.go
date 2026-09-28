package tvp

import (
	"cmp"
	"context"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/combor/magnetowid/internal/provider"
)

// TVP numbers the episodes of long soaps from the series' start and keeps
// them in blocks of 100. TVDB's seasons follow the broadcast years, so its
// season and episode don't give that number. TVDB's absolute numbers, its
// titles that are only the number, and TVP's guide do, but TVDB's are patchy
// and sometimes wrong, so a number is used only if it holds up.

// maxYearsApart is how far TVP's year for an episode may be from TVDB's air
// date. TVP's years are sometimes a year or two off; a wrong number from TVDB
// is usually many years off (M jak miłość's S7, from 2007, has TVDB absolute
// numbers of 2020 episodes).
const maxYearsApart = 2

// block is a season that TVP uses as a block of episode numbers.
type block struct {
	season      product
	first, last int // last is 0 for the latest, open block
}

// blockTitle is the title of a block: "1–100", "801-900" or, for the latest,
// "1901–".
var blockTitle = regexp.MustCompile(`^\s*(\d+)\s*[–-]\s*(\d*)\s*$`)

// blocks returns the seasons TVP uses as blocks of episode numbers, as it
// does for long soaps.
func blocks(seasons []product) []block {
	var bs []block
	for _, s := range seasons {
		m := blockTitle.FindStringSubmatch(s.Title)
		if m == nil {
			continue
		}
		first, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		last, _ := strconv.Atoi(m[2]) // 0 when open
		bs = append(bs, block{season: s, first: first, last: last})
	}
	return bs
}

// numberTitle is a TVDB episode title that is only its number: "1945",
// "Odcinek 1378", "Episode 75", "Odcinek 846 (12.09.2011)".
var numberTitle = regexp.MustCompile(`(?i)^\s*(?:(?:odcinek|episode|odc\.)\s*)?(\d+)\s*(?:\(.*\))?\s*$`)

// titleNumber returns the number a TVDB episode title gives, or 0.
func titleNumber(title string) int {
	m := numberTitle.FindStringSubmatch(title)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// conflicting is the number of an episode whose sources give different
// numbers. No other source overrides it.
const conflicting = -1

// agreed returns the number a and b both give, where 0 is unknown.
func agreed(a, b int) int {
	switch {
	case a == 0:
		return b
	case b == 0 || a == b:
		return a
	}
	return conflicting
}

// episodeKey is a TVDB season and episode.
type episodeKey struct{ season, episode int }

// episodeNumbers returns TVP's numbers for the series' episodes, where they
// can be told. It asks TVP's guide about the episodes it still lists only if
// the season has some, so a search for an older season doesn't depend on it.
func (p *Provider) episodeNumbers(ctx context.Context, s *series, serialTitle string, season int, now time.Time) (map[episodeKey]int, error) {
	inGuide := func(e tvdbEpisode) bool {
		return !e.aired.Before(now.Add(-guideWindow)) && !e.aired.After(now.Add(airDateSlack))
	}
	askGuide := slices.ContainsFunc(s.episodes, func(e tvdbEpisode) bool { return e.season == season && inGuide(e) })
	numbers := make([]int, len(s.episodes))
	for i, e := range s.episodes {
		numbers[i] = e.number
		if e.season == 1 {
			// Season 1 counts from the first episode, as TVP does.
			numbers[i] = agreed(numbers[i], e.episode)
		}
		if !askGuide || !inGuide(e) {
			continue
		}
		n, ok, err := p.broadcastNumber(ctx, serialTitle, e.aired)
		if err != nil {
			return nil, err
		}
		if ok {
			numbers[i] = agreed(numbers[i], n)
		}
	}
	return checkNumbers(s.episodes, numbers), nil
}

// checkNumbers keeps the episodes' numbers that no other episode has and
// that an adjacent episode of the same season agrees with: its number is at
// the same offset from its episode number, as in a run of episodes numbered
// in order. That drops TVDB's stray absolute numbers and typos (Barwy
// szczęścia's "Odcinek 28981"), and a guide number found through a wrong
// air date. numbers[i] is episodes[i]'s, 0 or less if unknown.
func checkNumbers(episodes []tvdbEpisode, numbers []int) map[episodeKey]int {
	claims := make(map[int]int)
	for _, n := range numbers {
		if n > 0 {
			claims[n]++
		}
	}
	order := make([]int, len(episodes))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int {
		return cmp.Or(cmp.Compare(episodes[a].season, episodes[b].season), cmp.Compare(episodes[a].episode, episodes[b].episode))
	})
	unique := func(i int) bool { return numbers[i] > 0 && claims[numbers[i]] == 1 }
	offset := func(i int) int { return numbers[i] - episodes[i].episode }

	checked := make(map[episodeKey]int)
	for k, i := range order {
		if !unique(i) {
			continue
		}
		agrees := func(j int) bool {
			return j >= 0 && j < len(order) && unique(order[j]) &&
				episodes[order[j]].season == episodes[i].season && offset(order[j]) == offset(i)
		}
		if agrees(k-1) || agrees(k+1) {
			checked[episodeKey{episodes[i].season, episodes[i].episode}] = numbers[i]
		}
	}
	return checked
}

// byNumber finds the wanted episodes of a serial kept in blocks by TVP's
// numbers for them. numbered is false if none of them has a number, so other
// ways can be tried.
func (p *Provider) byNumber(ctx context.Context, serial product, bs []block, q provider.Query, tv *series) (items []provider.Item, numbered bool, err error) {
	numbers, err := p.episodeNumbers(ctx, tv, serial.Title, q.Season, time.Now())
	if err != nil {
		return nil, false, err
	}
	for _, e := range tv.episodes {
		if e.season != q.Season || (q.Episode != 0 && e.episode != q.Episode) {
			continue
		}
		n, ok := numbers[episodeKey{e.season, e.episode}]
		if !ok {
			continue
		}
		numbered = true
		ep, ok, err := p.numbered(ctx, serial.ID, bs, n)
		if err != nil {
			return nil, false, err
		}
		if !ok || ep.Payable || (ep.Year > 0 && !e.aired.IsZero() && abs(ep.Year-e.aired.Year()) > maxYearsApart) {
			continue
		}
		items = append(items, episodeItem(serial, ep, e.season, e.episode))
	}
	return items, numbered, nil
}

// numbered returns the serial's only episode with TVP's number n.
func (p *Provider) numbered(ctx context.Context, serialID int64, bs []block, n int) (product, bool, error) {
	var match []product
	for _, b := range bs {
		if n < b.first || (b.last != 0 && n > b.last) {
			continue
		}
		eps, err := p.episodes(ctx, serialID, b.season.ID)
		if err != nil {
			return product{}, false, err
		}
		for _, e := range eps {
			if e.Number == n {
				match = append(match, e)
			}
		}
	}
	if len(match) != 1 {
		return product{}, false, nil
	}
	return match[0], true, nil
}
