# TVP VOD

The `tvp` provider serves [TVP VOD](https://vod.tvp.pl). Add it to Sonarr and
Radarr as a Newznab indexer at `http://<vodarr-host>:8484/tvp` with API path
`/api`. See [Setup and configuration](../../../docs/usage.md) for everything
else.

## Series titles

TVP lists titles only in Polish. Sonarr's series titles come from TVDB and are
often English, e.g. "Days of Honor" for *Czas honoru*, so a search by Sonarr's
title finds nothing. For TVP, vodarr also accepts Sonarr's search by TVDB ID,
which Sonarr runs before its search by title:

1. vodarr gets Sonarr's title for the series from Skyhook, the metadata service
   Sonarr itself uses. It finds the Polish title on Wikidata, from the series'
   TVDB ID or, if Wikidata doesn't have that, its IMDb ID.
2. It searches TVP with the Polish title and names the releases with Sonarr's
   title, so Sonarr imports them automatically.
3. If the Polish title finds nothing, it searches TVP with Sonarr's title.
   Series such as *Ranczo* have the same title in both.
4. If a lookup fails, it returns nothing and Sonarr searches by its title as
   before.

vodarr keeps the titles it looks up for a day, and retries a failed lookup after
5 minutes.

To use this:

- vodarr must be able to reach `skyhook.sonarr.tv` and `www.wikidata.org`.
- Sonarr keeps an indexer's search options for up to 7 days. After upgrading
  from vodarr 0.2 or earlier, restart Sonarr so it starts searching TVP by
  TVDB ID.

Limitations:

- A series without a Polish title on Wikidata is found only if Sonarr's title
  matches TVP's.
- Skyhook is Sonarr's own service, not a public API, and may change without
  notice.

## Long soaps

TVP keeps long soaps such as *M jak miłość*, *Klan* and *Barwy szczęścia* in
blocks of 100 episode numbers, titled by range ("1801–1900", "1901–"), not in
seasons. The blocks have nothing to do with TVDB's seasons: TVP's block 20 of
*M jak miłość* starts at no. 1901, but TVDB's S20E01 is no. 1452. So vodarr
never maps a TVDB season onto a block. It finds an episode by TVP's number
for it, when Sonarr searches by TVDB ID. The number comes from:

- **TVP's TV guide:** it lists each broadcast on TVP 1 and TVP 2 with its
  number ("M jak miłość - odc. 1943"). vodarr looks for the broadcast within
  an hour of TVDB's air time, and takes the newest if reruns air nearby. The
  guide keeps about three weeks, so it numbers the recent episodes, which the
  new-episode feed needs. vodarr asks it only when searching a season with
  recent episodes, so older seasons don't depend on it.
- **TVDB's absolute number,** and a TVDB episode title that is only the
  number ("Odcinek 1452", "1945"). They number older episodes.
- **Season 1's episode numbers,** as season 1 counts from the first episode.

TVDB's numbers are patchy and sometimes wrong, so vodarr uses a number only
if its sources agree, no other episode has it, and a neighbouring episode of
the same TVDB season has a number at the same offset. It also skips an
episode whose year on TVP is more than 2 years from TVDB's air date.

Limitations:

- Episodes that neither the guide nor TVDB numbers aren't found. That
  includes all of *M jak miłość*'s season 26 (2025/26), and each episode
  once the guide drops it, unless TVDB numbers it.
- Otherwise vodarr finds an episode only when Sonarr's episode number is
  TVP's own, as in TVDB's *Klan* S15E2113. A search by title, without a TVDB
  ID, also takes season 1's episode numbers as TVP's.
- TVDB's *Klan* ends in 2011 and its *Barwy szczęścia* in April 2024. Sonarr
  never asks for later episodes until they are added to TVDB.

## New episodes

TVP has no list of new episodes, so vodarr watches the series Sonarr cares
about:

- **Watch list:** when Sonarr searches by TVDB ID and vodarr finds the series
  on TVP, vodarr watches it, even if the episode isn't there yet. The list is
  kept in `.vodarr-jobs.db` and survives restarts. Series stay on it.
- **Air dates:** for each watched series, vodarr takes from Skyhook the
  episodes that aired in the last 14 days, or are due in the next day, and
  searches TVP for each, as Sonarr would. It never maps a TVP episode back to
  Sonarr's numbering: TVP's *M jak miłość* no. 1943, in TVP's season 20, is
  TVDB's S27E07, and would otherwise be named S20E43.
- **Background updates:** the feed is rebuilt in the background at most every
  10 minutes, so RSS sync never waits for TVP. The first RSS sync after vodarr
  starts gets the placeholder.
- **Early premieres:** TVP lists some episodes a few days before broadcast as
  paid premieres, and makes them free at broadcast. vodarr offers an episode
  only once it is free, dated when vodarr first found it.
- **Response cache:** vodarr keeps TVP's search results, season lists and
  episode lists for 10 minutes. This serves the feed, and the per-episode
  searches Sonarr runs when a season search finds nothing. Stream URLs aren't
  kept: TVP ties them to the requesting IP address.

