package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// distro is a Linux distribution whose vodarr package TestPackageService
// installs.
type distro struct {
	name string
	// dockerfile builds an image that can boot systemd.
	dockerfile string
	// Shell commands run in the container, which has the snapshot in /dist,
	// packaging/linux in /packaging and the Go architecture in $GOARCH.
	install, upgrade, remove string
	// restarts is whether an upgrade restarts a running service. Arch leaves
	// that to the operator.
	restarts bool
	// x264 is whether the distro's ffmpeg can encode the download engine tests'
	// streams. Fedora's ffmpeg-free can't, so those tests skip there.
	x264 bool
}

var distros = []distro{
	{
		name: "debian",
		dockerfile: `FROM debian:trixie-slim
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends systemd`,
		install:  "apt-get install -y /dist/vodarr_*_linux_$GOARCH.deb",
		upgrade:  "apt-get install -y --reinstall /dist/vodarr_*_linux_$GOARCH.deb",
		remove:   "apt-get remove -y vodarr",
		restarts: true,
		x264:     true,
	},
	{
		name: "fedora",
		dockerfile: `FROM fedora:44
RUN dnf install -y systemd`,
		install:  "dnf install -y /dist/vodarr_*_linux_$GOARCH.rpm",
		upgrade:  "dnf reinstall -y /dist/vodarr_*_linux_$GOARCH.rpm",
		remove:   "dnf remove -y vodarr",
		restarts: true,
	},
	{
		// Builds the package from the PKGBUILD that GoReleaser pushes to the AUR,
		// which checks its checksums against the archive.
		name: "arch",
		dockerfile: `FROM archlinux:latest
RUN pacman -Syu --noconfirm --needed binutils debugedit fakeroot && useradd -m builder`,
		install: `set -e
install -d -o builder /build
cd /build
cp /dist/aur/vodarr-bin.pkgbuild PKGBUILD
cp /packaging/vodarr.install .
. ./PKGBUILD
cp /dist/vodarr_*_linux_$GOARCH.tar.gz "${pkgname}_${pkgver}_$(uname -m).tar.gz"
chown builder ./*
runuser -u builder -- makepkg --nodeps
pacman -U --noconfirm vodarr-bin-*.pkg.tar.zst`,
		upgrade: "pacman -U --noconfirm /build/vodarr-bin-*.pkg.tar.zst",
		remove:  "systemctl disable --now vodarr && pacman -R --noconfirm vodarr-bin",
		x264:    true,
	},
}

// TestPackageService installs each Linux package from a GoReleaser snapshot in
// a container that boots systemd, as an operator would, and takes the service
// through its lifecycle. Set VODARR_SMOKE_DIST to the absolute path of the
// snapshot's dist folder, or run `make package-smoke`, which builds it first.
func TestPackageService(t *testing.T) {
	dist := os.Getenv("VODARR_SMOKE_DIST")
	if dist == "" {
		t.Skip("VODARR_SMOKE_DIST is unset; no packages to test")
	}
	if !filepath.IsAbs(dist) {
		t.Fatalf("VODARR_SMOKE_DIST is %q, want an absolute path", dist)
	}
	packaging, err := filepath.Abs(filepath.Join("..", "..", "packaging", "linux"))
	if err != nil {
		t.Fatal(err)
	}

	// Run under the unit's sandbox, the download engine's tests show that the
	// hardening leaves ffmpeg working.
	engineTests := filepath.Join(t.TempDir(), "engine.test")
	build := exec.Command("go", "test", "-c", "-o", engineTests, "github.com/combor/vodarr/internal/downloader")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the download engine tests: %v\n%s", err, out)
	}

	for _, d := range distros {
		t.Run(d.name, func(t *testing.T) {
			t.Parallel()
			testPackage(t, d, dist, packaging, engineTests)
		})
	}
}

func testPackage(t *testing.T, d distro, dist, packaging, engineTests string) {
	if d.name == "arch" && runtime.GOARCH != "amd64" {
		t.Skip("the Arch Linux image is amd64 only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	image := "vodarr-package-smoke-" + d.name
	build := exec.CommandContext(ctx, "docker", "build", "-q", "-t", image, "-")
	build.Stdin = strings.NewReader(d.dockerfile)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the image: %v\n%s", err, out)
	}
	// Privileged, so systemd can boot and build the unit's sandbox.
	id := docker(ctx, t, "run", "-d", "--privileged", "--cgroupns=private",
		"--tmpfs", "/run", "--tmpfs", "/run/lock", "-p", "127.0.0.1::8484",
		"-e", "GOARCH="+runtime.GOARCH,
		"--mount", "type=bind,readonly,source="+dist+",target=/dist",
		"--mount", "type=bind,readonly,source="+packaging+",target=/packaging",
		image, "/usr/lib/systemd/systemd")
	c := container{ctx: ctx, id: id}
	t.Cleanup(func() {
		if t.Failed() {
			// ctx is done by now.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			journal, _ := container{ctx: ctx, id: id}.try("journalctl -u vodarr --no-pager -o cat -n 100")
			t.Logf("vodarr journal:\n%s", journal)
		}
		_ = exec.Command("docker", "rm", "-f", id).Run()
	})
	c.await(t, "systemctl is-system-running", "running", "degraded")

	c.sh(t, d.install)
	if groups := strings.Fields(c.sh(t, "id -nG vodarr")); !slices.Contains(groups, "media") {
		t.Errorf("vodarr is in groups %q, want media", groups)
	}
	// The packaged settings have no API key, which vodarr won't run without.
	if state, _ := c.try("systemctl is-enabled vodarr"); state != "disabled" {
		t.Errorf("installed service is %s, want disabled", state)
	}
	c.sh(t, "systemctl start vodarr")
	c.await(t, "systemctl show -p Result --value vodarr", "exit-code")
	c.sh(t, "systemctl stop vodarr")

	docker(ctx, t, "cp", engineTests, id+":/var/lib/vodarr/engine.test")
	c.sh(t, `mkdir -p /run/systemd/system/vodarr.service.d
printf '[Service]\nExecStart=\nExecStart=/var/lib/vodarr/engine.test -test.v\nStandardOutput=file:/var/lib/vodarr/engine.log\nRestart=no\n' >/run/systemd/system/vodarr.service.d/engine.conf
systemctl daemon-reload
systemctl start vodarr`)
	c.await(t, "systemctl show -p ActiveState --value vodarr", "inactive", "failed")
	log := c.sh(t, "cat /var/lib/vodarr/engine.log")
	if result := c.sh(t, "systemctl show -p Result --value vodarr"); result != "success" {
		t.Errorf("download engine tests under the sandbox: %s\n%s", result, log)
	} else if d.x264 && !strings.Contains(log, "--- PASS: TestFFmpegDownloadsHLS") {
		t.Errorf("ffmpeg download test didn't run under the sandbox:\n%s", log)
	}
	c.sh(t, "rm -r /run/systemd/system/vodarr.service.d /var/lib/vodarr/engine.* && systemctl daemon-reload")

	const apiKey, downloadDir = "smoke", "/var/lib/vodarr/downloads"
	c.sh(t, "sed -i 's/^VODARR_API_KEY=$/VODARR_API_KEY="+apiKey+"/' /etc/vodarr/vodarr.env")
	c.sh(t, "systemctl enable --now vodarr")
	addr, _, _ := strings.Cut(docker(ctx, t, "port", id, "8484/tcp"), "\n")
	base := "http://" + addr
	checkAPIs(ctx, t, base, apiKey, downloadDir)
	// Sonarr and Radarr, in the media group, must be able to move downloads.
	if got := c.sh(t, "stat -c '%a %U:%G' "+downloadDir+"/tv"); got != "775 vodarr:media" {
		t.Errorf("category folder is %s, want 775 vodarr:media", got)
	}

	pid := c.sh(t, "systemctl show -p MainPID --value vodarr")
	c.sh(t, d.upgrade)
	if got := c.sh(t, "systemctl show -p MainPID --value vodarr"); (got != pid) != d.restarts {
		t.Errorf("upgrade changed the main PID from %s to %s, want a restart: %v", pid, got, d.restarts)
	}
	// Still serving, with the API key kept.
	checkAPIs(ctx, t, base, apiKey, downloadDir)

	c.sh(t, d.remove)
	if state, _ := c.try("systemctl is-active vodarr"); state != "inactive" {
		t.Errorf("removed service is %s, want inactive", state)
	}
	c.sh(t, "test ! -e /usr/lib/systemd/system/vodarr.service")
	c.sh(t, "test ! -e /etc/systemd/system/multi-user.target.wants/vodarr.service")
}

// container is a running container.
type container struct {
	ctx context.Context
	id  string
}

// try runs a shell script in the container and returns its trimmed output.
func (c container) try(script string) (string, error) {
	out, err := exec.CommandContext(c.ctx, "docker", "exec", c.id, "sh", "-c", script).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// sh runs a shell script in the container, failing the test if it fails.
func (c container) sh(t *testing.T, script string) string {
	t.Helper()
	out, err := c.try(script)
	if err != nil {
		t.Fatalf("%s: %v\n%s", script, err, out)
	}
	return out
}

// await runs a shell script in the container until it prints one of want.
func (c container) await(t *testing.T, script string, want ...string) {
	t.Helper()
	deadline := time.After(2 * time.Minute)
	for {
		out, _ := c.try(script)
		if slices.Contains(want, out) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("%s printed %q, want one of %q", script, out, want)
		case <-time.After(200 * time.Millisecond):
		}
	}
}
