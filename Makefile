BINARY  := magnetowid
VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test docker-smoke package-smoke integration live snapshot clean

build:
	go build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/magnetowid

test:
	@unformatted="$$(gofmt -l .)"; if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	go test -race ./...

# Bypass the test cache when the image may have changed.
docker-smoke:
	docker build --build-arg VERSION=$(VERSION) -t $(BINARY):smoke .
	MAGNETOWID_SMOKE_IMAGE=$(BINARY):smoke go test -count=1 -run TestContainerServesAPIs ./cmd/magnetowid

package-smoke:
	goreleaser release --snapshot --clean --skip=nix
	MAGNETOWID_SMOKE_DIST=$(CURDIR)/dist go test -count=1 -timeout 30m -run TestPackageService ./cmd/magnetowid

# Requires Linux, Docker, and ffmpeg with libx264.
SONARR_IMAGE ?= lscr.io/linuxserver/sonarr:4.0.20.3014-ls326@sha256:f247545d23ba8b233d6604575347e48a623fe6ad75dda02348bf81917f3b5c06
RADARR_IMAGE ?= lscr.io/linuxserver/radarr:6.4.4.10685-ls318@sha256:adb6c09d6b729ea5e642c99cea35af72702ef476bf4763f153299ac5db9f0b4f
integration:
	MAGNETOWID_SONARR_IMAGE=$(SONARR_IMAGE) MAGNETOWID_RADARR_IMAGE=$(RADARR_IMAGE) \
		go test -count=1 -timeout 20m -v -run TestSonarrAndRadarr ./cmd/magnetowid

live:
	MAGNETOWID_LIVE=1 go test -count=1 -v -run TestLive ./internal/provider/tvp

# Build release artifacts without publishing.
snapshot:
	goreleaser release --snapshot --clean

clean:
	rm -rf dist $(BINARY)
