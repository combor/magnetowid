# Setup and configuration

Downloads movies and series from video-on-demand sites for Sonarr and Radarr.
vodarr needs no changes to either app: it looks like two things they already support.

- **A Newznab indexer per site** at `/{provider}/api`, so they can search the site's catalogue.
- **A SABnzbd download client** at `/api`, so they can send downloads, follow progress and import the finished MP4.

Supported sites: **TVP VOD** (`tvp`).

> Proof of concept: minimal features, and jobs are kept in memory only.

## Install and run

### Docker Compose

Follow the [quick start](../README.md#quick-start) to download the
[Compose configuration](../compose.yaml) and create `.env` from
[the example](../.env.example). The image is `ghcr.io/combor/vodarr:latest`
and includes ffmpeg.

The Compose example reads these settings from `.env`:

| Setting | Default | Description |
|---|---|---|
| `VODARR_API_KEY` | required | A long, random key shared by both APIs. |
| `VODARR_DOWNLOAD_PATH` | `./downloads` | Host folder mounted at `/downloads` inside vodarr. |
| `VODARR_UID` | `1000` | User ID for the container process. |
| `VODARR_GID` | `1000` | Group ID for the container process. |

Create the download folder before starting. On Linux, use the user and group IDs
of the account that owns it; `id -u` and `id -g` show your current account's IDs.
The container needs permission to create files and category folders there.
Sonarr/Radarr also need access to the same files. See
[Docker networking and shared downloads](#docker-networking-and-shared-downloads).

Start vodarr with `docker compose up -d`. After changing `.env`, run the same
command to apply the new settings. To update to the latest image:

```sh
docker compose pull
docker compose up -d
```

### Build from source

To build from source, use Go 1.27+ and install `ffmpeg` for runtime use.

```sh
git clone https://github.com/combor/vodarr.git
cd vodarr
go build -o vodarr ./cmd/vodarr
./vodarr -api-key YOUR_API_KEY -download-dir ./downloads
```

Replace `YOUR_API_KEY` with a long, random key and use it for both connections
in Sonarr/Radarr.

## Configuration

Set these environment variables or pass the equivalent command-line flags.
Flags take precedence. For Docker, add any extra variables to the `environment`
section in `compose.yaml`.

| Flag | Env | Default | Description |
|---|---|---|---|
| `-listen` | `VODARR_LISTEN` | `:8484` | listen address |
| `-api-key` | `VODARR_API_KEY` | | required; used by both APIs |
| `-download-dir` | `VODARR_DOWNLOAD_DIR` | | required; finished files go to `<dir>/<category>/<release>/` |
| `-categories` | `VODARR_CATEGORIES` | `tv,movies` | download categories to offer |
| `-ffmpeg` | `VODARR_FFMPEG` | `ffmpeg` | ffmpeg binary |

Sonarr/Radarr must be able to read the download directory. If they see it at a different path (e.g. in containers), add a Remote Path Mapping.

## Sonarr / Radarr setup

1. **Download client:** Settings → Download Clients → **SABnzbd**.
   - Name: e.g. "VOD Downloader".
   - Host and port: vodarr's host and port.
   - API key: the one vodarr was started with.
   - Category: `tv` (Sonarr) or `movies` (Radarr).
2. **Indexer:** Settings → Indexers → **Newznab**, one per site.
   - Name: e.g. "TVP VOD".
   - URL: `http://<host>:8484/tvp`, API path `/api`, the same API key.
   - Categories: 5000, 5040 (Sonarr) or 2000, 2040 (Radarr).
   - **Download Client:** set it to the client from step 1, so vodarr releases never go to a real Usenet client.

The test buttons should pass for both. The indexer test uses a placeholder item that is never grabbed.

## Docker networking and shared downloads

Choose an address that Sonarr/Radarr can reach:

- If an app runs directly on the same host as vodarr, use `localhost` and port `8484`.
- If both containers share a Docker network, use the service name `vodarr` and port `8484`.
- For apps on another machine or a different Docker network, use the Docker
  host's reachable address and the published port `8484`.

Inside a container, `localhost` refers to that container. Separate Compose
projects do not share a network by default.

Mount the same host download folder into Sonarr/Radarr. The supplied Compose
file mounts it at `/downloads` in vodarr, so mounting it at `/downloads` in
your apps gives them matching paths. vodarr creates `tv` and `movies`
subfolders for the default categories.

If an app sees the folder at a different path, add a **Remote Path Mapping**
under **Settings → Download Clients**:

| Field | Value |
|---|---|
| Host | The host entered for the `vodarr` download client. |
| Remote Path | `/downloads` |
| Local Path | The same shared folder as seen by Sonarr/Radarr, for example `/data/downloads`. |

## Troubleshooting

vodarr provides APIs and has no separate web interface. Search for titles and
manage downloads in Sonarr/Radarr.

| Problem | What to check |
|---|---|
| A connection test cannot reach vodarr | Check the container is running, port `8484` is reachable, and the host is correct for your [network setup](#docker-networking-and-shared-downloads). |
| A connection reports an invalid API key | Use the same key for the indexer, download client and `VODARR_API_KEY`. Run `docker compose up -d` after changing `.env`. |
| The container cannot find or write to the download folder | Create `VODARR_DOWNLOAD_PATH` before starting and check that `VODARR_UID` and `VODARR_GID` have write access. |
| Downloads finish but are not imported | Mount the shared folder into Sonarr/Radarr, check file permissions and add a Remote Path Mapping if the paths differ. |
| A vodarr release is sent to another download client | Set the indexer's **Download Client** to `vodarr`. |

To inspect recent container messages:

```sh
docker compose logs --tail 100 vodarr
```

For bugs or feature requests, [open an issue](https://github.com/combor/vodarr/issues).
Include the steps to reproduce and any relevant error message, with API keys removed.

## Limitations

- **Titles:** vodarr searches the site with the title Sonarr/Radarr send.
  - Radarr also searches with a film's original title, so non-English films are found.
  - Sonarr only sends its own series title, which is often English. A series is found only if that title matches the site's, e.g. *Ranczo* but not *Rojst* (Sonarr: "The Mire").
- **What can't be downloaded:** DRM-protected and paid content, and titles not available where vodarr runs. These jobs fail with the site's reason. Getting network access to region-restricted titles is up to the operator.
- **Release details:**
  - Releases are always named `1080p`; ffmpeg downloads the best quality available.
  - Only a single audio track is kept, and no subtitles.
- **Restarts:** the queue and history are lost when vodarr restarts.

## Adding a site

Implement `provider.Provider` (`internal/provider/provider.go`) in a new package under `internal/provider/` and add it to the registry in `cmd/vodarr/main.go`. The provider:

- searches its catalogue and maps Sonarr/Radarr numbering onto its own;
- resolves an ID to a stream URL ffmpeg can open, at download time.

Everything else is shared.

## Development

`make test` runs formatting checks, vet, and race tests. Install ffmpeg with the
libx264 encoder to run the download-engine tests. `make docker-smoke` builds the
container and checks startup and both APIs.

## License

[BSD-3-Clause](../LICENSE).
