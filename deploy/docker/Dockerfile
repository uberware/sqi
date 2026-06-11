# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Standalone multi-stage Dockerfile for sqi-server.
#
# Usage:
#   docker build -f deploy/docker/Dockerfile \
#     --build-arg VERSION=dev \
#     --build-arg COMMIT=$(git rev-parse --short HEAD) \
#     --build-arg BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
#     -t ghcr.io/uberware/sqi/sqi-server:dev .
#
# For CI releases goreleaser uses Dockerfile.goreleaser instead, which copies
# the pre-built binary rather than building inside Docker.

# ── Build stage ───────────────────────────────────────────────────────────────

FROM golang:1.26-bookworm AS builder

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
# GOVERSION is injected so `sqi-server version` can report the toolchain.
ARG GOVERSION=unknown

WORKDIR /src

# Copy dependency manifests first so Docker can cache the download layer
# separately from the source. Re-downloading only happens when go.mod changes.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# Copy the full source tree.
COPY . .

# Build the server binary with CGO disabled (modernc.org/sqlite is pure Go)
# and version metadata injected via ldflags.
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w \
      -X github.com/uberware/sqi/internal/version.Version=${VERSION} \
      -X github.com/uberware/sqi/internal/version.Commit=${COMMIT} \
      -X github.com/uberware/sqi/internal/version.BuildDate=${BUILD_DATE} \
      -X github.com/uberware/sqi/internal/version.GoVersion=${GOVERSION}" \
    -o /out/sqi-server \
    ./cmd/sqi-server

# ── Runtime stage ─────────────────────────────────────────────────────────────
#
# distroless/static-debian12 ships no shell, no package manager, and no OS
# libraries — only the CA bundle and tzdata needed by Go binaries. This keeps
# the attack surface minimal and the image small (~5 MB total).
#
# The :nonroot variant sets USER 65532 (nonroot) by default, satisfying
# pod-security-policy and most OPA/Kyverno rules that prohibit root containers.

FROM gcr.io/distroless/static-debian12:nonroot AS runtime

# OCI image annotations (https://github.com/opencontainers/image-spec).
LABEL org.opencontainers.image.title="sqi-server" \
      org.opencontainers.image.description="sqi distributed task management server" \
      org.opencontainers.image.url="https://github.com/uberware/sqi" \
      org.opencontainers.image.source="https://github.com/uberware/sqi" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later"

# Copy the statically-linked binary from the build stage.
# --chown matches the nonroot UID/GID (65532) used by distroless:nonroot.
COPY --from=builder --chown=65532:65532 /out/sqi-server /usr/local/bin/sqi-server

# sqi-server listens on 8080 by default (configurable via SQI_HTTP_ADDR).
EXPOSE 8080

# NATS embedded broker listens on 4222 by default.
EXPOSE 4222

ENTRYPOINT ["/usr/local/bin/sqi-server"]
CMD ["serve"]
