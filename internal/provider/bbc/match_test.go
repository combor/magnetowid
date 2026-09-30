package bbc

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/combor/magnetowid/internal/provider"
)

// ep builds a BBC episode aired on date; broadcast is "HH:MM" or "".
func ep(id, subtitle, originalTitle, date, broadcast string) programme {
	fb := ""
	if date != "" {
		fb = date + "T00:00:00.000Z"
		if broadcast != "" {
			fb = date + "T" + broadcast + ":00.000Z"
		}
	}
	var rt string
	if date != "" {
		rt = date + "T00:00:00.000Z"
	}
	return programme{ID: id, Type: "episode", TLEOType: "brand", Subtitle: subtitle, OriginalTitle: originalTitle,
		ReleaseTime: rt, Versions: []version{{ID: id + "v", Kind: "original", FirstBroadcast: fb}}}
}

func tvdb(season, episode int, title, aired string) tvdbEpisode {
	return tvdbEpisode{season: season, episode: episode, title: title, aired: aired}
}

// Return "S01E02=id" for each pair, sorted.
func pairs(ps []pair) []string {
	var out []string
	for _, p := range ps {
		out = append(out, fmt.Sprintf("S%02dE%02d=%s", p.tvdb.season, p.tvdb.episode, p.bbc.ID))
	}
	slices.Sort(out)
	return out
}

func checkMatch(t *testing.T, name string, tv []tvdbEpisode, eps []programme, want ...string) {
	t.Helper()
	slices.Sort(want)
	if got := pairs(match(tv, eps, provider.SeriesOverride{})); !slices.Equal(got, want) {
		t.Errorf("%s:\n got %s\nwant %s", name, strings.Join(got, " "), strings.Join(want, " "))
	}
}

func TestMatchTitles(t *testing.T) {
	// Doctor Who (2023): TVDB marks parts and uses "&"; BBC's specials have no series.
	tv := []tvdbEpisode{
		tvdb(0, 1, "The Star Beast", "2023-11-25"),
		tvdb(0, 4, "The Church on Ruby Road", "2023-12-25"),
		tvdb(1, 1, "Space Babies", "2024-05-11"),
		tvdb(1, 2, "The Devil's Chord", "2024-05-11"),
		tvdb(2, 5, "The Story & the Engine", "2025-05-10"),
		tvdb(2, 7, "Wish World (1)", "2025-05-24"),
	}
	eps := []programme{
		ep("star", "Specials: 1. The Star Beast", "The Star Beast", "2023-11-25", "18:30"),
		ep("ruby", "Christmas Special: The Church on Ruby Road", "", "2023-12-25", ""),
		// Listed out of order, aired on the same day.
		ep("chord", "Season 1: 2. The Devil's Chord", "The Devil's Chord", "2024-05-11", "18:05"),
		ep("babies", "Season 1: 1. Space Babies", "Space Babies", "2024-05-11", "17:20"),
		ep("engine", "Season 2: 5. The Story and the Engine", "The Story and the Engine", "2025-05-10", ""),
		// A different date: titles decide.
		ep("wish", "Season 2: 7. Wish World", "Wish World", "2025-05-25", ""),
	}
	checkMatch(t, "Doctor Who", tv, eps,
		"S00E01=star", "S00E04=ruby", "S01E01=babies", "S01E02=chord", "S02E05=engine", "S02E07=wish")
}

func TestMatchDates(t *testing.T) {
	// The Traitors: generic titles, "The Final" in two series, three episodes on one day.
	tv := []tvdbEpisode{
		tvdb(1, 1, "Episode 1", "2022-11-29"),
		tvdb(1, 2, "Episode 2", "2022-11-29"),
		tvdb(1, 3, "Episode 3", "2022-11-29"),
		tvdb(3, 12, "The Final", "2025-01-24"),
		tvdb(4, 12, "The Final", "2026-01-23"),
	}
	eps := []programme{
		ep("t4f", "Series 4: 12. The Final", "The Final", "2026-01-23", "20:30"),
		ep("t3f", "Series 3: 12. The Final", "The Final", "2025-01-24", ""),
		// Only the first has a broadcast time; BBC's numbers order the day.
		ep("t13", "Series 1: Episode 3", "Episode 3", "2022-11-29", ""),
		ep("t11", "Series 1: Episode 1", "Episode 1", "2022-11-29", "21:30"),
		ep("t12", "Series 1: Episode 2", "Episode 2", "2022-11-29", ""),
	}
	checkMatch(t, "The Traitors", tv, eps, "S01E01=t11", "S01E02=t12", "S01E03=t13", "S03E12=t3f", "S04E12=t4f")

	// EastEnders: TVDB numbers by year and names parts differently.
	tv = []tvdbEpisode{
		tvdb(38, 105, "04/07/2022 (1)", "2022-07-04"),
		tvdb(38, 106, "04/07/2022 (2)", "2022-07-04"),
		tvdb(41, 84, "27/08/2025", "2025-05-27"), // TVDB's typo
		tvdb(42, 155, "29/09/2026", "2026-09-29"),
	}
	eps = []programme{
		ep("p1", "04/07/2022 - Part 1", "04/07/2022 - Part 1", "2022-07-04", "19:30"),
		ep("p2", "04/07/2022 - Part 2", "04/07/2022 - Part 2", "2022-07-04", "20:00"),
		ep("typo", "27/05/2025", "27/05/2025", "2025-05-27", ""),
		ep("today", "29/09/2026", "29/09/2026", "2026-09-29", ""),
	}
	checkMatch(t, "EastEnders", tv, eps, "S38E105=p1", "S38E106=p2", "S41E84=typo", "S42E155=today")

	// iPlayer lists a day's parts in reverse; BBC's positions order them.
	tv = []tvdbEpisode{tvdb(41, 111, "", "2025-07-15"), tvdb(41, 112, "", "2025-07-15")}
	part1, part2 := ep("p1", "15/07/2025, Part 1", "", "2025-07-15", ""), ep("p2", "15/07/2025, Part 2", "", "2025-07-15", "")
	part1.ParentPosition, part2.ParentPosition = 7154, 7155
	checkMatch(t, "parts", tv, []programme{part2, part1}, "S41E111=p1", "S41E112=p2")
	// Without a position for each, broadcast times order the day.
	part1, part2 = ep("p1", "15/07/2025, Part 1", "", "2025-07-15", "19:30"), ep("p2", "15/07/2025, Part 2", "", "2025-07-15", "20:00")
	part1.ParentPosition = 7154
	checkMatch(t, "parts, one position", tv, []programme{part2, part1}, "S41E111=p1", "S41E112=p2")

	// A day's counts must agree.
	tv = []tvdbEpisode{tvdb(1, 1, "", "2020-01-01"), tvdb(1, 2, "", "2020-01-01")}
	checkMatch(t, "unequal day", tv, []programme{ep("only", "Pilot", "Pilot", "2020-01-01", "")})
}

func TestMatchNearDay(t *testing.T) {
	// A late broadcast TVDB dates the next day.
	tv := []tvdbEpisode{tvdb(1, 1, "", "2020-01-02"), tvdb(1, 2, "", "2020-01-09")}
	eps := []programme{ep("a", "Late Show", "", "2020-01-01", "23:40"), ep("b", "Later Show", "", "2020-01-08", "23:40")}
	checkMatch(t, "late", tv, eps, "S01E01=a", "S01E02=b")

	// Episodes the day before and after are ambiguous.
	tv = []tvdbEpisode{tvdb(1, 1, "", "2020-01-02")}
	eps = []programme{ep("a", "One", "", "2020-01-01", ""), ep("b", "Two", "", "2020-01-03", "")}
	checkMatch(t, "either side", tv, eps)

	// Different titles contradict a near date.
	tv = []tvdbEpisode{tvdb(1, 1, "Arrival", "2020-01-02")}
	checkMatch(t, "near, other title", tv, []programme{ep("x", "Departure", "Departure", "2020-01-01", "")})
}

func TestMatchNumbers(t *testing.T) {
	// BBC airs acquired series later, with its own titles.
	tv := []tvdbEpisode{tvdb(1, 1, "Pilot", "2023-01-12"), tvdb(1, 2, "Episode 2", "2023-01-19")}
	eps := []programme{
		ep("s1e1", "Series 1: Episode 1", "Episode 1", "2023-06-12", ""),
		ep("s1e2", "Series 1: 2. The Second One", "The Second One", "2023-06-12", ""),
	}
	checkMatch(t, "acquired", tv, eps, "S01E01=s1e1", "S01E02=s1e2")

	// Numbers can't pair different titles.
	tv = []tvdbEpisode{tvdb(1, 1, "An Unearthly Child", "1963-11-23")}
	checkMatch(t, "classic", tv, []programme{ep("babies", "Season 1: 1. Space Babies", "Space Babies", "2024-05-11", "")})

	// A remake's original aired long before it, by title or number.
	tv = []tvdbEpisode{tvdb(1, 1, "Episode 1", "2024-01-01"), tvdb(1, 2, "Pilot", "2024-01-08")}
	eps = []programme{
		ep("uk1", "Series 1: Episode 1", "Episode 1", "2001-07-09", ""),
		ep("uk2", "Series 1: 2. Pilot", "Pilot", "2001-07-16", ""),
	}
	checkMatch(t, "remake", tv, eps)

	// iPlayer dates a box set by its release, weeks before TVDB's broadcasts.
	tv = []tvdbEpisode{tvdb(4, 1, "Episode 1", "2026-09-29"), tvdb(4, 2, "Episode 2", "2026-10-13"),
		tvdb(4, 6, "The Line", "2026-11-03")}
	eps = []programme{
		ep("e1", "Series 4: Episode 1", "Episode 1", "2026-09-29", "20:00"),
		ep("e2", "Series 4: Episode 2", "Episode 2", "2026-09-29", ""),
		ep("e6", "Series 4: 6. The Line", "The Line", "2026-09-29", ""),
	}
	checkMatch(t, "box set", tv, eps, "S04E01=e1", "S04E02=e2", "S04E06=e6")
	// Without episode 1, the release day's other episode is not TVDB's first.
	checkMatch(t, "box set without episode 1", tv, eps[1:], "S04E02=e2", "S04E06=e6")
	checkMatch(t, "box set with episode 2 alone", tv, eps[1:2], "S04E02=e2")

	// TVDB lacking episodes names S07E03 "Episode 5"; it aired as BBC's episode 5.
	tv = []tvdbEpisode{tvdb(7, 3, "Episode 5", "2021-03-19"), tvdb(7, 5, "Episode 8", "2021-03-24")}
	eps = []programme{ep("rs5", "Series 7: Episode 5", "Episode 5", "2021-03-19", ""),
		ep("rs8", "Series 7: Episode 8", "Episode 8", "2021-03-24", "")}
	checkMatch(t, "TVDB gaps", tv, eps, "S07E03=rs5", "S07E05=rs8")

	// A newer series with the same title is not a UK showing.
	tv = []tvdbEpisode{tvdb(1, 1, "Episode 1", "1975-04-16")}
	checkMatch(t, "newer remake", tv, []programme{ep("remake", "Series 1: Episode 1", "Episode 1", "2008-11-23", "")})

	// Two BBC episodes with one number are ambiguous.
	tv = []tvdbEpisode{tvdb(2, 1, "", "")}
	eps = []programme{ep("a", "Series 2: Episode 1", "", "", ""), ep("b", "Series 2: Episode 1", "", "", "")}
	checkMatch(t, "duplicate", tv, eps)
}
