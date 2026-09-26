package downloader

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/combor/vodarr/internal/provider"
)

// FFmpeg is an Engine that lets ffmpeg fetch the stream (HLS, DASH, ...) and
// remux it losslessly into MP4. For HLS master playlists vodarr picks the
// best video variant and its audio rendition itself (see pickInputs);
// otherwise ffmpeg's default stream selection picks the highest-resolution
// video and the best audio track.
type FFmpeg struct {
	Path   string       // binary; "ffmpeg" if empty
	Client *http.Client // fetches HLS master playlists; a 30s-timeout client if nil
	// StallTimeout kills ffmpeg when no media arrives for this long (a CDN
	// can accept a connection and then go silent). Long enough to cover the
	// final +faststart pass, which rewrites the file without progress.
	// 5 minutes if zero.
	StallTimeout time.Duration
}

// ffmpeg logs these (as warnings or errors) when it drops data yet carries
// on and exits 0, so a download that logs one is incomplete. A segment the
// server truncates but serves with a matching Content-Length can still slip
// through silently; HLS has no checksums to catch that.
var dataLossMessages = []string{
	"Failed to open segment",
	"failed too many times, skipping",
	"Stream ends prematurely",
	"Failed to reload playlist",
	"partial file",
	"Packet corrupt",          // e.g. MPEG-TS segment missing transport packets
	"corrupt input packet in", // ffmpeg's own report of the same
}

func (f *FFmpeg) Download(parent context.Context, s provider.Stream, out string, progress func(time.Duration, int64)) error {
	bin := f.Path
	if bin == "" {
		bin = "ffmpeg"
	}
	client := f.Client
	if client == nil {
		client = defaultClient
	}
	stall := f.StallTimeout
	if stall == 0 {
		stall = 5 * time.Minute
	}
	inputs, err := pickInputs(parent, client, s)
	if err != nil {
		return err
	}
	// Cancelling with a cause kills ffmpeg and records why (stall, data loss).
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)

	args := []string{"-nostdin", "-hide_banner", "-loglevel", "warning", "-y"}
	for _, in := range inputs {
		args = append(args, headerArgs(s.Header)...) // input options apply per input
		args = append(args, "-i", in)
	}
	if len(inputs) == 2 {
		args = append(args, "-map", "0:v:0", "-map", "1:a:0")
	}
	args = append(args,
		"-sn", "-dn", // subtitle/data streams can't always be copied into MP4
		"-c", "copy",
		"-movflags", "+faststart",
		"-progress", "pipe:1", "-nostats",
		out,
	)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = 5 * time.Second
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
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			line := sc.Text()
			tail.Write([]byte(line + "\n"))
			for _, m := range dataLossMessages {
				if strings.Contains(line, m) {
					cancel(fmt.Errorf("incomplete download: %s", strings.TrimSpace(line)))
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
		return fmt.Errorf("ffmpeg: %w: %s", waitErr, strings.TrimSpace(tail.String()))
	}
	return nil
}

// headerArgs turns extra request headers into ffmpeg input options.
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

// readProgress parses ffmpeg's "-progress" key=value blocks, reporting once
// per block.
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

// tailBuffer keeps the last max bytes written to it.
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
