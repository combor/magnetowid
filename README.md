# vodarr

Downloads movies and series from video-on-demand sites for Sonarr and Radarr.
vodarr needs no changes to either app: it looks like two things they already support.

- **A Newznab indexer per site** at `/{provider}/api`, so they can search the site's catalogue.
- **A SABnzbd download client** at `/api`, so they can send downloads, follow progress and import the finished MP4.

Supported sites: **TVP VOD** (`tvp`).

> Proof of concept: minimal features, and jobs are kept in memory only.

## Run

Requires Go 1.27+ to build and `ffmpeg` at runtime.

```sh
go build -o vodarr ./cmd/vodarr
./vodarr -api-key <key> -download-dir /path/to/downloads
```

| Flag | Env | Default | |
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
