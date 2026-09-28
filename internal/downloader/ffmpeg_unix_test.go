//go:build unix

package downloader

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/combor/magnetowid/internal/provider"
)

// Terminal Ctrl+C reaches the whole foreground process group.
func TestFFmpegRunsInOwnProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	bin := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\necho $$ > '" + pidFile + "'\nexec sleep 60\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (&FFmpeg{Path: bin}).Download(ctx,
			provider.Stream{URL: "http://example.invalid/video.mp4"}, filepath.Join(dir, "out.mp4"), func(time.Duration, int64) {})
	}()
	defer func() {
		cancel()
		<-done
	}()

	var pid int
	for deadline := time.Now().Add(10 * time.Second); pid == 0; {
		if time.Now().After(deadline) {
			t.Fatal("fake ffmpeg never started")
		}
		b, _ := os.ReadFile(pidFile)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		time.Sleep(10 * time.Millisecond)
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatal(err)
	}
	if pgid != pid {
		t.Errorf("ffmpeg (pid %d) is in process group %d, not its own", pid, pgid)
	}
}
