# vodarr

Downloads movies and TV shows from **TVP VOD** for **Sonarr and Radarr**, using Newznab and SABnzbd-compatible APIs.

Requires Go 1.27+ and ffmpeg.

```sh
go build -o vodarr ./cmd/vodarr
./vodarr -api-key YOUR_API_KEY -download-dir /path/to/downloads
```

Proof of concept: the queue and history are lost on restart.

[Setup and configuration](docs/usage.md) · [BSD-3-Clause](LICENSE)
