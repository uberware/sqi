#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# sqi capacity provider for an Azure Virtual Machine Scale Set (uniform orchestration).
#
# Configure it as a script provider (see README.md) and set, in the provider's `env`:
#
#   AZURE_RESOURCE_GROUP   the scale set's resource group (required)
#   AZURE_VMSS_NAME        the scale set's name (required)
#   AZURE_SUBSCRIPTION     the subscription ID or name (optional; az's default is used when unset)
#
# az reads its login from its own configuration or from the AZURE_* variables the provider's `env`
# can carry.
#
# Requires bash 4.4 or later (it refuses to run on an older one) and jq.
#
# Instance IDs: the worker reports the scale-set instance ID, the last segment of the VM's
# resource ID ("3"), as its cloud_instance_id, and `list` answers that same form. `retire` accepts
# ONLY that bare ID. A resource ID is refused: keeping its last segment would let the ID of a VM in
# another scale set pass the membership check as an instance of this one.
#
# Actions (request and response: internal/capacity/script, and README.md):
#
#   grow    Scales the set to the requested capacity. It NEVER lowers it: sqi's `current` is its own
#           record and can be below the set's real size (a set replacing preempted instances, for
#           one), so an absolute `scale` to a smaller number would delete working instances. The
#           script reads the set's own capacity and the instances it runs and scales to
#           max(desired, actual): when the request is no larger it answers ok,
#           "no change: desired <= current size", and changes nothing. A size it cannot read, or
#           one that is not a whole number, ends the script with a non-zero status and no change.
#   retire  Deletes each instance named by `members[].instance_id` with `az vmss delete-instances`,
#           which also lowers the set's capacity by the instances it removes. An instance that is
#           not in the set (or is already being deleted) is skipped, never deleted. It lowers
#           nothing else. Every member needs an instance_id: one without makes the whole request
#           refused.
#   list    Answers the scale-set instance IDs of the instances that are not being deleted.
#
# Any other action (wake) is answered "unsupported".
#
# Every instance ID is checked against ^[0-9]{1,10}$ before use; a request with any other value is
# refused whole. `desired` and `current` must be whole numbers from 0 to 100000. Request text is
# read with jq and passed to az as separate arguments, never interpolated into a shell string. Do
# not add `set -x`: it would write every command to the log.
#
# Documentation-derived: this script has not been run against a real Azure subscription.

set -euo pipefail

if ((BASH_VERSINFO[0] < 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] < 4))); then
  echo "azure-vmss.sh needs bash 4.4 or later (this is $BASH_VERSION)" >&2
  exit 1
fi

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

# fatal ends the script with a non-zero status and the reason on standard error. It is for a read
# that failed or made no sense, where no response should claim anything.
fatal() {
  echo "$1" >&2
  exit 1
}

req=$(cat)
action=$(jq -r '.action // ""' <<<"$req")

case $action in
  grow | retire | list) ;;
  *)
    emit unsupported
    exit 0
    ;;
esac

group=${AZURE_RESOURCE_GROUP:-}
vmss=${AZURE_VMSS_NAME:-}
[[ -n $group && $group != -* && -n $vmss && $vmss != -* ]] ||
  die "AZURE_RESOURCE_GROUP and AZURE_VMSS_NAME must be set and must not start with a dash"
target=(--resource-group "$group" --name "$vmss")
# --only-show-errors keeps az's warnings off the log; the subscription follows when one is set.
tail_args=(--only-show-errors)
if [[ -n ${AZURE_SUBSCRIPTION:-} ]]; then
  [[ $AZURE_SUBSCRIPTION != -* ]] || die "AZURE_SUBSCRIPTION must not start with a dash"
  tail_args+=(--subscription "$AZURE_SUBSCRIPTION")
fi

# list_group fills $live_json with the scale-set instance IDs of the instances that are not being
# deleted.
list_group() {
  local listed
  listed=$(az vmss list-instances "${target[@]}" --output json "${tail_args[@]}")
  live_json=$(jq -c '[.[]? | select((.provisioningState // "") != "Deleting") | (.instanceId // (.id | split("/") | last))]' <<<"$listed")
}

# group_size fills $actual, and $live_json, with the larger of the set's capacity and the number of
# instances it runs. A value that is not a whole number is fatal: comparing against it could read as
# "smaller" and lower the set.
group_size() {
  local shown capacity live_count
  shown=$(az vmss show "${target[@]}" --output json "${tail_args[@]}")
  capacity=$(jq -r '.sku.capacity | if type == "number" and . >= 0 and . <= 100000 and . == floor then tostring else "bad" end' <<<"$shown")
  [[ $capacity =~ ^[0-9]+$ ]] || fatal "the set's capacity is not a whole number from 0 to 100000"
  list_group
  live_count=$(jq 'length' <<<"$live_json")
  [[ $live_count =~ ^[0-9]+$ ]] || fatal "the instance count is not a whole number"
  actual=$((capacity > live_count ? capacity : live_count))
}

case $action in
  grow)
    want=$(jq -r '(.desired // 0) | if type == "number" and . >= 0 and . <= 100000 and . == floor then tostring else "bad" end' <<<"$req")
    [[ $want =~ ^[0-9]+$ ]] || die "desired must be a whole number from 0 to 100000"
    have=$(jq -r '(.current // 0) | if type == "number" and . >= 0 and . <= 100000 and . == floor then tostring else "bad" end' <<<"$req")
    [[ $have =~ ^[0-9]+$ ]] || die "current must be a whole number from 0 to 100000"
    group_size
    if ((want <= actual)); then
      emit ok "no change: desired <= current size"
      exit 0
    fi
    az vmss scale "${target[@]}" --new-capacity "$want" "${tail_args[@]}" >&2
    emit ok "capacity set to $want"
    ;;

  retire)
    mapfile -d '' -t requested < <(jq -j '.members[]? | objects | ((.instance_id // "") | if type == "string" then . else "?" end), "\u0000"' <<<"$req")
    if ((${#requested[@]} == 0)); then
      emit ok "no instances to retire"
      exit 0
    fi
    for id in "${requested[@]}"; do
      [[ $id =~ ^[0-9]{1,10}$ ]] || die "invalid or missing instance_id in members"
    done
    list_group
    doomed=()
    skipped=0
    declare -A seen=()
    for id in "${requested[@]}"; do
      [[ -z ${seen[$id]:-} ]] || continue
      seen[$id]=1
      if jq -e --arg id "$id" 'index($id) != null' <<<"$live_json" >/dev/null; then
        doomed+=("$id")
      else
        skipped=$((skipped + 1))
      fi
    done
    if ((${#doomed[@]} > 0)); then
      az vmss delete-instances "${target[@]}" --instance-ids "${doomed[@]}" "${tail_args[@]}" >&2
    fi
    emit ok "deleted ${#doomed[@]}; $skipped not a member, skipped"
    ;;

  list)
    list_group
    jq -cn --argjson ids "$live_json" '{result: "ok", instance_ids: $ids}'
    ;;
esac
