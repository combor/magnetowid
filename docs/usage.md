# Setup and configuration

magnetowid exposes two APIs for Sonarr and Radarr:

- **Newznab** at `/{provider}/api` for catalogue searches.
- **SABnzbd** at `/api` for downloads, progress, and completed MP4 files.

Supported sites: **TVP VOD** (`tvp`) and **BBC iPlayer** (`bbc`). See the
[TVP VOD notes](../internal/provider/tvp/README.md) and the
[BBC iPlayer notes](../internal/provider/bbc/README.md).

## Install and run

### Docker Compose

Follow the [quick start](../README.md#quick-start) to download the
[Compose configuration](../compose.yaml) and create `.env` from
[the example](../.env.example). The image is `ghcr.io/combor/magnetowid:latest`
and includes ffmpeg.

The Compose example reads these settings from `.env`:

| Setting | Default | Description |
|---|---|---|
| `MAGNETOWID_API_KEY` | required | A long, random key shared by both APIs. |
| `MAGNETOWID_DOWNLOAD_PATH` | `./downloads` | Host folder mounted at `/downloads` inside magnetowid. |
| `MAGNETOWID_UID` | `1000` | User ID for the container process. |
| `MAGNETOWID_GID` | `1000` | Group ID for the container process. |

Create the download folder before starting. On Linux, use its owner's user
and group IDs; `id -u` and `id -g` show your current account's IDs. Give
magnetowid and Sonarr/Radarr write access to the shared folder. See
[Docker networking and shared downloads](#docker-networking-and-shared-downloads).

Run `docker compose up -d` to start or apply `.env` changes. To update:

```sh
docker compose pull
docker compose up -d
```

### Linux service

The `magnetowid-bin` AUR package, and the `.deb` and `.rpm` packages on the
[releases page](https://github.com/combor/magnetowid/releases/latest), install
magnetowid as a systemd service. Set `MAGNETOWID_API_KEY` in
`/etc/magnetowid/magnetowid.env`, then start magnetowid and enable it at boot:

```sh
sudo systemctl enable --now magnetowid
```

The service runs as `magnetowid:media`, matching the group used by Arch's
Sonarr and Radarr packages. Downloads default to `/var/lib/magnetowid/downloads`.
An alternative `MAGNETOWID_DOWNLOAD_DIR` must be writable by `media` and outside
`/home`.

If Sonarr/Radarr share a different group, run `sudo systemctl edit magnetowid`
and add:

```ini
[Service]
Group=yourgroup
```

Run `sudo systemctl restart magnetowid` after changing settings.

### Build from source

Requires Go 1.27.1+ and ffmpeg.

```sh
git clone https://github.com/combor/magnetowid.git
cd magnetowid
go build -o magnetowid ./cmd/magnetowid
./magnetowid -api-key YOUR_API_KEY -download-dir ./downloads
```

Replace `YOUR_API_KEY` with a long, random key and use it for both connections
in Sonarr/Radarr.

## Configuration

Set these environment variables or pass the equivalent command-line flags.
Flags take precedence. For Docker, add any extra variables to the `environment`
section in `compose.yaml`. For the Linux service, set them in
`/etc/magnetowid/magnetowid.env`.

| Flag | Env | Default | Description |
|---|---|---|---|
| `-listen` | `MAGNETOWID_LISTEN` | `:8484` | listen address |
| `-api-key` | `MAGNETOWID_API_KEY` | | required; used by both APIs |
| `-download-dir` | `MAGNETOWID_DOWNLOAD_DIR` | | required; downloads go to `<dir>/<category>/<release>/`; state is stored in `<dir>/.magnetowid-jobs.db` |
| `-categories` | `MAGNETOWID_CATEGORIES` | `tv,movies` | download categories to offer |
| `-ffmpeg` | `MAGNETOWID_FFMPEG` | `ffmpeg` | ffmpeg binary |
| `-log-level` | `MAGNETOWID_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error`; debug includes requests and RSS activity |

`GET /health` returns `OK` without an API key. The container health check runs
`magnetowid -healthcheck`, which queries `MAGNETOWID_LISTEN` and exits with 0
on success. Set a container's listen address through `MAGNETOWID_LISTEN`:
the health check cannot read the server's command-line flags.

Sonarr/Radarr must be able to read the download directory. If they see it at a different path (e.g. in containers), add a Remote Path Mapping.

## Sonarr / Radarr setup

1. **Download client:** Settings → Download Clients → **SABnzbd**.
   - Name: `magnetowid`.
   - Host and port: magnetowid's host and port.
   - API key: the one magnetowid was started with.
   - Category: `tv` (Sonarr) or `movies` (Radarr).
   - Priority: higher-priority jobs run first, one at a time. **Paused** queues the job without starting it. See [Pausing downloads](#pausing-downloads).
2. **Indexer:** Settings → Indexers → **Newznab**, one per site.
   - Name: e.g. "TVP VOD" or "BBC iPlayer".
   - URL: `http://<host>:8484/tvp` or `http://<host>:8484/bbc`, API path `/api`, the same API key.
   - Categories: 5000, 5040 (Sonarr) or 2000, 2040 (Radarr).
   - **Download Client:** select `magnetowid` to route its releases correctly.
3. **Language (Radarr, TVP VOD):** under Settings → Profiles, set **Language** to **Any**,
   or **Polish** for Polish audio only. The default, original language, rejects
   foreign films with TVP's Polish audio. Sonarr profiles have no language setting.

Test both connections. An empty feed produces a placeholder to pass the
indexer test; it cannot be downloaded.

Sonarr can search for specials and, for daily series, episodes by air date.
Releases always use TVDB's season and episode numbers, e.g. `S00E01` or
`S2026E187`, which Sonarr accepts for daily series too.

### New episodes and films (RSS)

For sites with RSS support, magnetowid watches titles after Sonarr or Radarr
searches for them. RSS sync, every 15 minutes by default, finds new episodes
and newly available films.

- Add new titles with a search enabled.
- For existing wanted titles, use **Search Monitored** on each Sonarr series
  page, and **Wanted → Missing → Search All** in Radarr.
- Feeds cover episodes aired within 14 days and films among the site's newest.
  Search manually for older releases. See each site's notes for support and limits.

### Subtitles

Subtitles are saved beside the video as SRT, with language and `sdh` labels
(subtitles for the deaf and hard of hearing), e.g. `<release>.pol.sdh.srt`.
To import them, enable **Import Extra Files** under **Settings → Media Management
→ Show Advanced** in both apps, with `srt` among the extensions (included by
default). Subtitle failures log a warning and keep the video.

### Pausing downloads

Use SABnzbd API commands to pause or resume the queue or individual jobs.
Sonarr and Radarr display paused jobs but cannot control pausing:

```sh
curl 'http://localhost:8484/api?mode=pause&apikey=YOUR_API_KEY'
curl 'http://localhost:8484/api?mode=resume&apikey=YOUR_API_KEY'
curl 'http://localhost:8484/api?mode=queue&name=resume&value=JOB_ID&apikey=YOUR_API_KEY'
```

`mode=queue&apikey=YOUR_API_KEY` lists job IDs (`nzo_id`). Use `name=pause`
and comma-separated IDs in `value` to pause jobs. Pauses survive restarts;
interrupted downloads restart from the beginning when resumed.

## Web interface

Open `http://<host>:8484/` in a browser and sign in with the API key. The page
shows the running download's progress and time left, the queued jobs in the
order they will run, and paused or retrying jobs with their last error. It
refreshes itself every second.

Signing in lasts 30 days. Changing `MAGNETOWID_API_KEY` signs every browser
out. Search for titles and manage downloads in Sonarr/Radarr.

## Correcting matches

If magnetowid misses a series or film, or pairs the wrong episodes, add an
override. The overrides API takes the same API key, in an `X-Api-Key` header
or an `apikey` parameter. Changes apply to the next search, and RSS feeds
rebuild with them at the next sync. Overrides are saved in
`.magnetowid-jobs.db`.

| Request | Purpose |
|---|---|
| `GET /overrides` | List each site's overrides. |
| `PUT /overrides/{site}/series/{tvdbid}` | Set a series' override. |
| `GET` or `DELETE /overrides/{site}/series/{tvdbid}` | Show or remove it. |
| `PUT /overrides/{site}/films/{year}/{title}` | Set a film's override, using Radarr's title and year. |
| `GET` or `DELETE /overrides/{site}/films/{year}/{title}` | Show or remove it. |

`{site}` is `tvp` or `bbc`. A series' TVDB ID is in its TVDB link in Sonarr.
Escape `/` in film titles as `%2F`.

A series override has one or more of:

| Field | Meaning |
|---|---|
| `titles` | The site's titles to search, instead of those magnetowid finds. |
| `id` | The site's series, among the search results for the titles, if several share a title. |
| `seasons` | Rules placing TVDB seasons in the site's numbering: TVDB's episode *n* is the site's episode *n* + `offset` in season `site_season`. `site_season` 0 accepts any season, if only one has that number. |
| `episodes` | Single TVDB episodes, such as `S01E05` or the special `S00E01`, each with the site's episode ID. These win over `seasons`, and no other episode matches a pinned one. |

A film override has `titles` to search instead of Radarr's, an `id`, or both.
The site's film with that `id`, found by searching the titles, is used even
if its title or year differs from Radarr's, and no other film matches it.

IDs accept the site's page URLs. For example, *Ranczo*'s second season
continues TVP's numbering from 14:

```sh
curl -X PUT -H 'X-Api-Key: YOUR_API_KEY' http://localhost:8484/overrides/tvp/series/81970 -d '{
  "titles": ["Ranczo"],
  "id": "https://vod.tvp.pl/seriale,18/ranczo-odcinki,316445",
  "seasons": [{"season": 2, "site_season": 2, "offset": 13}]
}'
```

The response shows the override as saved, with IDs taken from the URLs.
Invalid overrides are refused with an explanation.

An override replaces magnetowid's matching for everything it covers,
including its checks. An episode it places where the site has none, or only a
paid one, gets no release; other episodes are matched as before. Overrides
apply to searches and RSS, including the title searches Sonarr makes when its
TVDB ID search finds nothing. Specials are not supported. See each site's
notes for its numbering.

## Docker networking and shared downloads

Choose an address that Sonarr/Radarr can reach:

- If an app runs directly on the same host as magnetowid, use `localhost` and port `8484`.
- If both containers share a Docker network, use the service name `magnetowid` and port `8484`.
- For apps on another machine or a different Docker network, use the Docker
  host's reachable address and the published port `8484`.

Inside a container, `localhost` refers to that container. Separate Compose
projects do not share a network by default.

Mount the same host download folder into Sonarr/Radarr. The supplied Compose
file mounts it at `/downloads` in magnetowid, so mounting it at `/downloads` in
your apps gives them matching paths. magnetowid creates `tv` and `movies`
subfolders for the default categories.

If an app sees the folder at a different path, add a **Remote Path Mapping**
under **Settings → Download Clients**:

| Field | Value |
|---|---|
| Host | The host entered for the `magnetowid` download client. |
| Remote Path | `/downloads` |
| Local Path | The same shared folder as seen by Sonarr/Radarr, for example `/data/downloads`. |

## Troubleshooting

Search for titles and manage downloads in Sonarr/Radarr. The
[web interface](#web-interface) shows magnetowid's download queue.

| Problem | What to check |
|---|---|
| A connection test cannot reach magnetowid | Check the container is running, port `8484` is reachable, and the host is correct for your [network setup](#docker-networking-and-shared-downloads). |
| A connection reports an invalid API key | Use the same key for the indexer, download client and `MAGNETOWID_API_KEY`. Run `docker compose up -d` after changing `.env`. |
| The container cannot find or write to the download folder | Create `MAGNETOWID_DOWNLOAD_PATH` before starting and check that `MAGNETOWID_UID` and `MAGNETOWID_GID` have write access. |
| Downloads finish but are not imported | Mount the shared folder into Sonarr/Radarr, check file permissions and add a Remote Path Mapping if the paths differ. |
| A magnetowid release is sent to another download client | Set the indexer's **Download Client** to `magnetowid`. |
| Downloads stay queued with `provider unreachable` in the log | Check the network, DNS, and VPN. Jobs resume automatically when the site becomes reachable, without consuming retries. |
| A series or film is missing, or its episodes are wrong | Check that the site has it for free, then add an [override](#correcting-matches). |

To inspect recent container messages:

```sh
docker compose logs --tail 100 magnetowid
```

For the Linux service, use `journalctl -u magnetowid -n 100`.
Set `MAGNETOWID_LOG_LEVEL=debug` for request and RSS details.

For bugs or feature requests, [open an issue](https://github.com/combor/magnetowid/issues).
Include the steps to reproduce and any relevant error message, with API keys removed.

## Limitations

- **Titles:** Radarr searches local and original film titles. Sonarr sends its
  series title, often English; a different site title requires TVDB ID support.
  See each site's notes, or add an [override](#correcting-matches).
- **Episode numbers:** differences from TVDB require provider-specific mapping.
  This applies to both search and RSS; TVP's soap mapping and BBC's matching
  by title and air date have their own limits. [Overrides](#correcting-matches)
  correct the rest.
- **Availability:** DRM, paid, region-blocked, and unreadable streams are omitted
  from results. Availability can still change between search and download.
- **Streams:** release names use the selected stream's resolution, codecs, and
  known audio language. Probes run during search and are cached for a day.
  Downloads keep one audio track; subtitle conversion supports TTML only.
- **Restarts:** `.magnetowid-jobs.db` stores the queue, history, and watch lists.
  Completed history expires after 30 days. Interrupted downloads restart from
  the beginning. The download filesystem must support file locks; only one
  magnetowid instance can use it at a time.

## Adding a site

Implement `provider.Provider` (`internal/provider/provider.go`) in a new package under `internal/provider/` and add it to the registry in `cmd/magnetowid/main.go`. The provider:

- searches its catalogue and maps Sonarr/Radarr numbering onto its own;
- resolves an ID to a stream URL ffmpeg can open, at download time, and may
  amend an HLS master playlist, e.g. to add variants the site omits;
- optionally implements `provider.TVDBSearcher` to find series by TVDB ID, if
  Sonarr's titles don't match the site's;
- optionally implements `provider.RecentLister` to offer new releases to RSS
  sync;
- optionally implements `provider.Overridable` to apply the user's
  [overrides](#correcting-matches) in its searches and feeds;
- documents the site's own behaviour and limits in a `README.md` in its package.

## Development

| Command | Checks | Requirements |
|---|---|---|
| `make test` | Formatting, vet, race tests | ffmpeg with libx264 for download tests |
| `make docker-smoke` | Image build, startup, both APIs | Docker |
| `make package-smoke` | Install, run, upgrade, and remove packages on Debian, Fedora, and Arch | Docker, GoReleaser |
| `make integration` | Sonarr/Radarr search, RSS grabs, video and subtitle imports against fake VOD sites | Linux, Docker, ffmpeg with libx264, access to the apps' metadata servers |
| `make live` | Live TVP, BBC iPlayer, Skyhook, and Wikidata APIs; also runs daily in CI | Network access; BBC streams need a UK connection |

## License

[BSD-3-Clause](../LICENSE).
