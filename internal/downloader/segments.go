package downloader

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/combor/magnetowid/internal/hls"
)

const (
	segmentsDir    = "segments"
	segmentWorkers = 4
	segmentTries   = 5
	// Bump to discard segments saved in an older layout.
	segmentsLayout = 1
)

var errStalled = errors.New("stalled")

// track is a rendition saved in its own folder, listed in a local playlist of
// the same name.
type track struct {
	name, kind string // "v" and "video", or "a" and "audio"
	uri        string // media playlist
	source     string // identifies the rendition
	segments   []hls.Segment
	inits      []string // distinct initialization sections
}

// part is a file to fetch.
type part struct {
	uri   string
	path  string        // slash-separated, relative to the segments folder
	start time.Duration // playback order
	video time.Duration // playback time a video segment adds
	label string        // e.g. "video segment 12 of 900"
}

// downloadSegments saves the stream's segments in dir, keeping those an
// earlier attempt saved, and remuxes them into out. It returns
// hls.ErrUnsupported before touching dir.
func (f *FFmpeg) downloadSegments(ctx context.Context, client *http.Client, h http.Header, m hls.Master,
	dir, out string, progress func(time.Duration, int64)) error {
	tracks, err := loadTracks(ctx, client, h, m)
	if err != nil {
		return err
	}
	stale, err := prepare(dir, tracks)
	if err != nil {
		return err
	}
	if stale {
		f.log().Info("saved segments are of another stream; starting over", "file", filepath.Base(out))
	}
	parts := layout(tracks)
	pending, done, bytes := scan(dir, parts)
	progress(done, bytes)
	if saved := len(parts) - len(pending); saved > 0 {
		f.log().Info("resuming download", "file", filepath.Base(out), "saved", saved, "files", len(parts))
	}
	var mu sync.Mutex
	err = f.fetchAll(ctx, client, h, dir, pending, func(p part, n int64) {
		mu.Lock()
		defer mu.Unlock()
		done += p.video
		bytes += n
		progress(done, bytes)
	})
	if err != nil {
		return err
	}

	out, err = filepath.Abs(out)
	if err != nil {
		return err
	}
	args := []string{"-i", tracks[0].name + ".m3u8"}
	if len(tracks) == 2 {
		args = append(args, "-i", tracks[1].name+".m3u8")
	}
	// The inputs are local files, so any environment will do.
	err = f.run(ctx, dir, nil, append(args, outputArgs(len(tracks), m.AudioLanguage, out)...), func(time.Duration, int64) {})
	if errors.Is(err, errDataLoss) || errors.Is(err, errUnreadable) {
		// A damaged file on disk would fail every retry.
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			f.log().Warn("removing damaged segments", "file", filepath.Base(out), "err", rmErr)
		}
	}
	if err != nil {
		return err
	}
	fi, err := os.Stat(out)
	if err != nil {
		return err
	}
	progress(done, fi.Size())
	return nil
}

func loadTracks(ctx context.Context, client *http.Client, h http.Header, m hls.Master) ([]track, error) {
	ctx, cancel := context.WithTimeout(ctx, playlistTimeout)
	defer cancel()
	tracks := []track{{name: "v", kind: "video", uri: m.Video.URI, source: fmt.Sprintf("%dx%d %d %s %s",
		m.Video.Width, m.Video.Height, m.Video.Bandwidth, m.Video.Codecs, baseName(m.Video.URI))}}
	if m.Audio != "" {
		tracks = append(tracks, track{name: "a", kind: "audio", uri: m.Audio,
			source: fmt.Sprintf("%s %s", m.AudioLanguage, baseName(m.Audio))})
	}
	for i := range tracks {
		t := &tracks[i]
		segs, err := hls.LoadMedia(ctx, client, t.uri, h)
		if err != nil {
			return nil, err
		}
		t.segments = segs
		// Files are saved by position, so their names must identify the stream too.
		names := sha256.New()
		for _, s := range segs {
			if s.Init != "" && !slices.Contains(t.inits, s.Init) {
				t.inits = append(t.inits, s.Init)
				fmt.Fprintln(names, baseName(s.Init))
			}
			fmt.Fprintln(names, baseName(s.URI))
		}
		t.source += fmt.Sprintf(" files %x", names.Sum(nil)[:8])
	}
	return tracks, nil
}

// baseName leaves out the query, where tokens change with every resolve.
func baseName(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	return path.Base(u.Path)
}

// prepare keeps saved files only if the tracks' local playlists are unchanged;
// stale reports discarded ones.
func prepare(dir string, tracks []track) (stale bool, err error) {
	want := map[string]string{"v": "", "a": ""}
	for _, t := range tracks {
		want[t.name] = t.playlist()
	}
	same := true
	for name, w := range want {
		got, err := os.ReadFile(filepath.Join(dir, name+".m3u8"))
		stale = stale || err == nil
		same = same && string(got) == w
	}
	if same {
		return false, nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return false, err
	}
	for _, t := range tracks {
		if err := os.MkdirAll(filepath.Join(dir, t.name), 0o777); err != nil {
			return false, err
		}
	}
	for _, t := range tracks {
		if err := os.WriteFile(filepath.Join(dir, t.name+".m3u8"), []byte(t.playlist()), 0o666); err != nil {
			return false, err
		}
	}
	return stale, nil
}

// playlist lists the track's local files for ffmpeg. Its first lines identify
// the rendition, so the playlist changes with the stream.
func (t track) playlist() string {
	var target float64
	for _, s := range t.segments {
		target = max(target, math.Ceil(s.Duration.Seconds()))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n## magnetowid layout %d: %s\n#EXT-X-TARGETDURATION:%d\n#EXT-X-PLAYLIST-TYPE:VOD\n",
		segmentsLayout, t.source, int(target))
	init := ""
	for i, s := range t.segments {
		if s.Discontinuity {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		if s.Init != init {
			init = s.Init
			fmt.Fprintf(&b, "#EXT-X-MAP:URI=%q\n", t.initPath(init))
		}
		fmt.Fprintf(&b, "#EXTINF:%.6f,\n%s\n", s.Duration.Seconds(), t.segmentPath(i))
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

func (t track) initPath(uri string) string {
	return fmt.Sprintf("%s/init%d.mp4", t.name, slices.Index(t.inits, uri))
}

func (t track) segmentPath(i int) string {
	return fmt.Sprintf("%s/%06d%s", t.name, i, segmentExt(t.segments[i]))
}

// Use extensions that ffmpeg accepts for local segments of each format.
func segmentExt(s hls.Segment) string {
	if s.Init != "" {
		return ".mp4"
	}
	switch ext := strings.ToLower(path.Ext(baseName(s.URI))); ext {
	case ".aac", ".ac3", ".eac3", ".mp3":
		return ext
	case ".ec3":
		return ".eac3"
	case ".m4s", ".m4a", ".m4v", ".mp4", ".cmfa", ".cmfv", ".fmp4":
		return ".mp4"
	}
	return ".ts"
}

// layout lists initialization sections, then segments in playback order so
// that video and audio progress together.
func layout(tracks []track) []part {
	var inits, segs []part
	for _, t := range tracks {
		for i, uri := range t.inits {
			inits = append(inits, part{uri: uri, path: t.initPath(uri),
				label: fmt.Sprintf("%s initialization section %d", t.kind, i+1)})
		}
		var start time.Duration
		for i, s := range t.segments {
			p := part{uri: s.URI, path: t.segmentPath(i), start: start,
				label: fmt.Sprintf("%s segment %d of %d", t.kind, i+1, len(t.segments))}
			if t.name == "v" {
				p.video = s.Duration
			}
			segs = append(segs, p)
			start += s.Duration
		}
	}
	slices.SortStableFunc(segs, func(a, b part) int { return cmp.Compare(a.start, b.start) })
	return append(inits, segs...)
}

// scan returns the parts still to fetch, and the video time and bytes saved.
func scan(dir string, parts []part) (pending []part, done time.Duration, bytes int64) {
	for _, p := range parts {
		// Files appear only when complete, and never empty.
		fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p.path)))
		if err == nil && fi.Size() > 0 {
			done += p.video
			bytes += fi.Size()
			continue
		}
		pending = append(pending, p)
	}
	return pending, done, bytes
}

// fetchAll stops at the first error. It returns only after every worker has.
func (f *FFmpeg) fetchAll(parent context.Context, client *http.Client, h http.Header, dir string,
	parts []part, saved func(part, int64)) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	todo := make(chan part)
	var wg sync.WaitGroup
	for range segmentWorkers {
		wg.Go(func() {
			for p := range todo {
				n, err := f.fetch(ctx, client, h, dir, p)
				if err != nil {
					cancel(err)
					return
				}
				saved(p, n)
			}
		})
	}
feed:
	for _, p := range parts {
		select {
		case todo <- p:
		case <-ctx.Done():
			break feed
		}
	}
	close(todo)
	wg.Wait()
	if err := parent.Err(); err != nil {
		return err
	}
	return context.Cause(ctx)
}

// fetch tries again after transient errors.
func (f *FFmpeg) fetch(ctx context.Context, client *http.Client, h http.Header, dir string, p part) (int64, error) {
	for try := 1; ; try++ {
		n, err := f.fetchOnce(ctx, client, h, dir, p)
		if err == nil {
			return n, nil
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		err = fmt.Errorf("%s: %w", p.label, err)
		if try == segmentTries || !transient(err) {
			return 0, err
		}
		delay := time.Second << (try - 1)
		if f.retryDelay != nil {
			delay = f.retryDelay(try)
		}
		f.log().Debug("fetching again", "try", try, "in", delay, "err", err)
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(delay):
		}
	}
}

func (f *FFmpeg) fetchOnce(ctx context.Context, client *http.Client, h http.Header, dir string, p part) (n int64, err error) {
	timeout := cmp.Or(f.segmentTimeout, time.Minute)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	idle := time.AfterFunc(timeout, func() { cancel(fmt.Errorf("%w: no data for %v", errStalled, timeout)) })
	defer idle.Stop()
	defer func() {
		if cause := context.Cause(ctx); err != nil && errors.Is(cause, errStalled) {
			err = cause
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.uri, nil)
	if err != nil {
		return 0, permanent{err}
	}
	for k, vs := range h {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		// Leave tokenized URLs out of job errors.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, &statusError{resp.StatusCode}
	}
	// ffmpeg would skip an error page saved as a segment without a word.
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt == "text/html" {
		return 0, errors.New("a web page instead of media")
	}

	dst := filepath.Join(dir, filepath.FromSlash(p.path))
	tmp := dst + ".part"
	file, err := os.Create(tmp)
	if err != nil {
		return 0, permanent{err}
	}
	n, err = io.Copy(diskWriter{file}, idleReader{resp.Body, idle, timeout})
	if err == nil && n == 0 {
		err = errors.New("empty response")
	}
	if err == nil {
		err = diskError(file.Sync())
	}
	if closeErr := file.Close(); err == nil {
		err = diskError(closeErr)
	}
	if err == nil {
		err = diskError(os.Rename(tmp, dst))
	}
	if err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return n, nil
}

// idleReader restarts the stall timer whenever data arrives.
type idleReader struct {
	r     io.Reader
	timer *time.Timer
	d     time.Duration
}

func (r idleReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.timer.Reset(r.d)
	}
	return n, err
}

// diskWriter tells write errors from read errors in io.Copy.
type diskWriter struct{ w io.Writer }

func (d diskWriter) Write(p []byte) (int, error) {
	n, err := d.w.Write(p)
	return n, diskError(err)
}

func diskError(err error) error {
	if err != nil {
		return permanent{err}
	}
	return nil
}

// permanent errors recur when fetching again.
type permanent struct{ error }

func (p permanent) Unwrap() error { return p.error }

type statusError struct{ code int }

func (e *statusError) Error() string { return fmt.Sprintf("HTTP %d", e.code) }

// transient errors are server errors, rate limits, and lost or stalled connections.
func transient(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.code >= 500 || se.code == http.StatusRequestTimeout || se.code == http.StatusTooManyRequests
	}
	var p permanent
	return !errors.As(err, &p)
}
