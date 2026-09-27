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

// FFmpeg is an Engine that has ffmpeg fetch the stream and remux it into MP4.
type FFmpeg struct {
	Path   string       // binary; "ffmpeg" if empty
	Client *http.Client // for HLS master playlists; defaultClient if nil
	// StallTimeout kills ffmpeg when no data arrives for this long; 5 minutes
	// if zero, to cover the +faststart pass, which reports no progress.
	StallTimeout time.Duration
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
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)

	args := []string{"-nostdin", "-hide_banner", "-loglevel", "warning", "-y"}
	for _, in := range inputs {
		args = append(args, headerArgs(s.Header)...)
		args = append(args, "-i", in)
	}
	if len(inputs) == 2 {
		args = append(args, "-map", "0:v:0", "-map", "1:a:0")
	}
	args = append(args,
		"-sn", "-dn", // not all subtitle/data streams fit in MP4
		"-c", "copy",
		"-movflags", "+faststart",
		"-progress", "pipe:1", "-nostats",
		out,
	)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = 5 * time.Second
	// A terminal's Ctrl+C must reach only vodarr, which stops ffmpeg and
	// requeues the job. An ffmpeg that exited first would use up a retry.
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

// readProgress parses ffmpeg's -progress output.
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
