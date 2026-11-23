# SPDX-License-Identifier: AGPL-3.0-or-later

# Container fixture for `make test-isolation`.
#
# The run-as-user isolation feature (internal/worker/isolation, .../session,
# .../staging) can only be tested against a real OS: real uid/gid switching,
# real supplementary groups, real directory permission bits, and a real
# symlink-preserving sync tool. A fake Provider (internal/worker/isolation/fake.go)
# cannot see any of that; see that file's doc comment.
#
# Two unprivileged accounts sharing a group let the suite check both the
# right uid (render-a) and isolation between two unprivileged accounts
# (render-a's session directory must be unreadable to render-b). rendershare
# is a third, shared supplementary group, which shows that supplementary group
# membership survives isolation.Apply as well as the primary gid.
#
# render-a is also put in root's group (gid 0, via `usermod -aG root`) so
# TestIsolation_SupplementaryGroupsStripsGidZero has a real gid 0 membership
# for the supplementary-group filtering in
# internal/worker/isolation/provider_unix.go to strip, as reported by the OS's
# own `id -G`.
#
# The source is copied into the image at build time (below) rather than
# bind-mounted at `docker run` time. A bind mount (-v $(CURDIR):/src) works
# only when the Docker daemon's host shares that path with the container
# runtime; on Colima only $HOME is virtiofs-shared by default, so a repo
# anywhere else (e.g. /Volumes/...) resolves to an empty mount and `go test`
# fails with "go.mod file not found". `docker build` streams its context from
# wherever the CLI runs, whatever the daemon's host shares. The test-isolation
# Makefile target assembles that context: a staged copy of the repo filtered
# by this directory's .dockerignore, not the repo-root .dockerignore, which
# excludes test/.
FROM golang:1-bookworm

RUN apt-get update \
 && apt-get install -y --no-install-recommends rsync \
 && rm -rf /var/lib/apt/lists/*

RUN groupadd -g 3000 rendershare \
 && useradd -u 1001 -m -G rendershare render-a \
 && useradd -u 1002 -m -G rendershare render-b \
 && usermod -aG root render-a

WORKDIR /src

# Copy dependency manifests first so Docker caches the download layer
# separately from the source (unchanged unless go.mod/go.sum change).
COPY go.mod go.sum ./
RUN go mod download

# Copy the repository source needed to build and run the suite: go.mod's
# module tree (internal/, cmd/) plus test/integration. This directory's
# .dockerignore lists what is excluded from the build context.
COPY . .
