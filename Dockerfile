# syntax=docker/dockerfile:1

# golang:1.27.3-trixie; keep in sync with go.mod.
FROM --platform=$BUILDPLATFORM golang@sha256:fbb7ae40b62ff8afe31662df570d39b4f5052ac32b90dae8e60d7657a20a74f4 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY cmd ./cmd
COPY internal ./internal

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /magnetowid ./cmd/magnetowid

# alpine:3.24.3 supplies ffmpeg's runtime dependencies.
FROM alpine@sha256:b3413572228e34bd1c9245f5565e6cd127005655e8a7899aef33d8e83255c9e6

LABEL org.opencontainers.image.title="magnetowid" \
      org.opencontainers.image.description="Downloads movies and series from video-on-demand sites for Sonarr and Radarr." \
      org.opencontainers.image.source="https://github.com/combor/magnetowid" \
      org.opencontainers.image.licenses="BSD-3-Clause"

# Make fresh named volumes writable by the default user.
RUN apk add --no-cache ffmpeg \
    && install -d -o 65532 -g 65532 /downloads

COPY --from=build /magnetowid /usr/local/bin/magnetowid
COPY LICENSE /usr/share/licenses/magnetowid/LICENSE

ENV MAGNETOWID_DOWNLOAD_DIR=/downloads
EXPOSE 8484

HEALTHCHECK --interval=30s --timeout=10s --start-period=30s --start-interval=2s \
    CMD ["/usr/local/bin/magnetowid", "-healthcheck"]

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/magnetowid"]
