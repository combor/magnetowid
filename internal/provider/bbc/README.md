# BBC iPlayer

Add [BBC iPlayer](https://www.bbc.co.uk/iplayer) as a Newznab indexer at
`http://<magnetowid-host>:8484/bbc`, API path `/api`.
See [Setup and configuration](../../../docs/usage.md) for connection settings.

iPlayer streams only to the UK, where watching or downloading BBC programmes
requires a TV licence. Elsewhere, searches find programmes but leave every
release out as unavailable.

## Series

Sonarr searches by TVDB ID before trying its title. magnetowid gets Sonarr's
title and TVDB's episodes from Skyhook, finds the iPlayer programme with that
title, and pairs each TVDB episode with one of iPlayer's:

1. **Title**, if no other episode on either side shares it, e.g. *The Reality War*.
2. **UK air date**, in order when several episodes aired that day. EastEnders'
   *29/09/2026* is TVDB's S42E155.
3. **Air date a day apart**, if only one episode on each side qualifies.
4. **iPlayer's numbering**, e.g. "Series 4: Episode 3" as S04E03, unless the
   titles differ.

Titles and numbers never pair an iPlayer episode that aired over a year
before TVDB's, as a remake's original would. iPlayer dates box sets by their
release, which can precede TVDB's weekly air dates by weeks.

Releases use Sonarr's title and TVDB's numbering. TVDB's qualifiers "(2023)"
and "(UK)" are dropped, so *Doctor Who (2023)* finds iPlayer's *Doctor Who*;
other countries' remain, so *The Traitors (US)* finds *The Traitors US*.
Alternative titles are tried only if Sonarr's finds nothing. Of several
programmes with the same title, the one matching most episodes is used.

Title searches, which Sonarr makes after an ID search finds nothing, first
find the TVDB series with that title on Skyhook, as Sonarr writes it, e.g.
"Doctor Who 2023", then search as by ID. Alternative titles count only if
no main title matches, and exactly one series must: "Traitors" names both
Channel 4's *Traitors* and *The Traitors*, so it finds nothing. iPlayer's
numbering alone would make 2024's *Space Babies* classic Doctor Who's S01E01.

Skyhook data is cached for a day and failed lookups for 5 minutes; iPlayer's
listings for 10 minutes. Allow access to `skyhook.sonarr.tv`.

## Overrides

[Overrides](../../../docs/usage.md#correcting-matches) use iPlayer's numbering:

- `site_season` is iPlayer's series, and episodes are its episode numbers, as
  in *Series 4: Episode 3*. Unnumbered episodes need an `episodes` entry.
- IDs are programme IDs (PIDs), e.g. `m002d3lr` in
  `https://www.bbc.co.uk/iplayer/episode/m002d3lr/doctor-who-season-2-8-the-reality-war`.
  A series' `id` is on its episodes page, e.g.
  `https://www.bbc.co.uk/iplayer/episodes/p0gglvqn/doctor-who`. Overrides
  also accept iPlayer and `bbc.co.uk/programmes` URLs.

## Films

Radarr's title must match iPlayer's, and its year iPlayer's within one year.
iPlayer usually gives the release year, but the UK premiere's for some films.
Only one-off programmes count as films.

## New episodes and films

A series found by TVDB ID joins a permanent watch list, even if the requested
episode is missing. Radarr searches with a year add the film to another. Both
are saved in `.magnetowid-jobs.db`.

The feeds offer watched series' episodes that iPlayer made available within
14 days, and watched films among the 200 it added most recently. Feeds rebuild
in the background once 10 minutes old or when a series is added; the first
sync after startup returns a placeholder. Releases are dated when first found,
so Sonarr's RSS cutoff cannot hide episodes of newly watched series.

Specials (TVDB season 0) are offered through RSS only: magnetowid does not
answer Sonarr's searches for specials.

## Streams

iPlayer's TV playlists list up to 720p50 with 128 kbit/s stereo AAC. Where a
programme is available in 1080p, magnetowid adds the 1080p50 rendition those
playlists omit, as get_iplayer does. Audio-described and signed versions are
skipped. Stream URLs expire after 6 hours, so they are fetched at download
time; if a CDN refuses the playlist, the others are tried.

iPlayer does not tag its audio's language, so release names have none.
Sonarr and Radarr then assume the series' or film's original language.

## Subtitles

iPlayer's English subtitles are for the deaf and hard of hearing. They are
saved as `<release>.eng.sdh.srt`, with speakers' colours. See
[Subtitles](../../../docs/usage.md#subtitles) to enable import.
