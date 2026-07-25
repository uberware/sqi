# SPDX-License-Identifier: AGPL-3.0-or-later

# Container fixture for `make test-isolation`.
#
# The run-as-user isolation feature (internal/worker/isolation, .../session,
# .../staging) can only be verified against a real OS: real uid/gid switching,
# real supplementary groups, real directory permission bits, and a real
# symlink-preserving sync tool. A fake Provider (internal/worker/isolation/fake.go)
# is structurally blind to all of that — see that file's own doc comment. This
# mirrors why `make test-ldap` exists: a fake cannot see a wire-level mistake a
# real OpenLDAP server exposes.
#
# Two unprivileged accounts sharing a group let the suite check both "the
# right uid" (render-a) and "isolation between two different unprivileged
# accounts" (render-a's session directory must be unreadable to render-b).
# rendershare is a THIRD shared supplementary group — proof that supplementary
# group membership survives isolation.Apply, not just the primary gid.
#
# render-a is ALSO put in root's group (gid 0, via `usermod -aG root`) so
# TestIsolation_SupplementaryGroupsStripsGidZero has something real to strip:
# without this, no account in this image is ever in gid 0, and the gid-0
# strip in internal/worker/isolation/provider_unix.go's supplementary-group
# filtering is asserted only against isolation.NewFake — never exercised by a
# real OS's actual `id -G` output.
#
# The source is COPYied into the image at build time (below) rather than
# bind-mounted at `docker run` time. A bind mount (-v $(CURDIR):/src) only
# works when the Docker daemon's host actually shares that path with the
# container runtime; on Colima (common on macOS) only $HOME is virtiofs-shared
# by default, so a repo living anywhere else (e.g. /Volumes/...) resolves to
# an EMPTY mount and `go test` fails with "go.mod file not found" before a
# single test runs. COPY has no such dependency: `docker build` always reads
# its context from wherever the CLI itself runs and streams it to the daemon,
# regardless of what the daemon's host filesystem shares with it. See the
# test-isolation Makefile target for how the build context is assembled (a
# staged copy of the repo, filtered by this directory's .dockerignore, NOT the
# repo-root .dockerignore — that one excludes test/ entirely, which this image
# needs).
FROM golang:1-bookworm

RUN apt-get update \
 && apt-get install -y --no-install-recommends rsync \
 && rm -rf /var/lib/apt/lists/*

RUN groupadd -g 3000 rendershare \
 && useradd -u 1001 -m -G rendershare render-a \
 && useradd -u 1002 -m -G rendershare render-b \
 && usermod -aG root render-a

WORKDIR /src

# Copy dependency manifests first so Docker can cache the download layer
# separately from the source (unchanged unless go.mod/go.sum themselves
# change).
COPY go.mod go.sum ./
RUN go mod download

# Copy the full repository source needed to build and run the suite:
# go.mod's own module tree (internal/, cmd/) plus test/integration itself.
# See this directory's .dockerignore for what's excluded from the build
# context that this COPY draws from.
COPY . .
