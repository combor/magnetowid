# TVP VOD

Add [TVP VOD](https://vod.tvp.pl) as a Newznab indexer at
`http://<magnetowid-host>:8484/tvp`, API path `/api`.
See [Setup and configuration](../../../docs/usage.md) for connection settings.

## Installation and VPN access

Choose the [installation method for your platform](../../../docs/usage.md#install-and-run):
a Linux package, Homebrew, Scoop, Nix, a release archive, Docker, or a source build.
TVP uses the same indexer and download-client settings with every installation.

Some TVP titles require a Polish IP address. If you need a VPN, use an endpoint
in Poland and route catalogue requests, video downloads, subtitles, and DNS
through it. Stream URLs are tied to the requesting IP, so resolving a stream
and downloading it must use the same connection.

Keep your usual magnetowid installation and service. You can route it through
your VPN client's system tunnel or application routing, including FFmpeg.
Alternatively, configure [Gluetun's HTTP proxy](../../../examples/gluetun/README.md#http-proxy)
in magnetowid's environment. The example includes downloadable settings and
an override for the systemd service supplied by `.deb`, `.rpm`, and AUR packages.

Read the [FFmpeg proxy compatibility note](../../../examples/gluetun/README.md#ffmpeg-compatibility)
before using the HTTP proxy: affected builds can search successfully but fail
HTTPS downloads. The [Privoxy workaround](../../../examples/gluetun/README.md#native-workaround-with-privoxy)
avoids the malformed tunnel response while keeping magnetowid and FFmpeg native.
Linux users can also use the optional
[network namespace setup](../../../examples/gluetun/README.md#linux-network-namespace)
to route the native service through Gluetun without that proxy dependency.

Follow [Sonarr / Radarr setup](../../../docs/usage.md#sonarr--radarr-setup)
using a magnetowid address and download path your apps can reach. Test both
connections, then verify a complete download and library import, including
[subtitles](../../../docs/usage.md#subtitles) if enabled.

## Series titles

TVP uses Polish titles; Sonarr often uses English ones, such as "Days of Honor"
for *Czas honoru*. Sonarr searches by TVDB ID before trying its title.
magnetowid uses that ID to:

1. Get Sonarr's title from Skyhook and Polish titles from Wikidata, falling
   back to IMDb ID if Wikidata has no TVDB match.
2. Search TVP with the Polish titles, then Sonarr's title. Releases use
   Sonarr's title for automatic import.
3. Return no results on lookup failure, allowing Sonarr's title search to run.

Titles are cached for a day; failed lookups for 5 minutes. Allow access to
`skyhook.sonarr.tv` and `www.wikidata.org`. After upgrading from 0.2.0 or earlier,
restart Sonarr to refresh its indexer capabilities, otherwise cached for up to
7 days.

Without a Polish Wikidata title, Sonarr's title must match TVP's. Skyhook is
Sonarr's internal service and may change without notice.

## Long soaps

TVP groups *M jak miłość*, *Klan*, and *Barwy szczęścia* into blocks of 100
absolute episode numbers. These are not TVDB seasons: *M jak miłość* block 20
starts at 1901, but TVDB S20E01 is episode 1452.

For TVDB ID searches, magnetowid derives TVP's episode number from:

- **TVP's TV guide:** broadcast titles on TVP 1 and TVP 2 give episode numbers.
  Match within an hour of TVDB's air time, choosing the newest episode if
  reruns overlap. The guide retains about three weeks; only searches for
  seasons with recent episodes use it.
- **TVDB:** absolute numbers or numeric episode titles such as "Odcinek 1452".
- **Season 1:** episode numbers count from the series' start.

Known sources must agree. A number must be unique and share its offset with
an adjacent episode in the same TVDB season. TVP's episode year must be
within 2 years of TVDB's air date.

Limitations:

- Unnumbered episodes disappear from search when the guide drops them. TVDB
  lacks numbers for all of *M jak miłość* season 26 (2025/26).
- Without a validated mapping, Sonarr's episode number must already be TVP's
  absolute number, as in *Klan* S15E2113. Title-only searches also support
  season 1 numbering.
- TVDB's *Klan* ends in 2011 and *Barwy szczęścia* in April 2024. Sonarr cannot
  request later episodes until TVDB adds them.

## New episodes

TVP has no new-episode feed. A successful TVDB series match adds the series to
a permanent watch list, even if the requested episode is missing. The list is
saved in `.magnetowid-jobs.db`.

For watched series, the feed searches TVP for Skyhook episodes aired within
14 days or due within a day. Searches use TVDB numbering; reversing TVP's
numbering could select the wrong episode.

RSS requests trigger background rebuilds after the 10-minute cache expires
or a series is added. The first sync after startup returns a placeholder.
Paid premieres appear only when free, dated when first discovered so Sonarr's
RSS cutoff cannot hide them.

TVP search results, season lists, and episode lists are cached for 10 minutes.
Stream URLs expire and are tied to the requester's IP, so they are never cached.

## New films

Radarr searches add films to a permanent watch list by title and year, even
when found, because stream probing may still fail. Original and local titles
can produce separate entries. The list is saved in `.magnetowid-jobs.db`.

RSS requests trigger a background update after the 10-minute cache expires.
It checks TVP's 100 newest products (about two and a half weeks). Free films
match by title or original title, allowing a one-year difference. Original
titles require a search lookup, skipped when no watched year matches.
Releases use Radarr's title and year and are dated when first found free.

Films outside the newest 100 products need a manual search, even if they
became free later.

## Subtitles

TVP offers Polish subtitles for the deaf and hard of hearing and Ukrainian
subtitles for versions such as *UA Ranczo*. Files use TVP's ISO 639-2 language
codes, e.g. `<release>.pol.sdh.srt` or `<release>.ukr.srt`, and preserve speaker
colours. See [Subtitles](../../../docs/usage.md#subtitles) to enable import.
