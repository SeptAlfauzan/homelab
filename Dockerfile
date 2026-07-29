# syntax=docker/dockerfile:1
#
# Build:
#   podman build -t armbian-stats-web .
#
# Multi-arch build (e.g. building an amd64 dev machine image for an ARM board):
#   podman build --platform linux/arm64 -t armbian-stats-web:arm64 .
#   podman build --platform linux/arm/v7 -t armbian-stats-web:armv7 .
#
# Run (see README notes below the Dockerfile / the chat response for the
# full explanation of each mount):
#
#   podman run -d --name armbian-stats-web \
#     --network host \
#     -v /:/hostfs:ro \
#     -v /run/podman/podman.sock:/var/run/docker.sock:ro \
#     -e DOCKER_HOST=unix:///var/run/docker.sock \
#     armbian-stats-web -disk-path /hostfs

# ---- Build stage --------------------------------------------------------
FROM golang:1.22-alpine AS build

WORKDIR /src

COPY go.mod ./
COPY main.go ./
COPY static ./static

# TARGETOS/TARGETARCH/TARGETVARIANT are set automatically by
# `podman build --platform ...` / `podman buildx build --platform ...`.
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG TARGETVARIANT=

RUN set -eux; \
    export CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH"; \
    if [ "$TARGETARCH" = "arm" ] && [ -n "$TARGETVARIANT" ]; then \
        export GOARM="${TARGETVARIANT#v}"; \
    fi; \
    go build -trimpath -ldflags="-s -w" -o /out/homelab .

# ---- Runtime stage -------------------------------------------------------
FROM alpine:3.20

# docker-cli lets the app shell out to query a mounted Podman/Docker socket
# for the "Containers" panel (Podman's API is Docker-API compatible).
# ca-certificates/tzdata are cheap and generally useful to have around.
RUN apk add --no-cache docker-cli ca-certificates tzdata \
    && addgroup -S app && adduser -S -G app app

COPY --from=build /out/homelab /usr/local/bin/homelab

USER app
EXPOSE 5000

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:5000/api/stats || exit 1

ENTRYPOINT ["/usr/local/bin/homelab"]
CMD ["-addr", ":5000"]
