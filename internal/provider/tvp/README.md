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

