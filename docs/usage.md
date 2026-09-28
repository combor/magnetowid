# Setup and configuration

magnetowid exposes two APIs for Sonarr and Radarr:

- **Newznab** at `/{provider}/api` for catalogue searches.
- **SABnzbd** at `/api` for downloads, progress, and completed MP4 files.

Supported sites: **TVP VOD** (`tvp`). See the [TVP VOD notes](../internal/provider/tvp/README.md).

## Install and run

Choose the format for your operating system:

| Platform | Installation options |
|---|---|
| Linux | [`.deb` or `.rpm` packages](https://github.com/combor/magnetowid/releases/latest), [AUR `magnetowid-bin`](https://aur.archlinux.org/packages/magnetowid-bin), or a release archive |
| macOS | [Homebrew tap](https://github.com/combor/homebrew-tap) or a release archive |
| Windows (x86-64) | [Scoop bucket](https://github.com/combor/scoop-bucket) or a release archive |
| FreeBSD / OpenBSD | [Release archives](https://github.com/combor/magnetowid/releases/latest) |
| Nix on Linux or macOS | [Nix package](https://github.com/combor/nur) |
| Docker with Linux containers | [Compose setup](#docker-compose), including Docker Desktop on macOS and Windows |

Release archives are available on the
[releases page](https://github.com/combor/magnetowid/releases/latest). Choose
your OS and architecture, extract the binary, and install ffmpeg separately.
Use the [configuration settings](#configuration) to supply an API key and
download directory. You can also [build from source](#build-from-source).

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

The `.deb` and `.rpm` packages on the
[releases page](https://github.com/combor/magnetowid/releases/latest), and the
`magnetowid-bin` AUR package, install
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
   - Name: e.g. "TVP VOD".
   - URL: `http://<host>:8484/tvp`, API path `/api`, the same API key.
   - Categories: 5000, 5040 (Sonarr) or 2000, 2040 (Radarr).
   - **Download Client:** select `magnetowid` to route its releases correctly.
3. **Language (Radarr):** under Settings → Profiles, set **Language** to **Any**,
   or **Polish** for Polish audio only. The default, original language, rejects
   foreign films with TVP's Polish audio. Sonarr profiles have no language setting.

Test both connections. An empty feed produces a placeholder to pass the
indexer test; it cannot be downloaded.

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

magnetowid provides APIs and has no separate web interface. Search for titles
and manage downloads in Sonarr/Radarr.

| Problem | What to check |
|---|---|
| A connection test cannot reach magnetowid | Check the container is running, port `8484` is reachable, and the host is correct for your [network setup](#docker-networking-and-shared-downloads). |
| A connection reports an invalid API key | Use the same key for the indexer, download client and `MAGNETOWID_API_KEY`. Run `docker compose up -d` after changing `.env`. |
| The container cannot find or write to the download folder | Create `MAGNETOWID_DOWNLOAD_PATH` before starting and check that `MAGNETOWID_UID` and `MAGNETOWID_GID` have write access. |
| Downloads finish but are not imported | Mount the shared folder into Sonarr/Radarr, check file permissions and add a Remote Path Mapping if the paths differ. |
| A magnetowid release is sent to another download client | Set the indexer's **Download Client** to `magnetowid`. |
| Downloads stay queued with `provider unreachable` in the log | Check the network, DNS, and VPN. Jobs resume automatically when the site becomes reachable, without consuming retries. |

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
  See each site's notes.
- **Episode numbers:** differences from TVDB require provider-specific mapping.
  This applies to both search and RSS; TVP's soap mapping has its own limits.
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
- resolves an ID to a stream URL ffmpeg can open, at download time;
- optionally implements `provider.TVDBSearcher` to find series by TVDB ID, if
  Sonarr's titles don't match the site's;
- optionally implements `provider.RecentLister` to offer new releases to RSS
  sync;
- documents the site's own behaviour and limits in a `README.md` in its package.

## Development

| Command | Checks | Requirements |
|---|---|---|
| `make test` | Formatting, vet, race tests | ffmpeg with libx264 for download tests |
| `make docker-smoke` | Image build, startup, both APIs | Docker |
| `make package-smoke` | Install, run, upgrade, and remove packages on Debian, Fedora, and Arch | Docker, GoReleaser |
| `make integration` | Sonarr/Radarr search, RSS grabs, video and subtitle imports against fake VOD sites | Linux, Docker, ffmpeg with libx264, access to the apps' metadata servers |
| `make live` | Live TVP, Skyhook, and Wikidata APIs; also runs daily in CI | Network access |

## License

[BSD-3-Clause](../LICENSE).
