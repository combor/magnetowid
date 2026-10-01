package downloader

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/combor/magnetowid/internal/hls"
	"github.com/combor/magnetowid/internal/provider"
	"golang.org/x/text/language"
)

var defaultClient = &http.Client{Timeout: 30 * time.Second}

const playlistTimeout = 30 * time.Second

// FFmpeg downloads HLS segments itself, keeping them across attempts, and
// remuxes them into MP4 with ffmpeg. Other streams go straight to ffmpeg.
// Neither transcodes.
type FFmpeg struct {
	Path string // binary; "ffmpeg" if empty
	// Fetches playlists and segments, on the stream's Transport if it has one;
	// nil uses a default. Each download gets its own copy, with a cookie jar
	// if the client has none.
	Client *http.Client
	Log    *slog.Logger // nil discards
	// Zero defaults to 5 minutes, allowing for +faststart's silent final pass.
	StallTimeout time.Duration

	segmentTimeout time.Duration               // zero defaults to a minute
	retryDelay     func(try int) time.Duration // nil doubles from a second
}

// ffmpeg logs these when it drops data but still exits 0.
var dataLossMessages = []string{
	"Failed to open segment",
	"failed too many times, skipping",
	"Stream ends prematurely",
	"Failed to reload playlist",
	"partial file",
	"Packet corrupt",
	"corrupt input packet in",
}

var (
	errDataLoss   = errors.New("incomplete download")
	errUnreadable = errors.New("unreadable input")
)

// ffmpeg logs these for inputs it can't read at all; 5.x only the second.
var unreadableMessages = []string{
	"Error opening input",
	"Invalid data found when processing input",
}

// Segment requests share connections, one per worker.
var segmentTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = segmentWorkers
	return t
}()

// Download keeps segments in out's folder until a download succeeds or the
// stream changes.
func (f *FFmpeg) Download(ctx context.Context, s provider.Stream, out string, progress func(time.Duration, int64)) error {
	client := &http.Client{Transport: segmentTransport}
	if f.Client != nil {
		c := *f.Client
		client = &c
	}
	client = s.Client(client)
	if client.Jar == nil {
		// ffmpeg sends a playlist's cookies with its segments too.
		client.Jar, _ = cookiejar.New(nil)
	}
	// Fail before fetching a stream that can't be remuxed.
	if _, err := f.binary(); err != nil {
		return err
	}
	// Select HLS inputs before invoking ffmpeg, which otherwise probes every variant.
	loadCtx, cancel := context.WithTimeout(ctx, playlistTimeout)
	m, master, err := hls.Load(loadCtx, client, s)
	cancel()
	if err != nil {
		return err
	}
	dir := filepath.Join(filepath.Dir(out), segmentsDir)
	if master {
		err := f.downloadSegments(ctx, client, s.Header, m, dir, out, progress)
		if !errors.Is(err, hls.ErrUnsupported) {
			return err
		}
		f.log().Info("can't fetch the stream's segments; ffmpeg downloads it from the start",
			"file", filepath.Base(out), "err", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	env, err := proxyEnv(s)
	if err != nil {
		return err
	}
	inputs := directInputs(s, m, master)
	var args []string
	for _, in := range inputs {
		args = append(args, headerArgs(s.Header)...)
		args = append(args, "-i", in)
	}
	return f.run(ctx, "", env, append(args, outputArgs(len(inputs), m.AudioLanguage, out)...), progress)
}

// proxyEnv returns the environment in which ffmpeg reaches the stream as its
// Transport does, or nil, magnetowid's own, for a stream without one. ffmpeg
// reads http_proxy for HTTP and HTTPS alike.
func proxyEnv(s provider.Stream) ([]string, error) {
	t, ok := s.Transport.(*http.Transport)
	if !ok {
		return nil, nil
	}
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return strings.EqualFold(k, "http_proxy") || strings.EqualFold(k, "no_proxy")
	})
	if t.Proxy == nil {
		return env, nil
	}
	req, err := http.NewRequest(http.MethodGet, s.URL, nil)
	if err != nil {
		return nil, err
	}
	// A proxy that can't be determined must not become a direct connection.
	proxy, err := t.Proxy(req)
	if err != nil {
		return nil, fmt.Errorf("finding the stream's proxy: %w", err)
	}
	if proxy != nil {
		env = append(env, "http_proxy="+proxy.String())
	}
	return env, nil
}

func (f *FFmpeg) log() *slog.Logger {
	if f.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return f.Log
}

func directInputs(s provider.Stream, m hls.Master, master bool) []string {
	if !master {
		return []string{s.URL}
	}
	if m.Audio != "" {
		return []string{m.Video.URI, m.Audio}
	}
	return []string{m.Video.URI}
}

// outputArgs follow inputs, whose first video and, if there are two, second's
// audio go into out.
func outputArgs(inputs int, audioLanguage, out string) []string {
	var args []string
	if inputs == 2 {
		args = append(args, "-map", "0:v:0", "-map", "1:a:0")
	}
	// Opening rendition URLs bypasses the master's language tag. MP4 needs a
	// three-letter code; leave existing metadata alone when the tag is unknown.
	if tag, err := language.Parse(strings.TrimSpace(audioLanguage)); err == nil {
		base, _, _ := tag.Raw()
		if code := base.ISO3(); code != "und" {
			args = append(args, "-metadata:s:a:0", "language="+code)
		}
	}
	return append(args,
		"-sn", "-dn", // not all subtitle/data streams fit in MP4
		"-c", "copy",
		"-movflags", "+faststart",
		"-progress", "pipe:1", "-nostats",
		out,
	)
}

// binary returns ffmpeg's absolute path: a relative one would resolve against
// the folder ffmpeg runs in.
func (f *FFmpeg) binary() (string, error) {
	bin := f.Path
	if bin == "" {
		bin = "ffmpeg"
	}
	bin, err := exec.LookPath(bin)
	if err == nil {
		bin, err = filepath.Abs(bin)
	}
	if err != nil {
		return "", fmt.Errorf("ffmpeg: %w", err)
	}
	return bin, nil
}

// run runs ffmpeg in dir, or the current folder if dir is empty, with env, or
// magnetowid's environment if env is nil.
func (f *FFmpeg) run(parent context.Context, dir string, env, args []string, progress func(time.Duration, int64)) error {
	bin, err := f.binary()
	if err != nil {
		return err
	}
	stall := f.StallTimeout
	if stall == 0 {
		stall = 5 * time.Minute
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)

	args = append([]string{"-nostdin", "-hide_banner", "-loglevel", "warning", "-y"}, args...)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.WaitDelay = 5 * time.Second
	// Isolate ffmpeg from terminal signals so magnetowid can requeue interrupted jobs.
	cmd.SysProcAttr = ownProcessGroup()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}

	tail := &tailBuffer{max: 4096}
	var unreadable atomic.Bool
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			line := sc.Text()
			tail.Write([]byte(line + "\n"))
			for _, m := range unreadableMessages {
				if strings.Contains(line, m) {
					unreadable.Store(true)
				}
			}
			for _, m := range dataLossMessages {
				if strings.Contains(line, m) {
					cancel(fmt.Errorf("%w: %s", errDataLoss, strings.TrimSpace(line)))
				}
			}
		}
		io.Copy(io.Discard, stderr)
	}()

	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		tick := time.NewTicker(min(stall/4, 10*time.Second))
		defer tick.Stop()
		for {
			select {
			case <-watchDone:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
				if time.Since(time.Unix(0, lastActivity.Load())) > stall {
					cancel(fmt.Errorf("stalled: no data for %v", stall))
					return
				}
			}
		}
	}()

	var lastDone time.Duration
	var lastBytes int64
	readProgress(stdout, func(done time.Duration, bytes int64) {
		if done > lastDone || bytes > lastBytes {
			lastDone, lastBytes = done, bytes
			lastActivity.Store(time.Now().UnixNano())
		}
		progress(done, bytes)
	})
	<-stderrDone
	waitErr := cmd.Wait()

	if parent.Err() != nil {
		return parent.Err()
	}
	if cause := context.Cause(ctx); cause != nil {
		return fmt.Errorf("ffmpeg: %w", cause)
	}
	if waitErr != nil {
		if unreadable.Load() {
			waitErr = fmt.Errorf("%w: %w", errUnreadable, waitErr)
		}
		return fmt.Errorf("ffmpeg: %w: %s", waitErr, strings.TrimSpace(tail.String()))
	}
	return nil
}

func headerArgs(h http.Header) []string {
	var args []string
	var lines strings.Builder
	for k, vs := range h {
		for _, v := range vs {
			if http.CanonicalHeaderKey(k) == "User-Agent" {
				args = append(args, "-user_agent", v)
				continue
			}
			lines.WriteString(k + ": " + v + "\r\n")
		}
	}
	if lines.Len() > 0 {
		args = append(args, "-headers", lines.String())
	}
	return args
}

func readProgress(r io.Reader, progress func(time.Duration, int64)) {
	var done time.Duration
	var size int64
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch key {
		case "out_time_us":
			if us, err := strconv.ParseInt(value, 10, 64); err == nil && us > 0 {
				done = time.Duration(us) * time.Microsecond
			}
		case "total_size":
			if n, err := strconv.ParseInt(value, 10, 64); err == nil {
				size = n
			}
		case "progress":
			progress(done, size)
		}
	}
	io.Copy(io.Discard, r)
}

type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf bytes.Buffer
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if over := t.buf.Len() - t.max; over > 0 {
		t.buf.Next(over)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}
