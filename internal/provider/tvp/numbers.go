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

// TVP groups long soaps into blocks of 100 episodes; TVDB groups them by year.
// Validate absolute numbers, numeric titles, and guide matches before using them.

// Allow a two-year discrepancy; larger gaps suggest a wrong episode number.
const maxYearsApart = 2

type block struct {
	season      product
	first, last int // last is 0 for the latest, open block
}

// TVP block titles: "1–100", "801-900", or the open-ended "1901–".
var blockTitle = regexp.MustCompile(`^\s*(\d+)\s*[–-]\s*(\d*)\s*$`)

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

// TVDB numeric titles include "1945", "Episode 75", and "Odcinek 846 (12.09.2011)".
var numberTitle = regexp.MustCompile(`(?i)^\s*(?:(?:odcinek|episode|odc\.)\s*)?(\d+)\s*(?:\(.*\))?\s*$`)

func titleNumber(title string) int {
	m := numberTitle.FindStringSubmatch(title)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// Conflicting sources invalidate a number; later sources cannot override this.
const conflicting = -1

// Zero means unknown; conflicting known values invalidate the number.
func agreed(a, b int) int {
	switch {
	case a == 0:
		return b
	case b == 0 || a == b:
		return a
	}
	return conflicting
}

type episodeKey struct{ season, episode int }

// Consult the guide only for seasons with recent episodes, keeping old-season searches independent.
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

// Accept only unique numbers whose offset agrees with an adjacent episode in
// the same season. This rejects isolated typos and incorrect guide matches.
// numbers[i] belongs to episodes[i]; nonpositive values are unknown or conflicting.
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

// numbered=false allows fallbacks when none of the requested episodes has a validated number.
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

// Reject duplicate matches for the same absolute number.
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
