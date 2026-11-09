#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# sqi capacity provider for a named group of bare-metal machines with a BMC (IPMI over LAN).
#
# Configure it as a script provider (see README.md) and set, in the provider's `env`:
#
#   IPMI_BMC_FILE   path of a text file mapping each machine to its BMC, one `hostname bmc-address`
#                   pair per line (blank lines and lines starting with # are ignored). The
#                   hostname is the one the worker registers with.
#   IPMI_USER       the BMC user name
#   IPMI_PASSWORD   the BMC password. It is passed to ipmitool through the environment (`-E`),
#                   never on the argument line, and this script never prints it.
#   IPMI_INTERFACE  the ipmitool interface (optional, default lanplus)
#
# Actions (request and response: internal/capacity/script, and README.md):
#
#   wake    `ipmitool ... chassis power on` for each member's BMC.
#   retire  `ipmitool ... chassis power soft` for each member's BMC: an ACPI soft shutdown, which
#           lets the operating system stop cleanly. sqi has already drained the member.
#
# Any other action (grow, list) is answered "unsupported": a named group has a fixed set of
# machines. A member whose hostname is not in the file (or is not a plain hostname) is not
# touched and counts as failed; the others are still acted on, and the answer is an error so sqi
# retries.
#
# Documentation-derived: this script has not been run against a real BMC.

set -euo pipefail

emit() {
  if [[ -n ${2:-} ]]; then
    jq -cn --arg result "$1" --arg message "$2" '{result: $result, message: $message}'
  else
    jq -cn --arg result "$1" '{result: $result}'
  fi
}

# die writes an error response and ends the script. It must not run in a subshell.
die() {
  emit error "$1"
  exit 0
}

req=$(cat)
action=$(jq -r '.action // ""' <<<"$req")

case $action in
  wake)
    verb="power on"
    power=on
    ;;
  retire)
    verb="soft power off"
    power=soft
    ;;
  *)
    emit unsupported
    exit 0
    ;;
esac

user=${IPMI_USER:-}
[[ -n $user && $user != -* && -n ${IPMI_PASSWORD:-} ]] || die "IPMI_USER and IPMI_PASSWORD must be set"
interface=${IPMI_INTERFACE:-lanplus}
[[ $interface =~ ^[a-z]+$ ]] || die "IPMI_INTERFACE must be an ipmitool interface name"
bmc_file=${IPMI_BMC_FILE:-}
[[ -n $bmc_file && -r $bmc_file ]] || die "IPMI_BMC_FILE must name a readable file"

# bmc_for prints the BMC address of host, or nothing when there is none.
bmc_for() {
  local want=${1,,} host addr rest
  while read -r host addr rest || [[ -n $host ]]; do
    [[ -n $host && $host != '#'* && -n $addr ]] || continue
    if [[ ${host,,} == "$want" ]]; then
      printf '%s' "$addr"
      return 0
    fi
  done <"$bmc_file"
  return 0
}

mapfile -d '' -t hosts < <(jq -j '.members[]? | objects | ((.hostname // "") | if type == "string" then . else "?" end), "\u0000"' <<<"$req")
if ((${#hosts[@]} == 0)); then
  emit ok "no members"
  exit 0
fi

done_count=0
failed=0
for host in "${hosts[@]}"; do
  bmc=
  if [[ $host =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]]; then
    bmc=$(bmc_for "$host")
  fi
  # A BMC address is a host name or an IP address, and never starts with a dash.
  if [[ ! $bmc =~ ^[A-Za-z0-9][A-Za-z0-9._:-]*$ ]]; then
    failed=$((failed + 1))
    continue
  fi
  if ipmitool -I "$interface" -H "$bmc" -U "$user" -E chassis power "$power" >&2; then
    done_count=$((done_count + 1))
  else
    failed=$((failed + 1))
  fi
done

if ((failed > 0)); then
  die "$verb sent to $done_count of ${#hosts[@]} members; $failed failed (unmapped hostname or ipmitool error)"
fi
emit ok "$verb sent to $done_count members"
