# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Standalone multi-stage Dockerfile for sqi-worker.
# Alpine-based (see docs/worker-docker.md, "Image") for lightweight cloud worker deployments.
#
# Usage:
#   docker build -f deploy/docker/worker/Dockerfile \
#     --build-arg VERSION=dev \
#     --build-arg COMMIT=$(git rev-parse --short HEAD) \
#     --build-arg BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
#     -t ghcr.io/uberware/sqi/sqi-worker:dev .
#
# For CI releases goreleaser uses Dockerfile.goreleaser instead, which copies
# the pre-built binary rather than building inside Docker.

# ── Build stage ───────────────────────────────────────────────────────────────

FROM golang:1.26-bookworm AS builder

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
# GOVERSION is injected so `sqi-worker version` can report the toolchain.
ARG GOVERSION=unknown

WORKDIR /src

# Copy dependency manifests first so Docker can cache the download layer
# separately from the source. Re-downloading only happens when go.mod changes.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# Copy the full source tree.
COPY . .

# Build the worker binary with CGO disabled (modernc.org/sqlite is pure Go)
# and version metadata injected via ldflags.
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w \
      -X github.com/uberware/sqi/internal/version.Version=${VERSION} \
      -X github.com/uberware/sqi/internal/version.Commit=${COMMIT} \
      -X github.com/uberware/sqi/internal/version.BuildDate=${BUILD_DATE} \
      -X github.com/uberware/sqi/internal/version.GoVersion=${GOVERSION}" \
    -o /out/sqi-worker \
    ./cmd/sqi-worker

# ── Runtime stage ─────────────────────────────────────────────────────────────
#
# Alpine provides a minimal shell environment and ca-certificates/tzdata for
# TLS and timezone support. The shell is intentionally retained — render workers
# sometimes need to exec shell scripts as task commands. A dedicated non-root
# user (sqiworker) limits the blast radius of a compromised task process.

FROM alpine:3.21 AS runtime

RUN apk add --no-cache ca-certificates tzdata

# Create a non-root user for running the worker process.
# Alpine uses addgroup/adduser (BusyBox), not useradd.
RUN addgroup -S sqiworker && adduser -S sqiworker -G sqiworker

# OCI image annotations (https://github.com/opencontainers/image-spec).
LABEL org.opencontainers.image.title="sqi-worker" \
      org.opencontainers.image.description="sqi distributed task worker agent" \
      org.opencontainers.image.url="https://github.com/uberware/sqi" \
      org.opencontainers.image.source="https://github.com/uberware/sqi" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later"

# Copy the statically-linked binary from the build stage.
COPY --from=builder --chown=sqiworker:sqiworker /out/sqi-worker /usr/local/bin/sqi-worker

# Data directory for worker ID persistence and session working directories.
# Mount a persistent volume here so the worker retains its ID across restarts.
RUN mkdir -p /var/lib/sqi-worker && chown sqiworker:sqiworker /var/lib/sqi-worker

USER sqiworker

# Metrics / health-probe port (Prometheus + /healthz, /readyz).
EXPOSE 9091

ENTRYPOINT ["/usr/local/bin/sqi-worker"]
CMD ["start"]
