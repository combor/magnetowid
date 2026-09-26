BINARY  := vodarr
VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test docker-smoke snapshot clean

build:
	go build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/vodarr

test:
	@unformatted="$$(gofmt -l .)"; if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	go test -race ./...

# Builds the container image and checks that it starts and answers both APIs.
# -count=1 keeps a cached pass from standing in for a run against a freshly
# built image.
docker-smoke:
	docker build --build-arg VERSION=$(VERSION) -t $(BINARY):smoke .
	VODARR_SMOKE_IMAGE=$(BINARY):smoke go test -count=1 -run TestContainerServesAPIs ./cmd/vodarr

# Full local release dry-run: binaries, archives, and native packages into dist/.
snapshot:
	goreleaser release --snapshot --clean

clean:
	rm -rf dist $(BINARY)
