// Package probe reads stream quality and language for release names.
package probe

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/combor/magnetowid/internal/hls"
	"github.com/combor/magnetowid/internal/provider"
)

const (
	cacheTTL = 24 * time.Hour
	// Cover repeated season searches without caching temporary region blocks for long.
	unavailableTTL = 10 * time.Minute
	// Cache failures while Sonarr pages through results to keep offsets stable.
	failureTTL = time.Minute
	// Resolve again to try a different CDN edge.
	attempts = 3
	// Leave time to replace stalled items with other results.
	timeout = 20 * time.Second
)

var ErrNotHLS = errors.New("not an HLS master playlist")

var errNoResolution = errors.New("stream gives no resolution")

// Info describes the stream the downloader will fetch.
type Info struct {
	Width, Height int
	Codecs        string // RFC 6381, e.g. "avc1.640029,mp4a.40.2"
	Bandwidth     int64  // bits per second, average if the playlist gives it
	Language      string // RFC 5646, e.g. "pl"; "" if the playlist doesn't say
}

// Match Sonarr/Radarr's resolution thresholds on import.
func (i Info) Resolution() string {
	w, h := i.Width, i.Height
	switch {
	case w >= 3200 || h >= 2100:
		return "2160p"
	case w >= 1800 || h >= 1000:
		return "1080p"
	case w >= 1200 || h >= 700:
		return "720p"
	case w >= 1000 || h >= 560:
		return "576p"
	}
	return "480p"
}

func (i Info) VideoCodec() string {
	for _, c := range strings.Split(i.Codecs, ",") {
		switch c = strings.ToLower(strings.TrimSpace(c)); {
		case hasPrefix(c, "avc1", "avc3"):
			return "H.264"
		case hasPrefix(c, "hvc1", "hev1", "dvh1", "dvhe"):
			return "H.265"
		case hasPrefix(c, "av01"):
			return "AV1"
		case hasPrefix(c, "vp09"):
			return "VP9"
		}
	}
	return ""
}

// Multiple audio codecs are ambiguous: CODECS does not identify the selected rendition.
func (i Info) AudioCodec() string {
	name := ""
	for _, c := range strings.Split(i.Codecs, ",") {
		var n string
		switch c = strings.ToLower(strings.TrimSpace(c)); {
		case hasPrefix(c, "mp4a.40"):
			n = "AAC"
		case hasPrefix(c, "mp4a.69", "mp4a.6b", "mp3"):
			n = "MP3"
		case hasPrefix(c, "ac-3"):
			n = "DD"
		case hasPrefix(c, "ec-3"):
			n = "DDP"
		case hasPrefix(c, "opus"):
			n = "Opus"
		default:
			continue
		}
		if name != "" && n != name {
			return ""
		}
		name = n
	}
	return name
}

// ISO 639-1 names recognized by both Sonarr and Radarr in release titles.
var languageNames = map[string]string{
	"ar": "ARABIC", "bg": "BULGARIAN", "ca": "CATALAN", "da": "DANISH",
	"de": "GERMAN", "el": "GREEK", "en": "ENGLISH", "es": "SPANISH",
	"fi": "FINNISH", "fr": "FRENCH", "he": "HEBREW", "hi": "HINDI",
	"hu": "HUNGARIAN", "is": "ICELANDIC", "it": "ITALIAN", "ja": "JAPANESE",
	"ko": "KOREAN", "lv": "LATVIAN", "ml": "MALAYALAM", "nb": "NORWEGIAN",
	"nl": "DUTCH", "nn": "NORWEGIAN", "no": "NORWEGIAN", "pl": "POLISH",
	"pt": "PORTUGUESE", "ro": "ROMANIAN", "ru": "RUSSIAN", "sk": "SLOVAK",
	"sv": "SWEDISH", "th": "THAI", "tr": "TURKISH", "uk": "UKRAINIAN",
	"vi": "VIETNAMESE", "zh": "CHINESE",
}

// Return an empty name for languages unsupported by Sonarr/Radarr.
func (i Info) LanguageName() string {
	primary, _, _ := strings.Cut(i.Language, "-")
	return languageNames[strings.ToLower(strings.TrimSpace(primary))]
}

func hasPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// Prober caches stream metadata and is safe for concurrent use.
type Prober struct {
	Client  *http.Client
	Timeout time.Duration    // for one item's attempts; 0 is 20 s
	now     func() time.Time // for tests; nil is time.Now

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	info    Info
	err     error
	expires time.Time
}

// Probe wraps provider.ErrUnavailable for undownloadable content. Other errors
// mean the stream's quality could not be read.
func (pr *Prober) Probe(ctx context.Context, p provider.Provider, id string) (Info, error) {
	key := p.Name() + ":" + id
	now := time.Now()
	if pr.now != nil {
		now = pr.now()
	}
	pr.mu.Lock()
	hit, ok := pr.cache[key]
	pr.mu.Unlock()
	if ok && now.Before(hit.expires) {
		return hit.info, hit.err
	}

	info, err := pr.probe(ctx, p, id)
	ttl := cacheTTL
	switch {
	case err == nil:
	case errors.Is(err, provider.ErrUnavailable):
		ttl = unavailableTTL
	case ctx.Err() != nil:
		return info, err // caller cancellation is not a stream failure
	default:
		ttl = failureTTL
	}
	c := pr.store(key, cached{info: info, err: err, expires: now.Add(ttl)}, now)
	return c.info, c.err
}

// A failed probe must not overwrite a concurrent success.
func (pr *Prober) store(key string, c cached, now time.Time) cached {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.cache == nil {
		pr.cache = make(map[string]cached)
	}
	for k, v := range pr.cache {
		if now.After(v.expires) {
			delete(pr.cache, k)
		}
	}
	if cur, ok := pr.cache[key]; ok && cur.err == nil && c.err != nil {
		return cur
	}
	pr.cache[key] = c
	return c
}

// Cache the probe's own timeout, but not caller cancellation.
func (pr *Prober) probe(ctx context.Context, p provider.Provider, id string) (Info, error) {
	client := pr.Client
	if client == nil {
		client = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(pr.Timeout, timeout))
	defer cancel()
	var err error
	for range attempts {
		var s provider.Stream
		if s, err = p.Resolve(ctx, id); err == nil {
			var m hls.Master
			var ok bool
			if m, ok, err = hls.Load(ctx, client, s); err == nil {
				if !ok {
					return Info{}, ErrNotHLS
				}
				return info(m)
			}
		}
		if errors.Is(err, provider.ErrUnavailable) || ctx.Err() != nil {
			return Info{}, err
		}
	}
	return Info{}, err
}

func info(m hls.Master) (Info, error) {
	v := m.Video
	if v.Width == 0 && v.Height == 0 {
		return Info{}, errNoResolution
	}
	bw := v.Average
	if bw == 0 {
		bw = v.Bandwidth
	}
	return Info{Width: v.Width, Height: v.Height, Codecs: v.Codecs, Bandwidth: bw, Language: m.AudioLanguage}, nil
}
