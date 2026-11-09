#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# sqi capacity provider for a Google Compute Engine managed instance group (MIG).
#
# Configure it as a script provider (see README.md) and set, in the provider's `env`:
#
#   GCP_MIG_NAME   the managed instance group's name (required)
#   GCP_ZONE       the group's zone, for a zonal group, or
#   GCP_REGION     its region, for a regional group (set exactly one of the two)
#   GCP_PROJECT    the project (optional; gcloud's configured project is used when unset)
#
# gcloud reads its credentials from the usual variables (GOOGLE_APPLICATION_CREDENTIALS,
# CLOUDSDK_*), which the provider's `env` can also carry.
#
# Actions (request and response: internal/capacity/script, and README.md):
#
#   grow    Resizes the group to the requested size. It NEVER lowers it: sqi's `current` is its own
#           record and can be below the group's real size (a group replacing preempted instances,
#           for one), so an absolute `resize` to a smaller number would delete working instances.
#           The script reads the group's own target size and the instances it runs and resizes to
#           max(desired, actual): when the request is no larger it answers ok,
#           "no change: desired <= current size", and changes nothing.
#   retire  Deletes each instance named by `members[].instance_id` with `delete-instances`, which
#           also lowers the group's target size by the instances it removes, so no replacement is
#           started. An instance that is not in the group (or is already being deleted) is skipped,
#           never deleted. It lowers nothing else.
#   list    Answers the names of the group's instances that are not being deleted. The instance
#           name is the same value the worker reports as its cloud_instance_id.
#
# Any other action (wake) is answered "unsupported".
#
# Every instance name is checked against GCE's name rule before use; a request with any other
# value is refused whole. Request text is read with jq and passed to gcloud as separate arguments,
# never interpolated into a shell string.
#
# Documentation-derived: this script has not been run against a real GCP project.

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
  grow | retire | list) ;;
  *)
    emit unsupported
    exit 0
    ;;
esac

name_re='^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$'
mig=${GCP_MIG_NAME:-}
[[ $mig =~ $name_re ]] || die "GCP_MIG_NAME must be a valid instance group name"

zone=${GCP_ZONE:-}
region=${GCP_REGION:-}
if [[ -n $zone && -z $region ]]; then
  [[ $zone =~ ^[a-z][-a-z0-9]*$ ]] || die "GCP_ZONE is not a valid zone"
  scope=(--zone "$zone")
elif [[ -z $zone && -n $region ]]; then
  [[ $region =~ ^[a-z][-a-z0-9]*$ ]] || die "GCP_REGION is not a valid region"
  scope=(--region "$region")
else
  die "set exactly one of GCP_ZONE and GCP_REGION"
fi
if [[ -n ${GCP_PROJECT:-} ]]; then
  [[ $GCP_PROJECT =~ ^[a-z][-a-z0-9:.]*$ ]] || die "GCP_PROJECT is not a valid project ID"
  scope+=(--project "$GCP_PROJECT")
fi

# load_group fills $live_json with the names of the group's instances that are not being deleted,
# and $actual with the larger of the group's target size and their number.
load_group() {
  local described listed target
  described=$(gcloud compute instance-groups managed describe "$mig" --format=json "${scope[@]}")
  target=$(jq -r '.targetSize // 0 | if type == "number" then tostring else "0" end' <<<"$described")
  listed=$(gcloud compute instance-groups managed list-instances "$mig" --format=json "${scope[@]}")
  live_json=$(jq -c '[.[]? | select(.instance != null and (.currentAction // "") != "DELETING") | .instance | split("/") | last]' <<<"$listed")
  actual=$(jq -r --argjson target "$target" '[$target, length] | max' <<<"$live_json")
}

# list_group is load_group without the size: only the list-instances call.
list_group() {
  local listed
  listed=$(gcloud compute instance-groups managed list-instances "$mig" --format=json "${scope[@]}")
  live_json=$(jq -c '[.[]? | select(.instance != null and (.currentAction // "") != "DELETING") | .instance | split("/") | last]' <<<"$listed")
}

case $action in
  grow)
    want=$(jq -r '(.desired // 0) | if type == "number" and . >= 0 and . == floor then tostring else "bad" end' <<<"$req")
    [[ $want =~ ^[0-9]+$ ]] || die "desired must be a non-negative integer"
    load_group
    if ((want <= actual)); then
      emit ok "no change: desired <= current size"
      exit 0
    fi
    gcloud compute instance-groups managed resize "$mig" --size "$want" "${scope[@]}" >&2
    emit ok "size set to $want"
    ;;

  retire)
    mapfile -d '' -t requested < <(jq -j '.members[]? | objects | ((.instance_id // "") | if type == "string" then . else "?" end), "\u0000"' <<<"$req")
    if ((${#requested[@]} == 0)); then
      emit ok "no instances to retire"
      exit 0
    fi
    for id in "${requested[@]}"; do
      [[ $id =~ $name_re ]] || die "invalid or missing instance_id in members"
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
      names=$(IFS=,; printf '%s' "${doomed[*]}")
      gcloud compute instance-groups managed delete-instances "$mig" "--instances=$names" "${scope[@]}" >&2
    fi
    emit ok "deleted ${#doomed[@]}; $skipped not a member, skipped"
    ;;

  list)
    list_group
    jq -cn --argjson ids "$live_json" '{result: "ok", instance_ids: $ids}'
    ;;
esac
