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

// Set MAGNETOWID_SMOKE_IMAGE, or run make docker-smoke to build it first.
func TestContainerServesAPIs(t *testing.T) {
	image := os.Getenv("MAGNETOWID_SMOKE_IMAGE")
	if image == "" {
		t.Skip("MAGNETOWID_SMOKE_IMAGE is unset; no image to test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	const apiKey = "smoke"
	// No --rm, so a container that exits during startup keeps its logs.
	id := docker(ctx, t, "run", "-d", "-p", "127.0.0.1::8484", "-e", "MAGNETOWID_API_KEY="+apiKey, image)
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := exec.Command("docker", "logs", id).CombinedOutput()
			t.Logf("container logs:\n%s", logs)
		}
		_ = exec.Command("docker", "rm", "-f", id).Run()
	})
	addr, _, _ := strings.Cut(docker(ctx, t, "port", id, "8484/tcp"), "\n")
	checkAPIs(ctx, t, "http://"+addr, apiKey, "/downloads") // the image default

	for {
		status := docker(ctx, t, "inspect", "-f", "{{.State.Health.Status}}", id)
		if status == "healthy" {
			break
		}
		if status == "unhealthy" || ctx.Err() != nil {
			t.Fatalf("health status %q: %s", status, docker(ctx, t, "inspect", "-f", "{{json .State.Health.Log}}", id))
		}
		time.Sleep(time.Second)
	}

	// LookPath proves ffmpeg exists; executing it also checks runtime dependencies.
	if out := docker(ctx, t, "exec", id, "ffmpeg", "-hide_banner", "-version"); !strings.HasPrefix(out, "ffmpeg version") {
		t.Errorf("ffmpeg -version printed %q", out)
	}
}

func checkAPIs(ctx context.Context, t *testing.T, base, apiKey, downloadDir string) {
	t.Helper()
	wait, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

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

	for _, p := range []string{"tvp", "bbc"} {
		caps, err := get(ctx, base+"/"+p+"/api?t=caps")
		if err != nil {
			t.Errorf("%s Newznab caps: %v", p, err)
		} else if !strings.Contains(string(caps), "<caps>") {
			t.Errorf("%s Newznab caps returned %q", p, caps)
		}
	}

	if health, err := get(ctx, base+"/health"); err != nil || string(health) != "OK\n" {
		t.Errorf("health: %q, %v", health, err)
	}

	// The web interface's pages and files are embedded in the binary.
	if page, err := get(ctx, base+"/ui/login"); err != nil || !strings.Contains(string(page), `name="apikey"`) {
		t.Errorf("sign-in page: %q, %v", page, err)
	}
	if js, err := get(ctx, base+"/ui/static/htmx-4.0.0.min.js"); err != nil || !strings.Contains(string(js), "htmx") {
		t.Errorf("htmx script: %v", err)
	}
}

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
