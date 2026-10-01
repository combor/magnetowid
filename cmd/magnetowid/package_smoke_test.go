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

type distro struct {
	name       string
	dockerfile string
	// Container mounts: snapshot at /dist, packaging files at /packaging;
	// GOARCH selects the package architecture.
	install, upgrade, remove string
	// Arch upgrades leave service restarts to the operator.
	restarts bool
	// Fedora's ffmpeg-free lacks the test streams' libx264 encoder.
	x264 bool
}

var distros = []distro{
	{
		name: "debian",
		dockerfile: `FROM debian:trixie-slim
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends systemd`,
		install:  "apt-get install -y /dist/magnetowid_*_linux_$GOARCH.deb",
		upgrade:  "apt-get install -y --reinstall /dist/magnetowid_*_linux_$GOARCH.deb",
		remove:   "apt-get remove -y magnetowid",
		restarts: true,
		x264:     true,
	},
	{
		name: "fedora",
		dockerfile: `FROM fedora:44
RUN dnf install -y systemd`,
		install:  "dnf install -y /dist/magnetowid_*_linux_$GOARCH.rpm",
		upgrade:  "dnf reinstall -y /dist/magnetowid_*_linux_$GOARCH.rpm",
		remove:   "dnf remove -y magnetowid",
		restarts: true,
	},
	{
		// Build the generated AUR PKGBUILD to verify its archive checksums.
		name: "arch",
		dockerfile: `FROM archlinux:latest
RUN pacman -Syu --noconfirm --needed binutils debugedit fakeroot && useradd -m builder`,
		install: `set -e
install -d -o builder /build
cd /build
cp /dist/aur/magnetowid-bin.pkgbuild PKGBUILD
cp /packaging/magnetowid.install .
. ./PKGBUILD
cp /dist/magnetowid_*_linux_$GOARCH.tar.gz "${pkgname}_${pkgver}_$(uname -m).tar.gz"
chown builder ./*
runuser -u builder -- makepkg --nodeps
pacman -U --noconfirm magnetowid-bin-*.pkg.tar.zst`,
		upgrade: "pacman -U --noconfirm /build/magnetowid-bin-*.pkg.tar.zst",
		remove:  "systemctl disable --now magnetowid magnetowid-vpn@pl && pacman -R --noconfirm magnetowid-bin",
		x264:    true,
	},
}

// Set MAGNETOWID_SMOKE_DIST to the snapshot directory, or run make package-smoke.
func TestPackageService(t *testing.T) {
	dist := os.Getenv("MAGNETOWID_SMOKE_DIST")
	if dist == "" {
		t.Skip("MAGNETOWID_SMOKE_DIST is unset; no packages to test")
	}
	if !filepath.IsAbs(dist) {
		t.Fatalf("MAGNETOWID_SMOKE_DIST is %q, want an absolute path", dist)
	}
	packaging, err := filepath.Abs(filepath.Join("..", "..", "packaging", "linux"))
	if err != nil {
		t.Fatal(err)
	}

	// Run engine tests under the unit's sandbox to check ffmpeg still works.
	engineTests := filepath.Join(t.TempDir(), "engine.test")
	build := exec.Command("go", "test", "-c", "-o", engineTests, "github.com/combor/magnetowid/internal/downloader")
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

	image := "magnetowid-package-smoke-" + d.name
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
			journal, _ := container{ctx: ctx, id: id}.try("journalctl -u magnetowid --no-pager -o cat -n 100")
			t.Logf("magnetowid journal:\n%s", journal)
		}
		_ = exec.Command("docker", "rm", "-f", id).Run()
	})
	c.await(t, "systemctl is-system-running", "running", "degraded")

	c.sh(t, d.install)
	if groups := strings.Fields(c.sh(t, "id -nG magnetowid")); !slices.Contains(groups, "media") {
		t.Errorf("magnetowid is in groups %q, want media", groups)
	}
	// Do not enable the service before an API key is configured.
	if state, _ := c.try("systemctl is-enabled magnetowid"); state != "disabled" {
		t.Errorf("installed service is %s, want disabled", state)
	}
	c.sh(t, "systemctl start magnetowid")
	c.await(t, "systemctl show -p Result --value magnetowid", "exit-code")
	c.sh(t, "systemctl stop magnetowid")

	docker(ctx, t, "cp", engineTests, id+":/var/lib/magnetowid/engine.test")
	c.sh(t, `mkdir -p /run/systemd/system/magnetowid.service.d
printf '[Service]\nExecStart=\nExecStart=/var/lib/magnetowid/engine.test -test.v\nStandardOutput=file:/var/lib/magnetowid/engine.log\nRestart=no\n' >/run/systemd/system/magnetowid.service.d/engine.conf
systemctl daemon-reload
systemctl start magnetowid`)
	c.await(t, "systemctl show -p ActiveState --value magnetowid", "inactive", "failed")
	log := c.sh(t, "cat /var/lib/magnetowid/engine.log")
	if result := c.sh(t, "systemctl show -p Result --value magnetowid"); result != "success" {
		t.Errorf("download engine tests under the sandbox: %s\n%s", result, log)
	} else if d.x264 && !strings.Contains(log, "--- PASS: TestFFmpegDownloadsHLS") {
		t.Errorf("ffmpeg download test didn't run under the sandbox:\n%s", log)
	}
	c.sh(t, "rm -r /run/systemd/system/magnetowid.service.d /var/lib/magnetowid/engine.* && systemctl daemon-reload")

	const apiKey, downloadDir = "smoke", "/var/lib/magnetowid/downloads"
	c.sh(t, "sed -i 's/^MAGNETOWID_API_KEY=$/MAGNETOWID_API_KEY="+apiKey+"/' /etc/magnetowid/magnetowid.env")
	c.sh(t, "systemctl enable --now magnetowid")
	addr, _, _ := strings.Cut(docker(ctx, t, "port", id, "8484/tcp"), "\n")
	base := "http://" + addr
	checkAPIs(ctx, t, base, apiKey, downloadDir)
	// Sonarr and Radarr, in the media group, must be able to move downloads.
	if got := c.sh(t, "stat -c '%a %U:%G' "+downloadDir+"/tv"); got != "775 magnetowid:media" {
		t.Errorf("category folder is %s, want 775 magnetowid:media", got)
	}

	// A VPN exit runs Gluetun with its instance's settings. Docker is faked: a
	// script that records its arguments, behind a unit that does nothing.
	c.sh(t, `printf '#!/bin/sh\necho "$@" >>/run/docker.args\n[ "$1" = run ] && exec sleep infinity\nexit 0\n' >/usr/bin/docker
chmod +x /usr/bin/docker
printf '[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=/bin/true\n' >/run/systemd/system/docker.service
install -m 600 /usr/share/magnetowid/vpn.env.example /etc/magnetowid/vpn-pl.env
systemctl daemon-reload
systemctl enable --now magnetowid-vpn@pl`)
	// The example names no VPN provider, so the exit must fail without retrying.
	c.await(t, "systemctl is-failed magnetowid-vpn@pl", "failed")
	if got := c.sh(t, "systemctl show -p ExecMainStatus -p NRestarts magnetowid-vpn@pl | sort | tr '\\n' ' '"); got != "ExecMainStatus=78 NRestarts=0" {
		t.Errorf("the VPN exit without a provider ended with %s", got)
	}
	if run, _ := c.try("grep '^run ' /run/docker.args"); run != "" {
		t.Errorf("the VPN exit ran docker %s without a provider", run)
	}
	c.sh(t, `sed -i 's/^VPN_SERVICE_PROVIDER=$/VPN_SERVICE_PROVIDER=example/; s/^MAGNETOWID_VPN_PORT=.*/MAGNETOWID_VPN_PORT=8889/' /etc/magnetowid/vpn-pl.env
systemctl restart magnetowid-vpn@pl`)
	c.await(t, "systemctl is-active magnetowid-vpn@pl", "active")
	run := c.sh(t, "grep '^run ' /run/docker.args")
	// Gluetun's proxy stays on its own port, whatever the local one.
	for _, want := range []string{"--name magnetowid-vpn-pl ", "--env-file /etc/magnetowid/vpn-pl.env ",
		"--publish 127.0.0.1:8889:8888/tcp ", " qmcgaw/gluetun:"} {
		if !strings.Contains(run, want) {
			t.Errorf("the VPN exit ran docker %s, without %q", run, want)
		}
	}

	pid := c.sh(t, "systemctl show -p MainPID --value magnetowid")
	c.sh(t, d.upgrade)
	if got := c.sh(t, "systemctl show -p MainPID --value magnetowid"); (got != pid) != d.restarts {
		t.Errorf("upgrade changed the main PID from %s to %s, want a restart: %v", pid, got, d.restarts)
	}
	checkAPIs(ctx, t, base, apiKey, downloadDir)
	if state, _ := c.try("systemctl is-active magnetowid-vpn@pl"); state != "active" {
		t.Errorf("upgraded package's VPN exit is %s, want active", state)
	}

	c.sh(t, d.remove)
	for _, unit := range []string{"magnetowid", "magnetowid-vpn@pl"} {
		if state, _ := c.try("systemctl is-active " + unit); state != "inactive" {
			t.Errorf("removed package's %s is %s, want inactive", unit, state)
		}
		c.sh(t, "test ! -e /etc/systemd/system/multi-user.target.wants/"+unit+".service")
	}
	c.sh(t, "test ! -e /usr/lib/systemd/system/magnetowid.service")
	c.sh(t, "test ! -e /usr/lib/systemd/system/magnetowid-vpn@.service")
	c.sh(t, "test ! -e /usr/share/magnetowid/vpn.env.example")
}

type container struct {
	ctx context.Context
	id  string
}

func (c container) try(script string) (string, error) {
	out, err := exec.CommandContext(c.ctx, "docker", "exec", c.id, "sh", "-c", script).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (c container) sh(t *testing.T, script string) string {
	t.Helper()
	out, err := c.try(script)
	if err != nil {
		t.Fatalf("%s: %v\n%s", script, err, out)
	}
	return out
}

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
