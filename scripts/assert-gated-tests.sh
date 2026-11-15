#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Usage: assert-gated-tests.sh LOG TEST...
#
# Checks that each TEST passed in the `go test -v` output LOG. A skipped or
# filtered test would still leave `go test` green, so each is asserted by name;
# the trailing space in the match keeps a name from matching a longer one with
# the same prefix. A test named in GATED (whitespace-separated) that did not
# pass is an error and fails the script; any other is advisory and only warned
# about. Writes a result table to GITHUB_STEP_SUMMARY when it is set. Used by
# the chaos jobs of .github/workflows/load-nightly.yml.
set -euo pipefail

log=$1
shift
# -d '' reads all of GATED, not just its first line, so a one-per-line YAML
# block lists every test; read then returns non-zero at the end of input.
read -r -d '' -a gated <<<"${GATED:-}" || true
summary=${GITHUB_STEP_SUMMARY:-/dev/null}

is_gated() {
  local g
  for g in "${gated[@]}"; do
    [ "$g" = "$1" ] && return 0
  done
  return 1
}

fail=0
{
  echo "| Test | Gating | Result |"
  echo "|---|---|---|"
} >>"$summary"
for t in "$@"; do
  gating=advisory
  if is_gated "$t"; then
    gating=gated
  fi
  if grep -q -- "--- PASS: $t " "$log"; then
    result=PASS
  elif [ "$gating" = gated ]; then
    echo "::error::$t did not pass (or did not run)"
    result=FAIL
    fail=1
  else
    echo "::warning::advisory test $t did not pass (or did not run)"
    result="not passed"
  fi
  echo "| $t | $gating | $result |" >>"$summary"
done
exit "$fail"
