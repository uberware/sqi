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
FROM golang:1-bookworm

RUN apt-get update \
 && apt-get install -y --no-install-recommends rsync \
 && rm -rf /var/lib/apt/lists/*

RUN groupadd -g 3000 rendershare \
 && useradd -u 1001 -m -G rendershare render-a \
 && useradd -u 1002 -m -G rendershare render-b

WORKDIR /src
