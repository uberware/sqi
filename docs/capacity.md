# Capacity groups

A render farm's demand is spiky. Machines that sit idle cost money, and
machines that are missing during a crunch cost deadlines. A **capacity group**
is how an operator tells sqi which machines may come and go with demand, so that
sqi can publish how many of them the waiting work justifies.

This guide is for farm admins and for cloud or devops engineers wiring an
autoscaler to sqi.

> **What this release does.** Capacity groups are delivered in two parts. The
> first, described here, is the **signal**: sqi works out, for each group, how
> much waiting work the group could run, how many machines it should have and
> which of its machines are safe to give back, and it publishes that as
> metrics and over REST. **sqi does not start, stop, wake or sleep any machine.**
> Your own autoscaler (an AWS Auto Scaling group, a GCP managed instance group,
> an Azure scale set, KEDA, or a script) acts on the signal. The one thing sqi
> does do on its own is clean up: a cloud machine that left for good is deleted
> from the worker list and its credential revoked
> ([below](#worker-shutdown-and-drain)).
>
> Acting providers (waking on-premises machines over the LAN and putting them
> back to sleep, and a generic script provider that runs your commands to grow
> and shrink a group) are planned as the second part and are not available yet.
> The API accepts only `provider: external` and refuses the others with 400.

Nothing here changes a farm that has not defined a capacity group. With no
group, the scheduler does no capacity work, logs nothing about capacity and
exports no `sqi_capacity_*` series.

## What a capacity group is

A capacity group is a named set of machines that sqi may follow demand with. It
has a farm, minimum and maximum machine counts, a priority against other
groups, pacing settings, and optionally the queues it serves and a profile of
what one of its machines offers. Groups are managed through
`/api/v1/capacity-groups` and the web UI under **Admin, Capacity Groups**, and
need the `infra.read` and `infra.manage` permissions like queues and usage
pools.

There are two kinds, chosen when the group is created:

| Kind | Machines are | Typical use | How they join |
|---|---|---|---|
| `named` | specific, already-provisioned workers | on-premises render nodes that are normally switched off | an operator adds an existing worker to the group by ID |
| `sized` | interchangeable and anonymous until they register | cloud instances booted from one image | they enrol with the group's token, or name the group in their config ([Joining a group](#joining-a-group)) |

A sized group declares a `machine_profile`: what one of its machines offers
(cores, memory, GPUs, operating system, tags and so on). sqi uses the profile to
decide which waiting work the group could run before any machine exists. A
named group needs no profile, because sqi reads each member's own last
registration.

A worker belongs to at most one group. A worker in no group behaves exactly as
it always did.

### The opt-in rule

A machine is only ever part of a group because an operator put it there: by
adding it to a named group, by issuing a group token and baking it into an
image, or (on a farm with broker authentication off) by setting
`worker.capacity_group` on it. sqi does no idle detection of a user's
workstation and no schedule: if a machine should only render overnight, that is
your machine policy, and disabling a worker is the maintenance hold. sqi never
touches a disabled member.

## Creating a group

```sh
curl -s -X POST "$BASE/capacity-groups" \
  -H 'Content-Type: application/json' \
  -d '{
    "farm_id": "'"$FARM_ID"'",
    "name": "cloud-spot",
    "kind": "sized",
    "provider": "external",
    "min_machines": 0,
    "max_machines": 40,
    "max_step_up": 5,
    "scale_up_cooldown_seconds": 300,
    "idle_timeout_seconds": 900,
    "boot_timeout_seconds": 600,
    "machine_profile": {
      "cpu_count": 16,
      "ram_mb": 65536,
      "os": "linux",
      "tags": { "houdini": "20.5" }
    }
  }'
```

`PUT /capacity-groups/{id}` replaces a group's settings (a field you leave out
returns to its default), and `DELETE` removes a group that has no members. Unknown
fields are refused with 400, so a misspelt setting does not silently fall back
to its default.

| Field | Default | Rules |
|---|---|---|
| `farm_id` | required on create | cannot change afterwards |
| `name` | required | unique; 1 to 63 characters: letters, digits, `.`, `_`, `-`, starting with a letter or digit. The metrics use it as their label. |
| `kind` | required | `named` or `sized`; cannot change while the group has members |
| `provider` | `external` | only `external` is accepted in this release |
| `enabled` | `true` | a disabled group is not evaluated, exports nothing, and its members count as ordinary workers |
| `queue_ids` | `[]` (every queue in the farm) | up to 1000 queues, all in the group's farm. Only ready work in these queues drives the group. This is the main lever for keeping a low-priority backlog from launching paid machines. |
| `min_machines` | `0` | 0 up to `max_machines` |
| `max_machines` | required | 1 to 10000; a hard cap on the signal |
| `priority` | `0` | higher is preferred when groups compete for the same work |
| `max_step_up` | `1` | 1 to 10000; the most machines one evaluation may add |
| `scale_up_cooldown_seconds` | `60` | 0 up to one year; the shortest time between step-ups |
| `idle_timeout_seconds` | `600` | 60 up to one year; how long a member must sit idle before it is a retire candidate |
| `boot_timeout_seconds` | `600` | 60 up to one year; how long a machine that was asked for has to register, and how long a vanished cloud machine is kept ([below](#worker-shutdown-and-drain)). Keep it shorter than `scheduler.offline_worker_retention`. |
| `retire_action` | `sleep` | `sleep` or `shutdown`. Stored for the acting providers; it has no effect on the signal. |
| `machine_profile` | `{}` | a sized group must declare `cpu_count` of at least 1 |

The profile may declare `cpu_count`, `ram_mb`, `gpu_count`, `gpu_memory_mb`,
`os`, `arch`, `compute_location`, `tags`, `extensions` and `expr_limits`. A
field you leave out is not declared, and a machine offering more than the
profile declares is not a mismatch.

## How the signal is computed

Every scheduler sweep tick (about every 15 seconds), on the leader only, sqi
evaluates each enabled group. The work is per step, not per task, so a job with a
million ready tasks costs the same as one with a single task.

### What demand counts

Only work that a machine of the group could actually run is counted. For each
step that has leasable ready tasks, sqi asks whether the group's machine (the
profile of a sized group, or each offline member of a named group) could ever
run it, ignoring everything that varies with load:

- the farm, the group's queue allow-list, and the machine's compute location;
- required tags and attribute requirements, operating system and architecture;
- required amounts (cores, memory, GPUs, GPU memory);
- OpenJD extensions the worker must advertise, and the EXPR limits the worker
  enforces, where the profile declares them;
- that the step's core requirement fits on one machine at all;
- that a usage pool the step names exists (a pool at its cap still counts as
  runnable; a pool that does not exist does not).

Work that no group could run drives nothing. A job needing Houdini does not
grow a group whose profile has no Houdini tag, and a step that needs more cores
than one machine has is not counted. A sized group's profile has no operating
system **version** field, so a step requiring `attr.worker.os.version` never
counts toward a sized group. A named group is judged from each member's own
registration, which does carry its version.

Work is then taken off the demand before any group sees it:

1. **Idle workers absorb first.** Workers outside any group are consulted
   first, then each group's online members. A worker's free cores are spent
   once, across all demand, in the order the lease path would offer the work
   (job priority, then job creation, then step order).
2. **Groups take what is left, by priority.** The highest-priority group able to
   run a piece of work takes it, up to the machines it could still add; what it
   cannot take passes to the next group. This gives "local before cloud" as
   configuration.

A step that declares no per-task core count reserves a whole machine, as the
lease path does.

### The desired size

`desired_machines` is a **total target**. It counts the machines already
present, including the retire candidates, which are listed separately:

- If the group's demand calls for `need` more machines, the target is
  `online + need`, limited as below. `need` is the group's `demand_cores`
  divided by the cores one machine brings, rounded up.
- If nothing is needed, the target shrinks to the machines still useful: the
  online members that are not retire candidates.
- It is never below `min_machines` nor above `max_machines`.

How to act on it depends on your actuator:

| Actuator | Do this |
|---|---|
| Sets total capacity (an ASG's desired capacity, a MIG target size, KEDA replicas) | Set capacity to `desired_machines`. Do not also subtract the retire candidates. |
| Adds and removes machines incrementally | Launch `desired_machines - online_members`, less any machines you launched that have not registered yet, when positive. Retire the `retire_candidates` by name. Never subtract both from the same figure: while a group grows the candidates are already counted in `desired_machines` (they are present until retired), and while it shrinks they are already excluded. |

A count-driven autoscaler that cannot choose which machine to remove may remove
a busy one when shrinking; see [Limits and known gaps](#limits-and-known-gaps).

### How fast a group grows

Growth is bounded three ways, so that a stuck job or a broken image cannot
launch a thousand machines:

- **`max_machines`** is a hard cap.
- **`max_step_up`** limits how many machines one evaluation adds, and the step is
  measured from **registered** machines. If an image never registers, the group
  does not compound: it costs at most one step.
- **`scale_up_cooldown_seconds`** is the shortest time between step-ups. Set it
  at least as long as a machine takes to boot, or the group will ask for another
  step while the first is still starting.

Machines a step-up asked for that have not registered yet are held in
`desired_machines` for `boot_timeout_seconds`, so the target does not fall back
while they boot. After the timeout they are no longer held, and the group may
ask for one further step, measured from the machines that did register.

Not available yet: a degraded state with backoff for a broken image (so that the
group stops asking at all, instead of one step per boot timeout). It is planned
for the second part of capacity groups (H8b), with the acting providers. Until
then, the guards above limit the cost, and a mismatch report (next section) is
how you notice.

### Profile mismatches

If a sized group's image registers without what the profile promises, the
machine is a member that cannot run the work that called for it. The work stays
and the group keeps asking, bounded as above. To make that visible, the status
lists each online member that falls short of the profile as a **mismatch**, and
the server logs a warning once per member and reason. sqi compares:

- CPU count, memory, GPU count and GPU memory below the profile's;
- operating system family, architecture and compute location that differ;
- tags the profile declares that are missing or have another value;
- extensions the profile declares that the worker does not advertise;
- EXPR limits the profile declares that the worker enforces lower;
- queue affinity: a worker pinned to one queue that does not cover the group's
  queues. A worker with no single queue affinity covers any list.

### Retire candidates

A member is a retire candidate when it is online, not disabled, holds no task,
has been idle for at least `idle_timeout_seconds`, and absorbed none of this
evaluation's demand. The list is longest idle first, and retiring all of it
never leaves fewer online members than `min_machines`.

Idle time is sampled once per sweep tick and kept in memory. After a server
restart or a high-availability failover the new leader starts counting again, so
no machine is offered for retirement sooner than `idle_timeout_seconds` after
the leader first sees it idle. The same loss applies to machines still booting
(see below).

## Reading the signal

### The status

`GET /api/v1/capacity-groups/{id}` returns the group with its last evaluation
in `status`; the list endpoint does the same for every group. `status` is null
until the first evaluation and while the group is disabled.

Under [high availability](operations.md#running-more-than-one-server-high-availability)
only the leader evaluates groups and answers these routes. A standby answers
`503` on every REST route except the probes and `/metrics`, and its `/metrics`
carries no `sqi_capacity_*` series. Point an autoscaler at the leader (through a
load balancer that health-checks `/leaderz`), or scrape every server and ignore
the ones without the series. After a failover the new leader's `status` is null
until its first evaluation.

```json
{
  "name": "cloud-spot",
  "kind": "sized",
  "max_machines": 40,
  "status": {
    "demand_cores": 192,
    "desired_machines": 14,
    "online_members": 6,
    "offline_members": 1,
    "retire_candidates": [],
    "mismatches": [],
    "evaluated_at": "2026-11-08T10:15:30Z"
  }
}
```

`evaluated_at` is when the figures were computed. If the server cannot read the
store, it keeps the last evaluation and its gauges as they were rather than
publishing zeros, so a stale `evaluated_at` is how you tell a quiet farm from a
blind server. Reading the status needs `infra.read` when `auth.enabled` is on.

`GET /api/v1/capacity-groups/{id}/members` lists a group's workers in the shape
`GET /workers` returns.

### Metrics

`GET /metrics` on `sqi-server` carries three gauges per enabled group. The label
`group` is the group **name**. Only the leader exports them (a standby has no
`sqi_capacity_*` series; see [above](#the-status)), and `/metrics` is never gated
by authentication.

| Metric | Labels | Meaning |
|---|---|---|
| `sqi_capacity_demand_cores` | `group` | cores of ready work the group could run that no idle worker absorbed and no higher-priority group took |
| `sqi_capacity_desired_machines` | `group` | the total machines the group should have, counting the machines it already has (not the number to add) |
| `sqi_capacity_members` | `group`, `state` | the group's members by liveness; `state` is `online` or `offline`, and a disabled member counts in whichever it is |

A group's series disappear when it is disabled, renamed or deleted. See also
[Observability](observability.md).

`sqi_scheduler_queue_depth{queue}` (the label is the queue ID) now falls back to
`0` when a queue drains, instead of keeping its last non-zero value, because a
scaling signal that never falls would never scale in. Only a queue the scheduler
has already seen since it started is reset.

## Joining a group

### Named groups

Add an existing worker (one that has registered before) to a named group:

```sh
curl -s -X PUT "$BASE/capacity-groups/$GROUP_ID/members/$WORKER_ID"
curl -s -X DELETE "$BASE/capacity-groups/$GROUP_ID/members/$WORKER_ID"
```

The worker must be in the group's farm and in no other group. Members of a
named group are permanent: sqi never deletes them, and the offline-worker
retention sweep skips them. Only an offline, enabled member counts as a machine
the group could add.

### Sized groups with broker authentication on

With `auth.enabled` and `nats.auth.enabled` both on, a machine joins a sized
group by enrolling with the group's **group token**, which makes it an
*ephemeral* member. Mint one:

```sh
curl -s -X POST "$BASE/capacity-groups/$GROUP_ID/tokens" \
  -H 'Content-Type: application/json' \
  -d '{"name": "spot-image-2026-11", "expires_in": "720h"}'
```

The response carries the raw `token`, shown once. Put it where the worker reads
its join token ([`nats.join_token_file`](worker-configuration.md), preferred, or
`nats.join_token`) in the image's launch configuration. On first boot the worker
generates its key, enrols, and joins the group.

- **Group tokens are reusable**: any number of machines may enrol with one,
  whatever `nats.auth.join_token_single_use` says. They are tied to one sized
  group.
- **Expiry** defaults to 720 hours (30 days) and may be at most 2160 hours
  (90 days). Because a leaked reusable token is useful until it expires, rotate:
  mint the new token, update the launch template, and revoke the old one when no
  template refers to it. Both are valid meanwhile.
- **Revoking a token** (`DELETE /capacity-groups/{id}/tokens/{tokenID}`) stops
  new enrolments. It does **not** revoke credentials already issued from it:
  revoke those machines with `DELETE /workers/{id}/credential`.
- `GET /capacity-groups/{id}/tokens` lists a group's tokens (prefix, expiry,
  revocation; never the secret). Minting, listing and revoking need
  `infra.manage`.
- Minting needs REST `auth.enabled`: with it off the route is not mounted, and a
  `POST` is answered `405`. With REST auth on but `nats.auth.enabled` off, it is
  answered `409` with `group tokens need nats.auth.enabled; workers on an
  auth-off farm join with worker.capacity_group`. A named group answers `409`
  too, since its members are added by name.
- **With REST `auth.enabled: false` and `nats.auth.enabled: true`, no machine can
  join a sized group in this release.** The mint route is not mounted,
  `sqi-server worker token issue` mints only ordinary join tokens (never a group
  token), and `worker.capacity_group` is ignored because broker authentication
  is on. Turn REST authentication on to use sized groups on such a farm.

### Sized groups with broker authentication off

On a farm with `nats.auth.enabled: false` (the default) there is no enrolment,
so a worker names its group in its own configuration:

```yaml
worker:
  capacity_group: "cloud-spot"
```

(`SQI_WORKER_CAPACITY_GROUP` sets the same thing; see
[`worker.capacity_group`](worker-configuration.md#workercapacity_group).) The
group must be a sized group in the worker's farm. The worker joins as an
ephemeral member when it registers. This matches the auth-off trust model: any
worker that can connect is already trusted.

**With broker authentication on, `worker.capacity_group` alone is ignored, by
design.** Otherwise any authenticated worker could make itself ephemeral, and so
deletable. The server logs a warning naming the key and stays out of it; the
worker joins only through its group token.

When a worker's request to join is refused (no such group, another farm, a
named group), the registration still succeeds and the server logs a warning
saying why. The same applies with broker authentication on to a group token
whose group has since been changed to named: its machines no longer join it.

**Removing an ephemeral member does not keep it out.**
`DELETE /capacity-groups/{id}/members/{workerID}` clears the membership, but the
worker joins again at its next registration, which a restart or a NATS
reconnect triggers: through its credential's group with broker authentication
on, or through `worker.capacity_group` with it off. To make a removal stick,
revoke the worker's credential (`DELETE /workers/{id}/credential`), or with
broker authentication off, remove the setting from the worker.

### Build images without a worker ID

A worker creates its ID on first start and keeps it in `worker.data_dir`. **An
image built from a machine that has already run a worker carries that ID, and
every instance booted from it would share one identity.** Build the image
before the worker has ever started, or delete the worker's data directory (and
its `worker.nk` credential file) before taking the snapshot, so each instance
creates its own on first boot. Containers should use a data directory that is
not part of the image (a fresh `emptyDir`, say).

The server warns when an ephemeral member registers as a new process while its
previous process is still online and heartbeating, which is what two machines
sharing an ID look like.

## Worker shutdown and drain

How a worker leaves decides what the server does with it. The worker reports a
**kind** when it deregisters:

| The worker is | Kind | What the server does |
|---|---|---|
| restarted or upgraded (SIGTERM, a service stop) | none | marks it offline and keeps the row and credential; it comes back without a token |
| retired by sqi itself | `retired` | deletes an ephemeral member and revokes its credential. Nothing sends this kind yet: it is for the acting providers, and the server already handles it. |
| reclaimed by the provider (a [preemption notice](#preemption-notices)) | `preempted` | deletes an ephemeral member and revokes its credential |
| gone with no deregister at all (terminated, power cut, preempted without notice) | n/a | the heartbeat sweep marks it offline and reclaims its tasks |

A plain service stop is a restart, not a retirement. Because an autoscaler's
termination hook usually stops the service, an instance scaled in that way is
marked offline, and the capacity sweep then does the rest: an ephemeral member
**offline longer than its group's `boot_timeout_seconds`** (judged by its last
heartbeat) is deleted and its credential revoked, well before the 24 hour
offline-worker retention sweep, which revokes nothing. A member with no recorded
heartbeat is never judged vanished.

`boot_timeout_seconds` can be set as high as a year, but
[`scheduler.offline_worker_retention`](configuration.md#scheduleroffline_worker_retention)
(24 hours by default) deletes an offline, enabled ephemeral member's row
**without revoking its credential**. Keep `boot_timeout_seconds` shorter than the
retention, or revoke such a worker's credential yourself.

Some rules that never bend:

- A **disabled** member is never touched by the sweep: disabling is the
  maintenance hold.
- A member that is not ephemeral (every member of a named group) is never
  deleted by a deregister or by the capacity sweep, and
  `scheduler.offline_worker_retention` skips members of a named group as well.
- The capacity sweep removes nothing while the server is deferring liveness
  decisions after a broker outage, because during an outage no machine can come
  back. A `retired` or `preempted` deregister still deletes its member then: it
  is the machine's own report that it is gone.
- A row is deleted only when the worker is offline and holds no assigned or
  running task.

### Draining, and `shutdown_grace_period`

On SIGINT, SIGTERM or a service stop, a worker stops leasing and gives its
running tasks up to [`worker.shutdown_grace_period`](worker-configuration.md#workershutdown_grace_period)
to finish (default 30 s). Tasks still running at the end get SIGTERM, then
SIGKILL a third of the grace period later, and are reported as
`worker_shutdown`, which the server treats as a reclaim: the task returns to
`ready` and runs again from the start, with no retry consumed and no failure
counted. So the longest a drain can take is the grace period plus a third of it.

**Set `shutdown_grace_period` to your longest render**, and set the autoscaler's
termination hook timeout (below) to at least `grace + grace / 3` plus a margin
for the final status reports and the deregister. If the hook's timeout is
shorter, the cloud terminates the machine mid-render.

Two fixes ship with capacity groups that long drains depend on:

- **Heartbeats continue through a drain.** Previously a worker went silent once
  it began shutting down, so a drain longer than `scheduler.heartbeat_timeout`
  (30 s by default) was swept offline and its still-running tasks reclaimed.
- **A worker waits for its NATS connection to drain on shutdown**, so its final
  statuses and its deregister are actually delivered. A lease that commits for a
  worker that went offline (or whose row was deleted) after the scheduler read
  it online is refused, so a departing worker is not handed new work.

## Preemption notices

Spot and preemptible instances are reclaimed with short warning, long enough
to hand work back but not to finish a render. The worker can watch the
provider's instance metadata service (plain HTTP, no SDK) and react at once:

```yaml
worker:
  preemption:
    watch: aws        # or gcp, azure; default none
    poll_interval: 5s
```

(See [`worker.preemption.watch`](worker-configuration.md#workerpreemptionwatch)
for the exact request each provider receives.) On a notice the worker:

1. stops leasing and cancels its running tasks at once, without waiting out the
   grace period. The tasks are published as `worker_shutdown`, which the server
   treats as a **reclaim, not a failure**: they return to `ready` and the retry
   budget is untouched;
2. deregisters as `preempted`, so the server deletes an ephemeral member and
   revokes its credential, and reschedules the work immediately instead of
   waiting for `heartbeat_timeout`.

Keep in mind:

- The executor still sends the tasks SIGTERM, then SIGKILL after its kill
  window, which is a third of `shutdown_grace_period`. Keep that third **well
  under the provider's notice** (about 2 minutes on AWS, about 30 seconds on GCP
  and Azure). A grace period over 90 seconds gives a kill window over 30
  seconds, longer than a GCP or Azure notice, so workers on preemptible
  instances need a shorter grace period than workers on on-demand instances.
- The metadata service is link-local, so the watcher sends its requests
  directly and never through a proxy named by `HTTP_PROXY` or `HTTPS_PROXY`. To
  reach it some other way, set
  [`worker.preemption.endpoint`](worker-configuration.md#workerpreemptionendpoint).
- A failed poll never stops the watcher. It logs at debug, and warns after 10
  consecutive failures (about 50 seconds at the default interval) that a
  notice would be missed.
- An instance reclaimed with no notice, or a power cut, needs nothing new: the
  heartbeat sweep takes the worker offline and reclaims its tasks.

## Wiring an autoscaler

> **Documentation-derived: not run against a real cloud.** The AWS, GCP, Azure
> and KEDA sections below are written from the providers' documentation and
> sqi's own behaviour. sqi's CI uses no cloud account, so none of this wiring has
> been executed against a real Auto Scaling group, instance group, scale set or
> cluster. Treat the snippets as a starting point and check them against the
> provider's current documentation.

The pattern is the same everywhere:

1. **Scale out to `desired_machines`.** Read `sqi_capacity_desired_machines`
   (or `status.desired_machines`) and set the group's total capacity to it.
2. **Enrol launches with the group token** in the launch template, with
   `worker.preemption.watch` set for spot capacity.
3. **Protect instances from scale-in by default**, and take a machine out only by
   name from `retire_candidates`. sqi cannot map a worker to a cloud instance for
   you in this release (a worker reports its hostname, not its cloud instance
   ID), so the mapping is yours, for example by naming workers after instances
   through `worker.name`.
4. **Stop the worker service from the termination lifecycle hook**, so the
   machine drains instead of dying, with `shutdown_grace_period` and the hook
   timeout set as above.

**There is no server-side retirement check in this release.** `retire_candidates`
is advisory: a candidate was idle when it was evaluated, and may be leased work
before your actuator reaches it. Draining protects that work as long as the
grace period and hook timeout cover the longest task. If you cannot guarantee
that, retire only candidates that are still idle when you act (re-read the
status, or disable the worker first and check it holds no task).

**A failover or restart forgets machines still booting.** The in-memory record of
a recent step-up is lost, so for up to `boot_timeout_seconds` the target can read
lower than before the failover, and an actuator that sets capacity to it could
cancel machines that are still starting. Protect booting instances from scale-in
(the default above does) and the target corrects itself within a step.

### AWS Auto Scaling group

Documentation-derived: not run against a real cloud.

1. **Publish the metric.** sqi does not publish to CloudWatch. Either run a
   small publisher (a Lambda on a schedule, or a cron job on any host that
   reaches the leader) that reads
   `sqi_capacity_desired_machines{group="cloud-spot"}` from `/metrics` and calls
   `aws cloudwatch put-metric-data` with namespace `Sqi`, metric
   `DesiredMachines`, or have the CloudWatch agent's Prometheus metrics
   collection scrape `/metrics`.
2. **Scale out.** The simplest way, and the one that works from zero machines,
   is the same publisher calling `aws autoscaling set-desired-capacity` with the
   value, with the ASG's `MaxSize` set to the group's `max_machines`. Target
   tracking on the ratio `DesiredMachines / GroupInServiceInstances` with a
   target of `1` (a metric-math expression in the policy's customized metric
   specification) also makes capacity follow `desired_machines`, because target
   tracking moves capacity in proportion to metric over target and
   `desired_machines` already counts the machines you have. **It does not work
   for a group that can scale to zero** (`min_machines: 0`, as in the example
   above): with no instance in service the ratio has no value and the policy
   cannot scale out. Use `set-desired-capacity`, or step scaling on
   `DesiredMachines`, for such a group. `GroupInServiceInstances` is published
   only when the ASG's group metrics collection is enabled
   (`aws autoscaling enable-metrics-collection`). Set `DisableScaleIn: true` on
   a target tracking policy so that scale-in stays under your control.
3. **Protect instances.** Create the ASG with `NewInstancesProtectedFromScaleIn`
   so no instance is picked by the ASG's own scale-in. To retire a candidate
   from `retire_candidates`, your script clears the protection of that one
   instance (`aws autoscaling set-instance-protection --no-protected-from-scale-in`)
   and terminates it with
   `aws autoscaling terminate-instance-in-auto-scaling-group
   --should-decrement-desired-capacity`.
4. **Termination lifecycle hook.** Add a lifecycle hook on
   `autoscaling:EC2_INSTANCE_TERMINATING` with a heartbeat timeout of at least
   `shutdown_grace_period` plus a third, plus a margin. A small agent on the
   instance (or the instance's SSM Run Command) sees the hook and runs
   `systemctl stop sqi-worker` (or stops the Windows service), then calls
   `aws autoscaling complete-lifecycle-action` when the service has exited. If
   renders can outlast the maximum hook duration, use
   `record-lifecycle-action-heartbeat` to extend it.
5. **Spot.** Set `worker.preemption.watch: aws`. The worker reads IMDSv2, so the
   instance must allow IMDSv2 requests from the worker (a hop limit of 1 is fine
   outside containers).

### GCP managed instance group

Documentation-derived: not run against a real cloud.

1. **Publish the metric.** Write the value of
   `sqi_capacity_desired_machines{group="cloud-spot"}` to a Cloud Monitoring
   custom metric (for example `custom.googleapis.com/sqi/desired_machines`)
   from a scheduled job, or scrape sqi's `/metrics` with the Ops Agent's
   Prometheus receiver on a host that reaches the server.
2. **Autoscaler policy.** Add a `customMetricUtilizations` entry for that metric
   with `utilizationTargetType: GAUGE` and a `singleInstanceAssignment` of `1`.
   For a metric that is not per-instance, `singleInstanceAssignment` makes the
   autoscaler target `value / 1` instances, so the group follows
   `desired_machines`. Set the policy's `maxNumReplicas` to the group's
   `max_machines`. The autoscaler will also scale in when the target falls, and
   it chooses which instance to remove; the MIG's `scaleInControl` can slow
   that down, and the instance's shutdown handling (next step) is what lets the
   chosen worker drain.
3. **Retiring.** Delete a candidate with
   `gcloud compute instance-groups managed delete-instances` for its instance,
   after it has drained. A shutdown script that stops `sqi-worker` lets a worker
   drain on an instance being deleted, but GCP bounds how long a shutdown script
   may run, so check that limit against your `shutdown_grace_period`.
4. **Spot (preemptible) VMs.** Set `worker.preemption.watch: gcp`. GCP gives
   about 30 seconds of notice, so keep `shutdown_grace_period` short on these
   workers.

### Azure virtual machine scale set

Documentation-derived: not run against a real cloud.

1. **Publish the metric.** Send `desired_machines` and the scale set's current
   instance count to Azure Monitor as a custom metric (for example through the
   Azure Monitor metrics ingestion API or an Application Insights custom
   metric) from a scheduled job.
2. **Autoscale rule.** Azure autoscale rules compare a metric with a threshold.
   Publish the shortfall (`desired_machines` minus the instance count, floored
   at zero) and add a scale-out rule that increases the count when the shortfall
   is above zero, with a cool-down at least as long as a machine takes to boot.
   For an exact total, have the publisher set the scale set's capacity
   (`az vmss scale --new-capacity`) instead of using a rule.
3. **Retiring.** Delete a candidate's instance after it has drained
   (`az vmss delete-instances`). Enable the scale set's terminate notification
   (`scheduledEventsProfile.terminateNotificationProfile`) so that an instance
   being deleted is given time, and have an on-instance handler stop the
   `sqi-worker` service on that event, then approve it.
4. **Spot VMs.** Set `worker.preemption.watch: azure`. The worker reacts to a
   Scheduled Events `Preempt` event only; it ignores reboot, redeploy, freeze
   and terminate events, which is what the handler above is for. Azure gives
   about 30 seconds.

### Kubernetes with KEDA

Documentation-derived: not run against a real cluster.

Run the workers as a Deployment whose pods keep their worker data directory in
an `emptyDir` (so each pod creates its own worker ID) and enrol with the group
token from a Secret. KEDA scales the Deployment's replicas. With a Prometheus
trigger, a threshold of `1` makes the replica count equal the metric:

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: sqi-workers
spec:
  scaleTargetRef:
    name: sqi-worker
  minReplicaCount: 0
  maxReplicaCount: 40          # the group's max_machines
  cooldownPeriod: 600
  triggers:
    - type: prometheus
      metadata:
        serverAddress: http://prometheus.monitoring:9090
        query: sqi_capacity_desired_machines{group="cloud-spot"}
        threshold: "1"
```

Without Prometheus, a `metrics-api` trigger can read the status directly:
`url: http://sqi-server:8080/api/v1/capacity-groups/<id>`,
`valueLocation: status.desired_machines`, `targetValue: "1"`, with an API key
that has `infra.read` supplied through a `TriggerAuthentication` when
`auth.enabled` is on.

Set the pod's `terminationGracePeriodSeconds` to at least `shutdown_grace_period`
plus a third, plus a margin, so Kubernetes lets the worker drain on SIGTERM. A
scale-in removes pods the HPA picks, not `retire_candidates`, so a busy pod may
be chosen and will drain (up to the grace period) before it exits.

## Limits and known gaps

- **Signal only.** sqi launches, wakes, sleeps and terminates nothing in this
  release. There is no wake-on-LAN, no script provider and no first-party cloud
  SDK.
- **No degraded state.** A group whose image never registers is bounded by
  `max_machines`, `max_step_up` and the cooldown but is not backed off; the
  mismatch list and your cloud's own failed-launch metrics are how you notice.
  A degraded state with backoff is planned for the second part (H8b).
- **Sized groups need REST authentication when broker authentication is on**
  ([above](#sized-groups-with-broker-authentication-on)).
- **A removed ephemeral member rejoins** at its next registration unless its
  credential is revoked ([above](#sized-groups-with-broker-authentication-off)).
- **No retirement check.** `retire_candidates` is advisory
  ([above](#wiring-an-autoscaler)).
- **No instance IDs.** A worker does not report its cloud instance ID, so mapping
  a retire candidate to an instance is up to you.
- **Multi-queue workers.** A worker configured with two or more `queue_ids`
  registers with no single queue affinity, and the server offers it work from
  any queue. The signal reads queue affinity the same way the lease path does,
  so it neither over- nor under-counts against that behaviour, but a group with
  a `queue_ids` allow-list may have members that take work from other queues.
- **Idle and pacing state is in memory** and restarts with the server or on a
  failover ([above](#retire-candidates)).
