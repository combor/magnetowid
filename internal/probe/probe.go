// Package probe reads the quality of a provider's stream, so releases are
// named with the quality magnetowid will download.
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
	// cacheTTL: a stream's quality doesn't change.
	cacheTTL = 24 * time.Hour
	// unavailableTTL covers a Sonarr season search, whose per-episode
	// fallback asks for the same items again, without remembering a region
	// block from a VPN outage for long.
	unavailableTTL = 10 * time.Minute
	// failureTTL keeps a search's results in the same order while Sonarr
	// pages through them, so an item that fails and then recovers doesn't
	// shift the pages. The next search tries again.
	failureTTL = time.Minute
	// attempts: TVP's CDN refuses some edges; a fresh Resolve gets another.
	attempts = 3
	// timeout bounds an item's attempts, so a search has time to probe
	// another item in place of a stalled one.
	timeout = 20 * time.Second
)

// ErrNotHLS means the stream isn't an HLS master playlist, so its quality
// is unknown.
var ErrNotHLS = errors.New("not an HLS master playlist")

// errNoResolution means the best variant doesn't give its resolution.
var errNoResolution = errors.New("stream gives no resolution")

// Info is the quality of the variant the downloader will fetch, and the
// language of its audio.
type Info struct {
	Width, Height int
	Codecs        string // RFC 6381, e.g. "avc1.640029,mp4a.40.2"
	Bandwidth     int64  // bits per second, average if the playlist gives it
	Language      string // RFC 5646, e.g. "pl"; "" if the playlist doesn't say
}

// Resolution names the resolution the way Sonarr and Radarr classify a file
// on import, so the quality they read from the release name is the one they
// record afterwards.
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

// VideoCodec names the video codec as release names do, or "" if unknown.
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

// AudioCodec names the audio codec as release names do. It is "" if unknown,
// or if CODECS names several: they belong to the audio renditions the
// variant can use, and CODECS doesn't say which the downloader picks.
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

// languageNames are the languages Sonarr and Radarr both read from a word
// in a release name, by ISO 639-1 code.
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

// LanguageName names the audio's language as release names do, or "" if
// it is unknown or Sonarr and Radarr couldn't read the name.
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

// Prober reads the quality of provider items and caches it. It is safe for
// concurrent use.
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

// Probe returns the quality of the item's stream. It fails with an error
// wrapping provider.ErrUnavailable if the item can't be downloaded, and with
// any other error if its quality can't be read.
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
		return info, err // the caller gave up; nothing is known about the stream
	default:
		ttl = failureTTL
	}
	c := pr.store(key, cached{info: info, err: err, expires: now.Add(ttl)}, now)
	return c.info, c.err
}

// store caches c under key and returns what is cached. A failure doesn't
// replace a success, which a concurrent probe of the same item may have
// stored meanwhile.
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

// probe reads the item's quality within pr.Timeout. Running out of it is the
// stream's failure, which Probe caches; the caller's ctx ending isn't.
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
