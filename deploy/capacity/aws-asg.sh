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
# Requires bash 4.4 or later (it refuses to run on an older one) and jq.
#
# Actions (request and response: internal/capacity/script, and README.md):
#
#   grow    Raises the group's desired capacity to the requested size. It NEVER lowers it: sqi's
#           `current` is its own record and can be below the group's real size (a group replacing
#           preempted instances, for one), so an absolute `set-desired-capacity` to a smaller
#           number would terminate working instances. The script reads the group's own desired
#           capacity and the instances it runs, and sets max(desired, actual): when the request is
#           no larger it answers ok, "no change: desired <= current size", and changes nothing.
#           A size it cannot read, or one that is not a whole number, ends the script with a
#           non-zero status and no change.
#   retire  Terminates each instance named by `members[].instance_id`, with
#           --should-decrement-desired-capacity so the group does not launch a replacement. An
#           instance that is not in the group (or is already terminating) is skipped, never
#           terminated. It lowers nothing else. Every member needs an instance_id: one without
#           makes the whole request refused.
#   list    Answers the group's instance IDs that are not terminating. They are EC2 instance IDs
#           ("i-0123456789abcdef0"), the same value the worker reports as its cloud_instance_id.
#
# Any other action (wake) is answered "unsupported".
#
# Every instance ID is checked against ^i-[0-9a-f]{8}([0-9a-f]{9})?$ before use; a request with any
# other value is refused whole. `desired` and `current` must be whole numbers from 0 to 100000.
# Request text is read with jq and passed to aws as separate arguments, never interpolated into a
# shell string. Do not add `set -x`: it would write every command to the log.
#
# Documentation-derived: this script has not been run against a real AWS account.

set -euo pipefail

if ((BASH_VERSINFO[0] < 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] < 4))); then
  echo "aws-asg.sh needs bash 4.4 or later (this is $BASH_VERSION)" >&2
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

asg=${AWS_ASG_NAME:-}
[[ -n $asg && $asg != -* ]] || die "AWS_ASG_NAME must be set and must not start with a dash"

# load_group reads the group into $desc and fills $live_json with the IDs of its instances that are
# not terminating.
load_group() {
  desc=$(aws autoscaling describe-auto-scaling-groups --auto-scaling-group-names "$asg" --output json --no-cli-pager)
  jq -e '(.AutoScalingGroups | length) == 1' <<<"$desc" >/dev/null || die "auto scaling group not found"
  live_json=$(jq -c '[.AutoScalingGroups[0].Instances // [] | .[] | select((.LifecycleState // "") | test("^Terminat") | not) | .InstanceId]' <<<"$desc")
}

# group_size fills $actual, after load_group, with the larger of the group's desired capacity and
# the number of instances it runs. A value that is not a whole number is fatal: comparing against
# it could read as "smaller" and lower the group.
group_size() {
  local cloud_desired live_count
  cloud_desired=$(jq -r '.AutoScalingGroups[0].DesiredCapacity | if type == "number" and . >= 0 and . <= 100000 and . == floor then tostring else "bad" end' <<<"$desc")
  [[ $cloud_desired =~ ^[0-9]+$ ]] || fatal "the group's desired capacity is not a whole number from 0 to 100000"
  live_count=$(jq 'length' <<<"$live_json")
  [[ $live_count =~ ^[0-9]+$ ]] || fatal "the instance count is not a whole number"
  actual=$((cloud_desired > live_count ? cloud_desired : live_count))
}

case $action in
  grow)
    want=$(jq -r '(.desired // 0) | if type == "number" and . >= 0 and . <= 100000 and . == floor then tostring else "bad" end' <<<"$req")
    [[ $want =~ ^[0-9]+$ ]] || die "desired must be a whole number from 0 to 100000"
    have=$(jq -r '(.current // 0) | if type == "number" and . >= 0 and . <= 100000 and . == floor then tostring else "bad" end' <<<"$req")
    [[ $have =~ ^[0-9]+$ ]] || die "current must be a whole number from 0 to 100000"
    load_group
    group_size
    if ((want <= actual)); then
      emit ok "no change: desired <= current size"
      exit 0
    fi
    aws autoscaling set-desired-capacity --auto-scaling-group-name "$asg" --desired-capacity "$want" --no-cli-pager >&2
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
        aws autoscaling terminate-instance-in-auto-scaling-group --instance-id "$id" --should-decrement-desired-capacity --no-cli-pager >&2
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
