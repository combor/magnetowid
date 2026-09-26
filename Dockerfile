# syntax=docker/dockerfile:1

# golang:1.27.1-trixie, keep in step with the toolchain in go.mod.
# Compilation runs on the build machine's own platform and cross-compiles with
# GOARCH.
FROM --platform=$BUILDPLATFORM golang@sha256:433790e515d27dc6003e847e644cc0af956985cf315c1c58a3b73ee2dd305183 AS build

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
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /vodarr ./cmd/vodarr

# alpine:3.24.2. vodarr shells out to ffmpeg, so the runtime needs a distro
# rather than distroless; the arm64 image installs it under emulation.
FROM alpine@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

LABEL org.opencontainers.image.title="vodarr" \
      org.opencontainers.image.description="Downloads movies and series from video-on-demand sites for Sonarr and Radarr." \
      org.opencontainers.image.source="https://github.com/combor/vodarr" \
      org.opencontainers.image.licenses="BSD-3-Clause"

# /downloads is owned by the default user so a fresh named volume is writable.
RUN apk add --no-cache ffmpeg \
    && install -d -o 65532 -g 65532 /downloads

COPY --from=build /vodarr /usr/local/bin/vodarr
COPY LICENSE /usr/share/licenses/vodarr/LICENSE

ENV VODARR_DOWNLOAD_DIR=/downloads
EXPOSE 8484

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/vodarr"]
