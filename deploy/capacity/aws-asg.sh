#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# sqi capacity provider for an AWS EC2 Auto Scaling group.
#
# Configure it as a script provider (see README.md) and set, in the provider's `env`:
#
#   AWS_ASG_NAME   the Auto Scaling group's name (required)
#
# The aws CLI reads its credentials, region and profile from the usual AWS_* variables, which the
# provider's `env` can also carry (AWS_REGION, AWS_PROFILE, ...).
#
# Actions (request and response: internal/capacity/script, and README.md):
#
#   grow    Raises the group's desired capacity to the requested size. It NEVER lowers it: sqi's
#           `current` is its own record and can be below the group's real size (a group replacing
#           preempted instances, for one), so an absolute `set-desired-capacity` to a smaller
#           number would terminate working instances. The script reads the group's own desired
#           capacity and the instances it runs, and sets max(desired, actual): when the request is
#           no larger it answers ok, "no change: desired <= current size", and changes nothing.
#   retire  Terminates each instance named by `members[].instance_id`, with
#           --should-decrement-desired-capacity so the group does not launch a replacement. An
#           instance that is not in the group (or is already terminating) is skipped, never
#           terminated. It lowers nothing else.
#   list    Answers the group's instance IDs that are not terminating. They are EC2 instance IDs
#           ("i-0123456789abcdef0"), the same value the worker reports as its cloud_instance_id.
#
# Any other action (wake) is answered "unsupported".
#
# Every instance ID is checked against ^i-[0-9a-f]{8}([0-9a-f]{9})?$ before use; a request with any
# other value is refused whole. Request text is read with jq and passed to aws as separate
# arguments, never interpolated into a shell string.
#
# Documentation-derived: this script has not been run against a real AWS account.

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

asg=${AWS_ASG_NAME:-}
[[ -n $asg && $asg != -* ]] || die "AWS_ASG_NAME must be set and must not start with a dash"

# load_group reads the group into $desc and fills $live_json with the IDs of its instances that are
# not terminating, and $actual with the larger of its desired capacity and their number.
load_group() {
  desc=$(aws autoscaling describe-auto-scaling-groups --auto-scaling-group-names "$asg" --output json)
  jq -e '(.AutoScalingGroups | length) == 1' <<<"$desc" >/dev/null || die "auto scaling group not found"
  live_json=$(jq -c '[.AutoScalingGroups[0].Instances // [] | .[] | select((.LifecycleState // "") | test("^Terminat") | not) | .InstanceId]' <<<"$desc")
  actual=$(jq -r '[(.AutoScalingGroups[0].DesiredCapacity // 0), (.AutoScalingGroups[0].Instances // [] | map(select((.LifecycleState // "") | test("^Terminat") | not)) | length)] | max' <<<"$desc")
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
    aws autoscaling set-desired-capacity --auto-scaling-group-name "$asg" --desired-capacity "$want" >&2
    emit ok "desired capacity set to $want"
    ;;

  retire)
    mapfile -d '' -t requested < <(jq -j '.members[]? | objects | ((.instance_id // "") | if type == "string" then . else "?" end), "\u0000"' <<<"$req")
    if ((${#requested[@]} == 0)); then
      emit ok "no instances to retire"
      exit 0
    fi
    for id in "${requested[@]}"; do
      [[ $id =~ ^i-[0-9a-f]{8}([0-9a-f]{9})?$ ]] || die "invalid or missing instance_id in members"
    done
    load_group
    terminated=0
    skipped=0
    declare -A seen=()
    for id in "${requested[@]}"; do
      [[ -z ${seen[$id]:-} ]] || continue
      seen[$id]=1
      if jq -e --arg id "$id" 'index($id) != null' <<<"$live_json" >/dev/null; then
        aws autoscaling terminate-instance-in-auto-scaling-group --instance-id "$id" --should-decrement-desired-capacity >&2
        terminated=$((terminated + 1))
      else
        skipped=$((skipped + 1))
      fi
    done
    emit ok "terminated $terminated; $skipped not a member, skipped"
    ;;

  list)
    load_group
    jq -cn --argjson ids "$live_json" '{result: "ok", instance_ids: $ids}'
    ;;
esac
