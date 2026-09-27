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
3. If a lookup fails, or the Polish title is the same as Sonarr's, it returns
   nothing and Sonarr searches by its title as before.

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
