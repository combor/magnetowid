# vodarr

[![CI](https://github.com/combor/vodarr/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/combor/vodarr/actions/workflows/ci.yml)

Downloads movies and TV shows from **TVP VOD** for **Sonarr and Radarr**, using Newznab and SABnzbd-compatible APIs.

Build from source with Go 1.27+; ffmpeg is required at runtime.

```sh
go build -o vodarr ./cmd/vodarr
./vodarr -api-key YOUR_API_KEY -download-dir /path/to/downloads
```

Add vodarr as both a **Newznab indexer** and a **SABnzbd download client** using the [Sonarr/Radarr setup guide](docs/usage.md#sonarr--radarr-setup). Both apps must be able to read the download directory.

Proof of concept: the queue and history are lost on restart. See [limitations](docs/usage.md#limitations) for content and quality restrictions.

[Setup and configuration](docs/usage.md) · [Bugs and feature requests](https://github.com/combor/vodarr/issues) · [BSD-3-Clause](LICENSE)
