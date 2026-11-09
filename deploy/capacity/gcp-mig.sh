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
# Requires bash 4.4 or later (it refuses to run on an older one) and jq.
#
# Actions (request and response: internal/capacity/script, and README.md):
#
#   grow    Resizes the group to the requested size. It NEVER lowers it: sqi's `current` is its own
#           record and can be below the group's real size (a group replacing preempted instances,
#           for one), so an absolute `resize` to a smaller number would delete working instances.
#           The script reads the group's own target size and the instances it runs and resizes to
#           max(desired, actual): when the request is no larger it answers ok,
#           "no change: desired <= current size", and changes nothing. A size it cannot read, or
#           one that is not a whole number, ends the script with a non-zero status and no change.
#   retire  Deletes each instance named by `members[].instance_id` with `delete-instances`, which
#           also lowers the group's target size by the instances it removes, so no replacement is
#           started. It lowers nothing else. sqi applies the one answer to every member the request
#           names, and an ok finishes them all (an ephemeral member's record is deleted), so the
#           script checks every name first: one the group does not list at all (an instance of
#           another group, or one long gone) makes it delete nothing and answer an error naming it.
#           One the group lists as being deleted counts as done. Every member needs an
#           instance_id: one without makes the whole request refused.
#   list    Answers the names of the group's instances that are not being deleted. The instance
#           name is the same value the worker reports as its cloud_instance_id.
#
# Any other action (wake) is answered "unsupported".
#
# Every instance name is checked against GCE's name rule before use; a request with any other
# value is refused whole. `desired` and `current` must be whole numbers from 0 to 100000. Request
# text is read with jq and passed to gcloud as separate arguments, never interpolated into a shell
# string. Do not add `set -x`: it would write every command to the log.
#
# Documentation-derived: this script has not been run against a real GCP project.

set -euo pipefail

if ((BASH_VERSINFO[0] < 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] < 4))); then
  echo "gcp-mig.sh needs bash 4.4 or later (this is $BASH_VERSION)" >&2
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
# --quiet makes gcloud answer its prompts instead of waiting for a terminal that is not there.
scope+=(--quiet)

# list_group fills $live_json with the names of the group's instances that are not being deleted,
# and $all_json with the names of all its instances.
list_group() {
  local listed
  listed=$(gcloud compute instance-groups managed list-instances "$mig" --format=json "${scope[@]}")
  live_json=$(jq -c '[.[]? | select(.instance != null and (.currentAction // "") != "DELETING") | .instance | split("/") | last]' <<<"$listed")
  all_json=$(jq -c '[.[]? | select(.instance != null) | .instance | split("/") | last]' <<<"$listed")
}

# group_size fills $actual, and $live_json, with the larger of the group's target size and the
# number of instances it runs. A value that is not a whole number is fatal: comparing against it
# could read as "smaller" and lower the group.
group_size() {
  local described target live_count
  described=$(gcloud compute instance-groups managed describe "$mig" --format=json "${scope[@]}")
  target=$(jq -r '.targetSize | if type == "number" and . >= 0 and . <= 100000 and . == floor then tostring else "bad" end' <<<"$described")
  [[ $target =~ ^[0-9]+$ ]] || fatal "the group's target size is not a whole number from 0 to 100000"
  list_group
  live_count=$(jq 'length' <<<"$live_json")
  [[ $live_count =~ ^[0-9]+$ ]] || fatal "the instance count is not a whole number"
  actual=$((target > live_count ? target : live_count))
}

case $action in
  grow)
    want=$(jq -r '(.desired // 0) | if type == "number" and . >= 0 and . <= 100000 and . == floor then tostring else "bad" end' <<<"$req")
    [[ $want =~ ^[0-9]+$ ]] || die "desired must be a whole number from 0 to 100000"
    # current is only checked: the group's own size decides, not sqi's record of it (see grow above).
    [[ $(jq -r '(.current // 0) | if type == "number" and . >= 0 and . <= 100000 and . == floor then tostring else "bad" end' <<<"$req") =~ ^[0-9]+$ ]] ||
      die "current must be a whole number from 0 to 100000"
    group_size
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
    # Every name is checked before anything is deleted: the answer covers them all.
    doomed=()
    gone=0
    declare -A seen=()
    for id in "${requested[@]}"; do
      [[ -z ${seen[$id]:-} ]] || continue
      seen[$id]=1
      if jq -e --arg id "$id" 'index($id) != null' <<<"$live_json" >/dev/null; then
        doomed+=("$id")
      elif jq -e --arg id "$id" 'index($id) != null' <<<"$all_json" >/dev/null; then
        gone=$((gone + 1))
      else
        die "instance $id is not a member of $mig; nothing was terminated"
      fi
    done
    if ((${#doomed[@]} > 0)); then
      names=$(
        IFS=,
        printf '%s' "${doomed[*]}"
      )
      gcloud compute instance-groups managed delete-instances "$mig" "--instances=$names" "${scope[@]}" >&2
    fi
    emit ok "deleted ${#doomed[@]}; $gone already being deleted"
    ;;

  list)
    list_group
    jq -cn --argjson ids "$live_json" '{result: "ok", instance_ids: $ids}'
    ;;
esac
