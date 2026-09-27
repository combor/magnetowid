package downloader

import (
	"context"
	"net/http"
	"time"

	"github.com/combor/vodarr/internal/hls"
	"github.com/combor/vodarr/internal/provider"
)

var defaultClient = &http.Client{Timeout: 30 * time.Second}

// pickInputs returns the URLs ffmpeg should read. For an HLS master playlist
// it picks the best video variant and its audio rendition, since ffmpeg would
// otherwise probe every variant first. Other URLs are passed through.
func pickInputs(ctx context.Context, client *http.Client, s provider.Stream) ([]string, error) {
	m, ok, err := hls.Load(ctx, client, s)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []string{s.URL}, nil
	}
	inputs := []string{m.Video.URI}
	if m.Audio != "" {
		inputs = append(inputs, m.Audio)
	}
	return inputs, nil
}
