package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestContainerServesAPIs starts the container image with nothing but an API
// key, as an operator would, and checks that it passes its startup checks
// (ffmpeg on PATH, a writable download directory) and answers both APIs. Set
// VODARR_SMOKE_IMAGE to the image under test, or run `make docker-smoke`,
// which builds it first.
func TestContainerServesAPIs(t *testing.T) {
	image := os.Getenv("VODARR_SMOKE_IMAGE")
	if image == "" {
		t.Skip("VODARR_SMOKE_IMAGE is unset; no image to test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	const apiKey = "smoke"
	// No --rm, so a container that exits during startup keeps its logs.
	id := docker(ctx, t, "run", "-d", "-p", "127.0.0.1::8484", "-e", "VODARR_API_KEY="+apiKey, image)
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := exec.Command("docker", "logs", id).CombinedOutput()
			t.Logf("container logs:\n%s", logs)
		}
		_ = exec.Command("docker", "rm", "-f", id).Run()
	})
	addr, _, _ := strings.Cut(docker(ctx, t, "port", id, "8484/tcp"), "\n")
	checkAPIs(ctx, t, "http://"+addr, apiKey, "/downloads") // the image default

	// LookPath at startup only proves ffmpeg exists; this proves it runs.
	if out := docker(ctx, t, "exec", id, "ffmpeg", "-hide_banner", "-version"); !strings.HasPrefix(out, "ffmpeg version") {
		t.Errorf("ffmpeg -version printed %q", out)
	}
}

// checkAPIs waits for vodarr at base to answer, then checks both APIs and that
// finished downloads go to downloadDir.
func checkAPIs(ctx context.Context, t *testing.T, base, apiKey, downloadDir string) {
	t.Helper()
	wait, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	// The server only listens once its startup checks have passed.
	var version struct{ Version string }
	for {
		err := getJSON(wait, base+"/api?mode=version&apikey="+apiKey, &version)
		if err == nil {
			break
		}
		select {
		case <-wait.Done():
			t.Fatalf("SABnzbd API never answered: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
	}
	if version.Version == "" {
		t.Error("mode=version returned no version")
	}

	var status struct {
		Status struct{ CompleteDir string } `json:"status"`
	}
	if err := getJSON(ctx, base+"/api?mode=fullstatus&apikey="+apiKey, &status); err != nil {
		t.Errorf("mode=fullstatus: %v", err)
	} else if status.Status.CompleteDir != downloadDir {
		t.Errorf("download dir = %q, want %q", status.Status.CompleteDir, downloadDir)
	}

	caps, err := get(ctx, base+"/tvp/api?t=caps")
	if err != nil {
		t.Errorf("Newznab caps: %v", err)
	} else if !strings.Contains(string(caps), "<caps>") {
		t.Errorf("Newznab caps returned %q", caps)
	}
}

// docker runs a docker subcommand and returns its trimmed stdout.
func docker(ctx context.Context, t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			t.Fatalf("docker %s: %v\n%s", args[0], err, exitErr.Stderr)
		}
		t.Fatalf("docker %s: %v", args[0], err)
	}
	return strings.TrimSpace(string(out))
}

func get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func getJSON(ctx context.Context, url string, v any) error {
	body, err := get(ctx, url)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}
