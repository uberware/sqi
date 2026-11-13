# SPDX-License-Identifier: AGPL-3.0-or-later

# Container fixture for `make chaos-container`.
#
# The chaos disk scenarios fill a volume until writes fail with ENOSPC, which
# needs a size-limited filesystem the test can fill without touching the host:
# docker mounts one with --tmpfs at /chaos/server and /chaos/worker (see the
# Makefile target). The test binary runs as root, as does the sqi-worker it
# starts (the harness sets SQI_WORKER_ALLOW_ROOT=true), and root with
# CAP_SYS_ADMIN is also what internal/loadtest/diskfill's own tests need to
# mount their tmpfs.
#
# The source is COPYied in at build time from a staged copy of the repo, not
# bind-mounted; the Makefile target's comment above `test-isolation` explains
# why. The final RUN compiles both binaries and the chaos test binary once so
# the image carries a warm build cache and a scenario run does not pay for it.
FROM golang:1.26.3-bookworm

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build ./cmd/... && go test -tags chaos -run XXX ./test/chaos/
