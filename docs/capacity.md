# Capacity groups

A render farm's demand is spiky. Machines that sit idle cost money, and
machines that are missing during a crunch cost deadlines. A **capacity group**
is how an operator tells sqi which machines may come and go with demand, so that
sqi can publish how many of them the waiting work justifies and, if you let it,
act on that.

This guide is for farm admins and for cloud or devops engineers wiring an
autoscaler or a provider script to sqi.

> **What capacity groups do.** For each group, sqi works out how much waiting
> work the group could run, how many machines it should have and which of its
> machines are safe to give back, and it publishes that as metrics and over
> REST. What happens next depends on the group's **provider**:
>
> - `external`: nothing more. Your own autoscaler (an AWS Auto Scaling group, a
>   GCP managed instance group, an Azure scale set, KEDA, or a script) acts on
>   the signal ([Wiring an autoscaler](#wiring-an-autoscaler)).
> - `wake_on_lan`: sqi wakes switched-off on-premises machines with magic
>   packets and puts idle ones back to sleep or shuts them down
>   ([Wake-on-LAN](#wake-on-lan)).
> - `script:<name>`: sqi runs a program you configure on the server to wake,
>   grow, retire and list the group's machines
>   ([The script provider](#the-script-provider)).
>
> sqi never launches a cloud instance itself: a script provider asks your own
> tool (the cloud's CLI, a BMC) to do it. Whatever the provider, sqi also cleans
> up: a cloud machine that left for good is deleted from the worker list and its
> credential revoked ([below](#worker-shutdown-and-drain)).

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
| `provider` | `external` | `external`, `wake_on_lan` (named groups only) or `script:<name>`, where `<name>` is defined under [`capacity.providers`](configuration.md#capacityproviders) in the server's configuration ([Choosing a provider](#choosing-a-provider)) |
| `enabled` | `true` | a disabled group is not evaluated, exports nothing, and its members count as ordinary workers |
| `queue_ids` | `[]` (every queue in the farm) | up to 1000 queues, all in the group's farm. Only ready work in these queues drives the group. This is the main lever for keeping a low-priority backlog from launching paid machines. |
| `min_machines` | `0` | 0 up to `max_machines` |
| `max_machines` | required | 1 to 10000; a hard cap on the machines running. A member that is being retired and is still online counts toward it until it is gone. |
| `priority` | `0` | higher is preferred when groups compete for the same work |
| `max_step_up` | `1` | 1 to 10000; the most machines one evaluation may add |
| `scale_up_cooldown_seconds` | `60` | 0 up to one year; the shortest time between step-ups |
| `idle_timeout_seconds` | `600` | 60 up to one year; how long a member must sit idle before it is a retire candidate |
| `boot_timeout_seconds` | `600` | 60 up to one year; how long a machine that was asked for has to register, and how long a vanished cloud machine is kept ([below](#worker-shutdown-and-drain)). |
| `retire_action` | `sleep` | `sleep` or `shutdown`: what a `wake_on_lan` group tells an idle member to do when it retires it. Other providers ignore it (a script decides for itself what retiring means). |
| `machine_profile` | `{}` | a sized group must declare `cpu_count` of at least 1 |

The profile may declare `cpu_count`, `ram_mb`, `gpu_count`, `gpu_memory_mb`,
`os`, `arch`, `compute_location`, `tags`, `extensions` and `expr_limits`. A
field you leave out is not declared, and a machine offering more than the
profile declares is not a mismatch.

### Choosing a provider

| Provider | Group kinds | What sqi does |
|---|---|---|
| `external` (the default) | named, sized | publishes the signal; something outside sqi acts on it |
| `wake_on_lan` | named | wakes offline members with Wake-on-LAN magic packets, and tells idle members to sleep or shut down |
| `script:<name>` | named, sized | runs the program configured as `capacity.providers.<name>`: `wake` and `retire` named members, `grow`, `retire` and `list` a sized group |

`GET /api/v1/capacity-providers` (`infra.read`) lists what a group on this
server may name, with the kinds each serves. It returns names and kinds only,
never a script's command or environment:

```json
{
  "providers": [
    { "name": "external", "kinds": ["named", "sized"] },
    { "name": "wake_on_lan", "kinds": ["named"] },
    { "name": "script:aws-render", "kinds": ["named", "sized"] }
  ]
}
```

A sized group naming `wake_on_lan` is refused with 400, and so is a group
naming a script the server's configuration does not define. A script provider
removed from the configuration later does not break the groups that name it:
the scheduler records `provider script:<name> is not configured on this server`
as the group's last action, logs it once, and acts on nothing, and a `PUT` that
keeps the group's stored provider is still accepted, so the group can be
disabled or edited. Switching a group *to* an unconfigured provider is refused.

The two acting providers need two opt-ins before sqi stops a
machine: on the server, the group must name `wake_on_lan` or a script; for
`wake_on_lan`, each worker must also set
[`worker.power.allow_remote_sleep`](worker-configuration.md#workerpowerallow_remote_sleep)
or it refuses to sleep or shut down. A script stops machines through your own
tool, so its second opt-in is the program you wrote and configured.

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
system version field, so a step requiring `attr.worker.os.version` never
counts toward a sized group. A named group is judged from each member's own
registration, which does carry its version.

Work is then taken off the demand before any group sees it:

1. **Idle workers absorb first.** Workers outside any group are consulted
   first, then each group's online members, then each named group's members
   that are waking (asked to start and not registered yet), with all their
   cores, so a machine already on its way is not asked for twice. A worker's
   free cores are spent once, across all demand, in the order the lease path
   would offer the work (job priority, then job creation, then step order). A
   member that is being retired absorbs nothing.
2. **Groups take what is left, by priority.** The highest-priority group able to
   run a piece of work takes it, up to the machines it could still add; what it
   cannot take passes to the next group. This gives "local before cloud" as
   configuration.

A step that declares no per-task core count reserves a whole machine, as the
lease path does.

### The desired size

`desired_machines` is a total target. It counts the machines already
present, including the retire candidates, which are listed separately. The
machines present are the **active** members (online and not being retired) and,
in a named group, the members that are waking:

- If the group's demand calls for `need` more machines, the target is
  `active + waking + need`, limited as below. `need` is the group's
  `demand_cores` divided by the cores one machine brings, rounded up.
- If nothing is needed, the target shrinks to the machines still useful: the
  active members that are not retire candidates.
- It is never below `min_machines` nor above `max_machines`. A member that is
  being retired and is still online counts toward `max_machines`, which caps
  the machines running, and that cap wins over `min_machines` while the
  retirement runs.

With an acting provider sqi reads the target for you: a named group wakes its
`wake_candidates` by name, and a sized script group asks its provider for
`desired_machines` ([The script provider](#the-script-provider)). It never acts
on a count alone, so a target below the machines running (a group over its
`max_machines`, say) retires nothing by itself. With `external`, how to act on
it depends on your actuator:

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
  measured from registered machines. If an image never registers, or a woken
  machine never comes up, the group does not compound: it costs at most one
  step.
- **`scale_up_cooldown_seconds`** is the shortest time between step-ups. Set it
  at least as long as a machine takes to boot, or the group will ask for another
  step while the first is still starting.

Machines a step-up asked for that have not registered yet are held in
`desired_machines` for `boot_timeout_seconds`, so the target does not fall back
while they boot; a named group's waking members are held until they register or
their wake fails. After the timeout they are no longer held, and the group may
ask for one further step, measured from the machines that did register.

An `external` group has no degraded state with backoff for a broken image, so it
never stops asking: it asks for one step per boot timeout. The guards above
limit the cost, and a mismatch report (next section) is how you notice. A sized
group with a script provider does back off: see
[Degraded groups](#degraded-groups). A named group backs off per machine: a
member whose wake failed is not woken again for two boot timeouts.

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

A member is a retire candidate when it is online, awake, not disabled, holds
no task, has been idle for at least `idle_timeout_seconds`, and absorbed none of
this evaluation's demand. The list is longest idle first, and retiring all of it
never leaves fewer active members than `min_machines`.

Idle time is sampled once per sweep tick and kept in memory. After a server
restart or a high-availability failover the new leader starts counting again, so
no machine is offered for retirement sooner than `idle_timeout_seconds` after
the leader first sees it idle. The same loss applies to machines still booting
(see below). A change of power state restarts it too, so a member whose
retirement was refused or ran out of time is retried at most once per
`idle_timeout_seconds`.

### Wake candidates

A named group also lists **wake candidates**, which a `wake_on_lan` or script
group wakes and which are advice under `external`, like the retire candidates.
They are its members that are offline, not disabled, and awake, asleep, or
`wake_failed` for at least twice `boot_timeout_seconds`. Members that could run
the work the group took come first: the smallest machine first (one reporting
no cores last), then the one down longest. sqi wakes as many as
`desired_machines` leaves room for beside the active and waking members. After
them, other wakeable members are woken only to make up `min_machines`: demand
never wakes a machine that could run none of it.

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
  "provider": "script:aws-render",
  "max_machines": 40,
  "status": {
    "demand_cores": 192,
    "desired_machines": 14,
    "online_members": 6,
    "offline_members": 1,
    "waking": 0,
    "retiring": 0,
    "asleep": 0,
    "wake_failed": 0,
    "wake_candidates": [],
    "retire_candidates": [],
    "mismatches": [],
    "requested_machines": 14,
    "degraded": null,
    "last_action": {
      "action": "grow",
      "result": "ok",
      "message": "provider script:aws-render carried out the grow",
      "at": "2026-11-08T10:15:30Z"
    },
    "evaluated_at": "2026-11-08T10:15:30Z"
  }
}
```

| Field | Meaning |
|---|---|
| `waking`, `retiring`, `asleep`, `wake_failed` | members in each [power state](#power-states), online or offline, disabled or not |
| `wake_candidates` | a named group's members to wake now, in wake order ([above](#wake-candidates)); always empty for a sized group |
| `requested_machines` | the size a sized script group last asked its provider for ([Degraded groups](#degraded-groups)); `null` for every other group |
| `degraded` | `null`, or why a sized script group stopped growing: `reason`, `since` and `backoff_until` |
| `last_action` | `null`, or the group's latest provider action: `action` (`wake`, `grow`, `retire`, `list` or `configure`), `result` (`ok`, `failed`, `refused`, `unsupported`, `timeout` or `expired`), `message` and `at` |

`evaluated_at` is when the figures were computed. If the server cannot read the
store, it keeps the last evaluation and its gauges as they were rather than
publishing zeros, so a stale `evaluated_at` is how you tell a quiet farm from a
blind server. Reading the status needs `infra.read` when `auth.enabled` is on.

`GET /api/v1/capacity-groups/{id}/members` lists a group's workers in the shape
`GET /workers` returns. Every worker there carries `power_state` (`awake` for a
worker no group has moved) and `has_wake_address` (whether it reported an
interface with a 6-byte MAC and an IPv4 address), and, when set, `power_state_at`, `power_reason` (why it
last changed, such as a failed wake) and `cloud_instance_id`. The interfaces
themselves are in the worker's detail, `GET /api/v1/workers/{id}`.

### Metrics

`GET /metrics` on `sqi-server` carries three gauges per enabled group, and four
more families for each group whose provider acts. The label `group` is the
group name. Only the leader exports them (a standby has no `sqi_capacity_*`
series; see [above](#the-status)), and `/metrics` is never gated by
authentication.

| Metric | Labels | Meaning |
|---|---|---|
| `sqi_capacity_demand_cores` | `group` | cores of ready work the group could run that no idle worker absorbed and no higher-priority group took |
| `sqi_capacity_desired_machines` | `group` | the total machines the group should have, counting the machines it already has (not the number to add) |
| `sqi_capacity_members` | `group`, `state` | the group's members by liveness; `state` is `online` or `offline`, and a disabled member counts in whichever it is |
| `sqi_capacity_machines` | `group`, `power_state` | an acting group's members by power state: `awake`, `waking`, `retiring`, `asleep`, `wake_failed` |
| `sqi_capacity_actions_total` | `group`, `action`, `result` | counter of provider actions: `action` is `wake`, `grow`, `retire` or `list`; `result` is `ok`, `failed`, `refused`, `unsupported`, `timeout` or `expired` (a retirement the boot timeout ended) |
| `sqi_capacity_boot_timeouts_total` | `group` | counter of woken machines that did not register within the group's boot timeout |
| `sqi_capacity_group_degraded` | `group` | `1` while the group is [degraded](#degraded-groups), `0` otherwise |

The power-state and degraded gauges exist only for a group whose provider acts,
which includes a group naming a script provider this server does not configure.
They are removed when a group is switched to `external`. A group's gauges
disappear when it is disabled, renamed or deleted. The two counters are
removed with them, and kept when a group is switched to `external`. A disabled
group, or one switched to `external`, can still count a wake or retirement that
the boot timeout settles or that a provider call still running finishes, so its
counters continue, or reappear after a disable; after a rename or a deletion
they do not. See also [Observability](observability.md).

`sqi_scheduler_queue_depth{queue}` (the label is the queue ID) falls back to `0`
when a queue drains, so a scaling signal built on it falls too. Only a queue the
scheduler has seen with ready work since it started is reset.

### Waiting tasks

A ready task that no online worker can run is
[flagged unschedulable](observability.md#why-isnt-my-job-running--unschedulable-tasks).
When a group whose provider acts could bring a machine that runs it, the reason
names the group instead: `waiting for capacity group night-nodes to wake a
machine` for a named group with a member it is waking or could wake, and
`waiting for capacity group cloud-spot to launch a machine` for a sized group.
The check is on capability only: a group already at `max_machines` still reads
"to launch a machine". A sized group that is degraded and backing off is left
out until its backoff ends.

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
group by enrolling with the group's group token, which makes it an
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

- Group tokens are reusable: any number of machines may enrol with one,
  whatever `nats.auth.join_token_single_use` says. They are tied to one sized
  group.
- Expiry defaults to 720 hours (30 days) and may be at most 2160 hours
  (90 days). Because a leaked reusable token is useful until it expires, rotate:
  mint the new token, update the launch template, and revoke the old one when no
  template refers to it. Both are valid meanwhile.
- Revoking a token (`DELETE /capacity-groups/{id}/tokens/{tokenID}`) stops
  new enrolments. It does not revoke credentials already issued from it:
  revoke those machines with `DELETE /workers/{id}/credential`.
- `GET /capacity-groups/{id}/tokens` lists a group's tokens (prefix, expiry,
  revocation; never the secret). Minting, listing and revoking need
  `infra.manage`.
- Minting needs REST `auth.enabled`: with it off the route is not mounted, and a
  `POST` is answered `405`. With REST auth on but `nats.auth.enabled` off, it is
  answered `409` with `group tokens need nats.auth.enabled; workers on an
  auth-off farm join with worker.capacity_group`. A named group answers `409`
  too, since its members are added by name.
- With REST `auth.enabled: false` and `nats.auth.enabled: true`, no machine can
  join a sized group. The mint route is not mounted,
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

With broker authentication on, `worker.capacity_group` alone is ignored:
otherwise any authenticated worker could make itself ephemeral, and so
deletable. The server logs a warning naming the key and stays out of it; the
worker joins only through its group token.

When a worker's request to join is refused (no such group, another farm, a
named group), the registration still succeeds and the server logs a warning
saying why. The same applies with broker authentication on to a group token
whose group has since been changed to named: its machines no longer join it.

An ephemeral member cannot be removed from its group. It would join again
at its next registration, which a restart or a NATS reconnect triggers: through
its credential's group with broker authentication on, or through
`worker.capacity_group` with it off. So
`DELETE /capacity-groups/{id}/members/{workerID}` answers 409 for one. To retire
the machine, stop the worker and remove it with `DELETE /workers/{id}`, which
also revokes its credential; with broker authentication off, also remove
`worker.capacity_group` from its configuration before it starts again.

### Build images without a worker ID

A worker creates its ID on first start and keeps it in `worker.data_dir`. An
image built from a machine that has already run a worker carries that ID, and
every instance booted from it shares one identity. Build the image
before the worker has ever started, or delete the worker's data directory (and
its `worker.nk` credential file) before taking the snapshot, so each instance
creates its own on first boot. Containers should use a data directory that is
not part of the image (a fresh `emptyDir`, say).

The server warns when an ephemeral member registers as a new process while its
previous process is still online and heartbeating, which is what two machines
sharing an ID look like.

## Worker shutdown and drain

How a worker leaves decides what the server does with it. The worker reports a
kind when it deregisters:

| The worker is | Kind | What the server does |
|---|---|---|
| restarted or upgraded (SIGTERM, a service stop) | none | marks it offline and keeps the row and credential; it comes back without a token |
| retired by sqi itself (it accepted a `sleep` or `shutdown`; [Power control on the worker](#power-control-on-the-worker)) | `retired` | finishes the retirement: a named member becomes `asleep` and stays a member; an ephemeral member is deleted and its credential revoked. Any deregistration of a member sqi is retiring finishes it the same way, whatever its kind. |
| reclaimed by the provider (a [preemption notice](#preemption-notices)) | `preempted` | deletes an ephemeral member and revokes its credential |
| gone with no deregister at all (terminated, power cut, preempted without notice) | n/a | the heartbeat sweep marks it offline and reclaims its tasks |

A plain service stop is a restart, not a retirement. Because an autoscaler's
termination hook usually stops the service, an instance scaled in that way is
marked offline, and the capacity sweep then does the rest: an ephemeral member
offline longer than its group's `boot_timeout_seconds` (judged by its last
heartbeat) is deleted and its credential revoked. A member with no recorded
heartbeat is never judged vanished.

`boot_timeout_seconds` is also how long an ephemeral member may be out of
touch. The sweep cannot tell a terminated machine from one that is stopped,
hibernated or cut off from the server by a network outage (a site-to-cloud VPN
going down, say). A machine that comes back after its member was deleted has
a revoked credential, which the broker refuses, and a worker keeps its
credential file rather than enrolling again with its image's token: it never
reconnects, and sqi no longer counts it. Replace such a machine, or set
`boot_timeout_seconds` to cover the longest stop or outage the group must ride
out. Only an outage of the server's own broker is waited out for you.

The capacity sweep is the only automatic way an ephemeral member is deleted:
[`scheduler.offline_worker_retention`](configuration.md#scheduleroffline_worker_retention)
skips every capacity-group member, because its deletion revokes nothing. A
vanished member's row and credential therefore last exactly as long as its
group's `boot_timeout_seconds`, however that compares with the retention.

The sweep follows these rules:

- A disabled member is never touched by the sweep: disabling is the
  maintenance hold.
- A member that is not ephemeral (every member of a named group) is never
  deleted by a deregister or by the capacity sweep, and
  `scheduler.offline_worker_retention` skips every capacity-group member.
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

Set `shutdown_grace_period` to your longest render, and set the autoscaler's
termination hook timeout (below) to at least `grace + grace / 3` plus a margin
for the final status reports and the deregister. If the hook's timeout is
shorter, the cloud terminates the machine mid-render.

Two behaviours let long drains work:

- Heartbeats continue through a drain, so a drain longer than
  `scheduler.heartbeat_timeout` (30 s by default) is not swept offline and its
  running tasks are not reclaimed.
- A worker waits for its NATS connection to drain on shutdown, so its final
  statuses and its deregister are delivered. A lease that commits for a worker
  that went offline (or whose row was deleted) after the scheduler read it
  online is refused, so a departing worker is not handed new work.

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
   treats as a reclaim, not a failure: they return to `ready` and the retry
   budget is untouched;
2. deregisters as `preempted`, so the server deletes an ephemeral member and
   revokes its credential, and reschedules the work immediately instead of
   waiting for `heartbeat_timeout`.

Keep in mind:

- The executor still sends the tasks SIGTERM, then SIGKILL after its kill
  window, which is a third of `shutdown_grace_period`. Keep that third well
  under the provider's notice (about 2 minutes on AWS, about 30 seconds on GCP
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
- An AWS notice whose action is `stop` or `hibernate` is not treated as a
  reclaim: the instance keeps its disk, so its worker ID and credential come
  back with it. The worker shuts down as the operating system stops it, like a
  restart, and stays a member. It must be running again within the group's
  `boot_timeout_seconds` ([above](#worker-shutdown-and-drain)), or it is deleted
  and its credential revoked.

## Acting providers

With `wake_on_lan` or a script provider, the leader acts on every evaluation in
the same sweep tick: it wakes the wake candidates, retires the retire
candidates, and grows a sized script group. It acts only on machines the
evaluation named, or, for a sized group, through the size it records (below),
never on a count alone.

### Power states

Every worker has a `power_state`. A worker that no acting group has touched is
`awake`.

| State | Meaning | New work leased? |
|---|---|---|
| `awake` | the normal state, online or offline | yes, while online and enabled |
| `waking` | sqi asked the machine to start, and its worker has not registered yet | no (it is offline) |
| `retiring` | sqi asked the machine to sleep, shut down or be removed, and it has not gone yet | no: refused as for a disabled worker |
| `asleep` | a named member whose retirement finished; it is offline | no |
| `wake_failed` | a woken machine whose worker did not register within `boot_timeout_seconds` | no (it is offline) |

How a member moves between them:

```text
awake        --(wake candidate: offline, not disabled)------------> waking
asleep       --(wake candidate)-----------------------------------> waking
wake_failed  --(wake candidate, 2 x boot_timeout after failing)---> waking
waking       --(no registration within boot_timeout)--------------> wake_failed
waking,      --(the worker registers, however it was woken)-------> awake
asleep,
wake_failed
awake        --(retire candidate: online, idle, holding no task)--> retiring
retiring     --(it deregisters, or the script answers ok)---------> asleep (a named member), or
                                                                    deleted, credential revoked (an ephemeral member)
retiring     --(refused, no reply, or a script answer not ok)-----> awake
retiring     --(not finished within boot_timeout)-----------------> awake
any state    --(the worker leaves its group)----------------------> awake
```

A worker that registers while it is `retiring` stays retiring. The rules that
hold throughout:

- **A disabled member is never woken, retired or sent anything.** Disabling a
  worker is the maintenance hold, as for the signal.
- **Retiring and asleep members are leased nothing.** The lease path refuses
  them as it refuses a disabled worker, so a retiring worker that keeps asking
  for work until its machine suspends gets none.
- **Only a registration wakes a member.** A heartbeat changes no power state, so
  a late heartbeat from an asleep or offline worker does not bring it back.
- **A retirement starts only on an idle machine.** Moving a member to
  `retiring` is a compare-and-set against the lease, under the same row lock a
  lease takes: it is refused while the member holds an assigned or running
  task. A lease that lands between the evaluation choosing the member and the
  retirement starting therefore keeps the member awake, and no command is sent.
- **A retirement never finishes under a task.** If a retiring member still holds
  a task when it deregisters, the finish is refused and the member stays
  retiring until the boot timeout puts it back to `awake`.

### Recorded intent, then action

sqi writes what it is about to do to the store before it does it: a wake moves
the members to `waking`, a retirement moves the member to `retiring`, and a
sized group's grow records the size it asks for (its launch record). Only then
is the provider called, on a goroutine of its own, at most one call per group at
a time. While a group's call runs, sqi records no new wake or retirement for it:
the request could not be sent before the call ends, and a recorded transition
starts its boot timeout at once. Its results are applied through writes fenced
like every leader write, so a server that has lost the leadership meanwhile
changes nothing.

Every request is absolute: wake this machine, sleep that one, make the group 14
machines in all. Sending one twice is harmless, and that is what a new leader
does: after a restart or a [failover](operations.md#running-more-than-one-server-high-availability),
it sends each intent it finds recorded (a waking or retiring member, a launch
not yet met) once, whatever its predecessor managed to do. A worker told to
sleep while it is already retiring answers `already retiring`, which sqi takes
as success.

The timeouts below settle whatever a call left behind. They run every tick for
every group of the farm, enabled or not and whatever its provider, so a group
disabled or switched to `external` in the middle of a transition does not leave
a member stuck.

### Timeouts and backoff

| What | What happens |
|---|---|
| a wake whose worker has not registered after `boot_timeout_seconds` | the member becomes `wake_failed` (counted in `sqi_capacity_boot_timeouts_total`) |
| a `wake_failed` member | may be woken again two boot timeouts after it failed |
| a retirement not finished after `boot_timeout_seconds` | aborted: the member is `awake` again (counted as `retire` with result `expired`) |
| a retirement refused, unanswered or aborted | the member's idle time restarts, so it is retried at most once per `idle_timeout_seconds` |
| woken machines that never register | cost at most one step: growth is paced by `max_step_up` from registered machines |
| a sized script group's launch that does not register | the group degrades and stops growing for two boot timeouts ([Degraded groups](#degraded-groups)) |
| a wake-on-LAN relay that does not answer | after 5 s the server sends the packets itself |
| a worker's reply to `sleep` or `shutdown` | 10 s; no reply is a `timeout`, and the member is `awake` again at once |
| a script run | the provider's `timeout`: 2 minutes by default, 5 s to 30 minutes |

### Shutdown and failover

When `sqi-server` stops, or a leader hands over, it waits up to 20 s for the
provider calls still running, inside the server's 30 s shutdown budget (see
[Graceful shutdown](operations.md#graceful-shutdown)). A call still running then
is cut off: its context is canceled, so a store write or a script it is still
running stops, its results are not written, and it starts no further action.
Nothing is lost by that. Its request was recorded before it was made, so the
next leader sends it again, or the boot timeout settles it. A call that runs
longer than 20 s (any script on the default 2-minute timeout can, as can a call
that chains a list and a retire) can therefore be cut off at shutdown, and the
script must cope with being run again.

## Wake-on-LAN

`wake_on_lan` is for named groups of on-premises machines that are normally
asleep or switched off. When work they can run is waiting, sqi wakes them with
magic packets; when they have sat idle for `idle_timeout_seconds`, it tells them
to sleep or shut down, as the group's `retire_action` says.

```sh
curl -s -X POST "$BASE/capacity-groups" \
  -H 'Content-Type: application/json' \
  -d '{
    "farm_id": "'"$FARM_ID"'",
    "name": "night-nodes",
    "kind": "named",
    "provider": "wake_on_lan",
    "max_machines": 20,
    "retire_action": "sleep",
    "idle_timeout_seconds": 1800,
    "boot_timeout_seconds": 300
  }'
```

Then add the machines as for any [named group](#named-groups), and set
`worker.power.allow_remote_sleep: true` on each of them. Work through the
[setup checklist](#setup-checklist) before relying on it: most failed wakes are
a firmware or network-card setting.

### How a wake reaches a machine

Each time a worker registers it reports its network interfaces: those that are
up, not loopback, and have a hardware address, with their addresses. It leaves
out the host side of container and VM networking (interfaces named `veth*`,
`docker*`, `br-*`, `cni*`, `flannel*`, `cali*`, `vxlan*`, `virbr*`, `vmnet*`,
`vboxnet*`, `lxcbr*`, `lxdbr*`, `podman*`, `kube-*` or `weave*`), which cannot
carry a wake. A plain bridge such as `br0` and a Hyper-V `vEthernet` switch are
kept, since on a hypervisor host they hold the machine's own address. sqi
addresses a wake from that record: every distinct 6-byte MAC address of an
interface that has an IPv4 address, aimed at the IPv4 network of the interface
carrying the worker's registered IP address (or, failing that, the first IPv4
address recorded). A registration that reports no interfaces keeps the ones
already stored. A machine that has never registered while awake has nothing
recorded, and its wake fails with `no wake-on-LAN address recorded; the worker
must register once while awake`.

Magic packets are broadcasts and do not cross routers, so sqi tries two ways in
order:

1. **Through a relay.** The first worker, by ID, that is online, enabled, awake,
   in the same farm and has an interface on the target's network is asked, on
   its control subject, to broadcast the packets there: to that network's
   directed broadcast address and to `255.255.255.255`. Any such worker may
   relay, whether or not it belongs to a group; relaying needs no opt-in.
2. **From the server**, when there is no relay or it has not accepted within
   5 s: to `255.255.255.255`, to the target network's directed broadcast
   address, and to every
   [`capacity.wake_on_lan.send_to`](configuration.md#capacitywake_on_lansend_to)
   entry, all on
   [`capacity.wake_on_lan.port`](configuration.md#capacitywake_on_lanport)
   (UDP 9 by default).

No worker needs to be awake for the second way: on a flat network the server's
own broadcast reaches the machine. For machines on a routed network the server
is not on, and that may have no awake worker to relay, add a `send_to` entry:
the router's directed broadcast address for that network (`192.168.20.255:9`),
if the router forwards directed broadcasts, or a host there that forwards the
packets. `send_to` takes IPv4 entries only, as packets are sent over IPv4; the
configuration refuses anything else. Each packet is sent three times, 100 ms
apart. A target on a `/31` or `/32` network has no directed broadcast address,
so for one of those only `255.255.255.255` and `send_to` are tried.

A wake reported sent is not a machine that woke. sqi counts a machine as awake
only when its worker registers, and one that has not within
`boot_timeout_seconds` becomes `wake_failed`. A wake from the server counts as
sent when at least one of its targets could be sent to; the targets that could
not are logged at Warn. It is recorded as `failed` only when every send failed,
and the member then stays `waking` for the boot timeout to settle.

A relay accepts a wake before it sends anything, so a send that then fails on
the relay is only logged there, in the relay worker's own log: the server has
already recorded the wake as sent.

### Power control on the worker

A worker answers power requests the server sends it on `worker.control.<workerID>`.

| Request | Refused when | What the worker does |
|---|---|---|
| `sleep`, `shutdown` | it is running a task (`busy`), or `worker.power.allow_remote_sleep` is `false` (the default), or the platform has no command for it, or it is carrying out one already (`already retiring`) | replies, deregisters as `retired`, then runs the command |
| `wake` | no valid 6-byte MAC address, or no target | sends Wake-on-LAN broadcasts for the addresses to the targets, on the server's behalf |

`wake` has no opt-in because a worker acting as a relay only sends UDP
broadcasts. `sleep` and `shutdown` do, because they stop the machine, and they
are refused whatever the setting while the worker runs a task. A refusal names
its reason (`busy`, `worker.power.allow_remote_sleep is false`, or the missing
command), which becomes the group's last action, and the member is `awake`
again at once, as it is when no reply comes within 10 s. `already retiring` is
the exception: the command is under way, so sqi takes it as accepted.

A request that reaches no one (no worker listens on the member's control
subject) is handled according to which server began the retirement. When this
server began it, the request was its first: nothing was delivered, so the retirement is refused with
`worker is not listening on its control subject` and the member is `awake`
again at once. A request this server only repeats, for a retirement recorded
before it started (after a restart or a failover), leaves the member `retiring`
instead: the worker may have carried out the first request and be on its way
down, so sqi waits for its deregistration or the boot timeout.

A member put back to `awake`, by a refusal or by the boot timeout, takes work
again at once: a lease request it made while it was retiring is answered then,
not left to wait out its hold.

A disable that lands in the few seconds between sqi moving a member to
`retiring` and sending the request does not stop the request, so the command
can still reach a member just disabled. A worker process that restarts while
its member is `retiring` registers as a new process, and the member keeps the
state: the request already sent is not sent again, so the member stays idle
until the boot timeout puts it back to `awake`.

`worker.power.sleep_command` and `worker.power.shutdown_command` are argv
lists, run with no shell, set in the config file only. Left empty they use the
platform's own: `systemctl suspend` / `systemctl poweroff` on Linux,
`pmset sleepnow` / `shutdown -h now` on macOS, and a PowerShell suspend call /
`shutdown.exe /s /t 0` on Windows. Other platforms have none, so a request is
refused until one is configured. `shutdown -h now` needs root, and the
Linux commands need permission to suspend or power off, so a worker that is not
root needs a polkit rule or its own command (see the
[setup checklist](#setup-checklist)). A sudoers rule does not work under the
[example unit](worker-deployment.md#linux--systemd): its `NoNewPrivileges=yes`
stops `sudo` from gaining privileges.

A worker that suspends comes back by itself: it registers again when it sees
the machine wake, and in any case 60 seconds after the command returned. A
command that fails to start or exits non-zero makes it register again at once.
A `shutdown` that works ends the process with the machine. One whose command
exits 0 but leaves the machine running is treated like a sleep that did not
take: the worker registers again 60 seconds after the command returned.

### Setup checklist

Do this on each machine before adding it to a `wake_on_lan` group.

1. **Firmware.** Enable Wake-on-LAN in the BIOS or UEFI setup (it may be called
   "Wake on LAN", "Power On by PCI-E" or "Resume by LAN"). To wake a machine from
   shutdown, also turn off any "ErP" or "EuP" low-power mode, which cuts the
   network card's standby power.
2. **Linux.** Check the card with `ethtool <nic>`: `Wake-on: g` means it wakes
   on a magic packet. Turn it on with `sudo ethtool -s <nic> wol g`. That lasts
   until the next boot, so make it persistent, with either:
   - a systemd `.link` file. It replaces the distribution's default for the
     card, so keep the default's naming lines:

     ```ini
     # /etc/systemd/network/50-wol.link
     [Match]
     MACAddress=aa:bb:cc:dd:ee:ff

     [Link]
     NamePolicy=kernel database onboard slot path
     MACAddressPolicy=persistent
     WakeOnLan=magic
     ```

     A `.link` file takes effect only when udev next processes the card: reboot,
     or run `sudo udevadm trigger --action=add /sys/class/net/<nic>`. On a host
     configured with netplan, netplan's own generated link file wins over
     yours, so set `wakeonlan: true` on the interface in the netplan
     configuration instead and run `sudo netplan apply`.
   - or, on a machine whose network NetworkManager manages:
     `sudo nmcli connection modify <connection> 802-3-ethernet.wake-on-lan magic`.
3. **Windows.** In Device Manager, open the network adapter's properties. On the
   **Power Management** tab tick "Allow this device to wake the computer" and
   "Only allow a magic packet to wake the computer"; on the **Advanced** tab
   enable "Wake on Magic Packet" where the driver lists it. To wake from
   shutdown, turn fast startup off (Control Panel, Power Options, "Choose what
   the power buttons do", untick "Turn on fast startup"): with it on, a shutdown
   is a hibernation many cards do not wake from.
4. **macOS.** Turn on "Wake for network access" in System Settings (Battery or
   Energy options), or `sudo pmset -a womp 1`. It wakes a sleeping Mac, so give
   Macs `retire_action: sleep`.
5. **Let the worker sleep and shut the machine down.** Set
   `worker.power.allow_remote_sleep: true`, and give the worker the privilege
   its command needs:

   | OS | Default `sleep` | Default `shutdown` | Privilege |
   |---|---|---|---|
   | Linux | `systemctl suspend` | `systemctl poweroff` | a polkit rule for the worker's service user (below) |
   | macOS | `pmset sleepnow` | `shutdown -h now` | `shutdown` needs root: set `worker.power.shutdown_command` to a command the worker's user may run (a `sudo -n` rule, say) |
   | Windows | PowerShell `SetSuspendState('Suspend', ...)` | `shutdown.exe /s /t 0` | the service's default LocalSystem account may do both |

   On Linux, a service user may not suspend or power off the machine without a
   polkit rule. For the `sqiworker` user of the
   [example unit](worker-deployment.md#linux--systemd):

   ```js
   // /etc/polkit-1/rules.d/50-sqi-worker-power.rules
   polkit.addRule(function (action, subject) {
       if (subject.user == "sqiworker" &&
           (action.id == "org.freedesktop.login1.suspend" ||
            action.id == "org.freedesktop.login1.suspend-multiple-sessions" ||
            action.id == "org.freedesktop.login1.suspend-ignore-inhibit" ||
            action.id == "org.freedesktop.login1.power-off" ||
            action.id == "org.freedesktop.login1.power-off-multiple-sessions" ||
            action.id == "org.freedesktop.login1.power-off-ignore-inhibit")) {
           return polkit.Result.YES;
       }
   });
   ```

   The two `-ignore-inhibit` actions matter on a machine with a desktop
   session: a session's inhibitor (a screen lock, a media player) makes
   `systemctl suspend` and `systemctl poweroff` fail unless the rule allows
   them too. A minimal server install may not have polkit at all: install
   `polkitd` (the `polkit` package on older releases). JavaScript rules need
   polkit 0.106 or later; Ubuntu 22.04 and Debian 11 ship 0.105, which reads
   `.pkla` files instead:

   ```ini
   # /etc/polkit-1/localauthority/50-local.d/50-sqi-worker-power.pkla
   [sqi worker power]
   Identity=unix-user:sqiworker
   Action=org.freedesktop.login1.suspend;org.freedesktop.login1.suspend-multiple-sessions;org.freedesktop.login1.suspend-ignore-inhibit;org.freedesktop.login1.power-off;org.freedesktop.login1.power-off-multiple-sessions;org.freedesktop.login1.power-off-ignore-inhibit
   ResultAny=yes
   ResultInactive=yes
   ResultActive=yes
   ```

   Check it with `sudo -u sqiworker systemctl suspend`. That suspends
   the machine, so run it where you can wake the machine again.
6. **Register once while awake.** Start the worker with its network up and let
   it register, so the server records its MAC address. `GET /api/v1/workers/{id}`
   should list an interface with a `mac` and an IPv4 address in `addrs`.
7. **Try it.** Put the machine to sleep or shut it down by hand, then wake it
   with any Wake-on-LAN tool from another machine on its network. If that does
   not wake it, sqi cannot either.

## The script provider

A script provider is a program you configure on the server, under
[`capacity.providers`](configuration.md#capacityproviders), and a group
names as `script:<name>`. sqi runs it once per action with a JSON request on
standard input and reads a JSON response on standard output. It suits anything
sqi has no built-in provider for: a cloud's instance group driven through its
CLI, BMCs, a power strip, an in-house tool.

```yaml
capacity:
  providers:
    aws-render:
      command: ["/opt/sqi/capacity/aws-asg.sh"]   # absolute path; never run through a shell
      timeout: 2m
      env:
        AWS_ASG_NAME: render-asg
        AWS_REGION: us-east-1
```

| Action | When sqi sends it | The script should |
|---|---|---|
| `wake` | a named group has wake candidates | power those machines on |
| `grow` | a sized group should have more machines than sqi last asked for | raise the cloud group's size to `desired`, never lowering it |
| `retire` | a group has retire candidates; or, for a sized group, after a degradation, the instances that never registered | stop or remove those machines and, for a sized group, lower the cloud group's desired size by as many |
| `list` | a sized group degraded | report the instance IDs the cloud group runs |

A named group's script receives `wake` and `retire`; a sized group's receives
`grow`, `retire` and `list`.

### The request

One JSON object on standard input:

```json
{
  "version": 1,
  "action": "retire",
  "group": { "id": "…", "name": "cloud-spot", "kind": "sized" },
  "members": [
    {
      "worker_id": "…",
      "hostname": "ip-10-0-1-23",
      "ip_address": "10.0.1.23",
      "instance_id": "i-0123456789abcdef0",
      "interfaces": [{ "name": "eth0", "mac": "0a:1b:2c:3d:4e:5f", "addrs": ["10.0.1.23/24"] }]
    }
  ]
}
```

- `version` is always `1`, and `group` names the group (`id`, `name`, `kind`).
- `wake` and `retire` carry `members`. Each names its `worker_id` and, when
  known, its `hostname`, `ip_address`, `instance_id` (its
  [cloud instance ID](#cloud-instance-ids)) and `interfaces` (`name`, `mac`,
  `addrs`). An instance that never registered is retired by a member that
  carries an `instance_id` and nothing else, no `worker_id`.
- `grow` carries `desired`, the total number of machines the group should have,
  and `current`, the size sqi last asked for (or its registered members, when
  more). A request repeated after a failover carries `current` equal to
  `desired`.
- `list` carries nothing more.

A field that is zero or empty is left out, so a grow from nothing has no
`current`: read missing numbers as `0`.

### The response

One JSON object on standard output:

```json
{ "result": "ok", "message": "desired capacity set to 14" }
```

| Field | Meaning |
|---|---|
| `result` | `ok` (done, or for `wake`, started), `unsupported` (the script does not implement this action) or `error` |
| `message` | optional; recorded as the group's last action and logged |
| `instance_ids` | `list` only: the instances the backend runs, in the form each worker reports as its `cloud_instance_id` |

The rules the server applies:

- Empty standard output with exit status 0 is `ok`.
- A non-zero exit status is an error, carrying the status and the last line of
  standard error, whatever standard output held.
- A `retire` request may name several members, and its one answer applies to
  every member it names: `ok` finishes the retirement of each of them (see
  [Retiring](#retiring)).
- Standard output that is not one JSON object, or whose `result` is none of the
  three, is an error. So is more than 1 MiB of it: the script is killed.
- Standard error is the script's log: logged at Info line by line, up to 64 KiB
  per run, with the provider, action and group name. It is never parsed. Send
  your command output there, never to standard output.
- A `message` is cut to 2 KiB, and its control characters (newlines included)
  become spaces, so it cannot forge log lines.
- `unsupported` is not a failure. sqi records it and carries on: an unsupported
  `list` leaves orphan cleanup undone (and says so in the degraded reason), an
  unsupported `retire` puts the member back to `awake`, and an unsupported
  `wake` or `grow` is settled by the boot timeout like any wake or launch whose
  machines do not register.
- Instance IDs from `list` are used only when each is 1 to 256 bytes of UTF-8
  with no space, control or Unicode format character (such as a zero-width
  space or a bidirectional override); others are ignored, and duplicates
  dropped.

### How the server runs it

- **No shell, no `PATH` lookup.** `command[0]` must be an absolute path; the
  arguments are passed as they are.
- **Environment.** The server's own environment minus every variable whose
  name starts with `SQI_`, plus the provider's `env`. Everything else is
  inherited (`PG*`, cloud credentials, proxies); to isolate the script from
  them, wrap the command (`env -i`, a container, a service account).
- **Secrets go in `env`.** Each `env` value of four bytes or more is replaced by
  `<redacted>` wherever the script's output carries it: logged standard-error
  lines, the line quoted in a failure, and the response `message`. `env` values
  are also redacted by `sqi-server config print`. A standard-error line longer
  than 8 KiB is read in 8 KiB pieces, and a value split across two of them is
  not redacted, so keep secrets out of very long lines. The command's
  arguments are not redacted anywhere, and show in the process list, so never
  put a secret on the command line. Do not `set -x` either: it writes every
  command to standard error, which the server logs.
- **Timeout.** Each run is bounded by the provider's `timeout` (2 minutes by
  default, 5 s to 30 minutes); a script still running then is killed along
  with every process it started (its process group on Linux and macOS, a job
  object on Windows), so an `aws` or `gcloud` call it made does not go on to
  finish the action, and the action is recorded as `timeout`.
- **One call per group at a time.** A slow script delays only its own group's
  next actions. Keep the provider's `timeout` below the group's
  `boot_timeout_seconds`: a member's boot timeout starts when its wake is
  recorded, just before the call, so a wake call that runs longer than the boot
  timeout fails the members it is waking.
- **Requests are absolute, and may come again.** After a failover, or a call cut
  off at shutdown, the next leader repeats what was recorded. Write every action
  so that running it twice does no harm.

### Growing a sized group

sqi grows a sized group by asking for a total, never by adding: when the policy
wants more machines than sqi last asked for, and more than the group has member
rows, it records the new size and sends `grow` with `desired` set to it. It
never sends a `grow` below `current`, and it never shrinks a group by lowering
its size: it retires named members. One `retire` request may name several
members, and its one answer applies to every member it names.

> `grow` must never lower the cloud group's size. sqi's `current` is its
> own record and can be below the cloud group's real size: a group that replaces
> preempted instances on its own runs more machines than sqi has counted.
> Setting the size to a smaller `desired` would terminate live instances doing
> work. A `grow` must read the group's actual size from the cloud and set the
> larger of `desired` and that; when `desired` is no larger, it changes nothing
> and answers `ok`. All three sized example scripts do this.

### Retiring

sqi retires a member only once it is `retiring` (idle, holding no task, and
leased nothing more), and only while it is online. A `retire` the script
answers `ok` finishes the retirement at once: a named member becomes `asleep`
and offline, and an ephemeral member is deleted and its credential revoked.
Any other answer puts the member back to `awake`,
and it is tried again after another `idle_timeout_seconds`.

The answer covers every member the request names, so a script must answer `ok`
only when each named member was stopped or is already gone. If any could not be
removed (its instance is not in the group, say), it must answer `error`, so the
whole batch goes back to `awake`; an `ok` would make sqi delete the record of
an instance that keeps running outside its view.

In a sized script group, a member that reports no
[cloud instance ID](#cloud-instance-ids) is never a retire candidate: the
script could not name its instance. sqi logs one warning per member per server
process naming `worker.cloud_instance_id.source`, and the member stays `awake`.

For a sized group the script must also:

- lower the cloud group's desired size by the instances it removes (AWS
  `terminate-instance-in-auto-scaling-group --should-decrement-desired-capacity`;
  `delete-instances` on GCP and Azure does it already), or the cloud group
  starts replacements;
- act only on the instance IDs it is given, and check that every one
  belongs to the configured group before removing any. One that does not makes
  the whole request an `error` with nothing removed. Orphan cleanup sends
  `retire` with members that carry an `instance_id` only.

The group's `retire_action` is not sent to a script: what retiring means
(power off, terminate, suspend) is the script's decision.

### Degraded groups

A sized script group keeps a **launch record**: the size it last asked for
(`requested_machines` in the status) and when. sqi writes it before it sends the
`grow`, so a server that fails between the two leaves a request the next leader
repeats.

- A launch is **unmet** until the group's registered member rows (online or
  offline, not being retired) reach the size asked for; then it is met.
- Only an unmet launch can degrade. If `boot_timeout_seconds` passes with
  machines still missing, the group is **degraded**: the status's `degraded`
  gives the reason (for example
  `3 of 14 machines did not register within boot_timeout (10m0s)`) and
  `since`, `requested_machines` drops to the machines that did register,
  `sqi_capacity_group_degraded` reads `1`, a warning is logged, and the group
  asks for no more machines for two boot timeouts (`backoff_until`). Tasks only
  that group could run stop reading as waiting for it until the backoff ends.
- Once a launch is met, `requested_machines` follows the group's member rows,
  online and offline. A worker restarting, or missing a heartbeat, keeps its row
  and so never lowers the record; a lowered record could make the next absolute
  `grow` terminate live instances. A machine that is gone drops out: a
  retired or preempted member's row is deleted when it deregisters, and an
  ephemeral member that vanished without a word is deleted after
  `boot_timeout_seconds`.
- While the group is degraded, `requested_machines` holds what was present at
  the degradation and follows only the member rows down: a member being retired
  still counts, so one whose retirement aborts does not read as a machine that
  registered late, while a row that is deleted lowers it.
- The degraded state clears when machines that registered late lift the
  group's member rows that are not being retired above the current
  `requested_machines`, or when a later launch is met. The backoff runs out on
  its own.

### Orphan cleanup

An instance that booted but never registered still bills. After a degradation,
once per degradation and per server process, sqi asks the script to `list` the
cloud group's instances, and retires every listed instance that no member
reports as its `cloud_instance_id`, by instance ID alone. It does this only
while no launch is unmet, and only within a boot timeout of the degradation.

- It is skipped, with one warning naming them, if any member of the group,
  online or offline, has no cloud instance ID: that member's own instance would
  look orphaned.
- It is refused, with a warning, if the instances to retire outnumber
  `max_machines` less the members: those cannot all be machines sqi asked for.
- A script with no `list` leaves the degraded reason ending
  `instances that never registered may still be running; the provider has no
  list action`.

Known limits of the cleanup:

- A member lost (its row deleted) while a launch is unmet, whose replacement the
  cloud group starts on its own and which has not registered by the launch's
  deadline, is listed as an orphan and may be retired. The refusal above bounds
  how many.
- An ephemeral member that vanished without deregistering keeps its row for up
  to `boot_timeout_seconds`, and meanwhile can hide one launched machine that
  never registered.
- A member that is not ephemeral never loses its row, so it keeps the launch
  record up even when its machine is gone. Give sized groups ephemeral members
  only.
- Every member of a sized group needs a cloud instance ID (set
  `worker.cloud_instance_id.source`). Without one orphan cleanup is skipped, and
  that member is never retired ([Retiring](#retiring)).
- A degradation that late registrations clear before its cleanup runs is not
  cleaned up at all.
- A group switched to another provider within a boot timeout of a degradation
  has its orphans cleaned up with the new provider's `list` and `retire`.
- A worker-side network partition can terminate the whole fleet (below).

#### A worker-side partition

If the workers of a sized group lose the broker while the server keeps its own
connection, and the partition outlasts roughly twice `boot_timeout_seconds`,
orphan cleanup retires the whole group:

1. The members miss their heartbeats and go offline.
2. After `boot_timeout_seconds` offline, their ephemeral rows are deleted as
   vanished.
3. The launch record drops with them, and the group's next launch (to replace
   machines that are in fact still running) does not register within
   `boot_timeout_seconds`, so the group degrades.
4. The cleanup's `list` returns every live instance, and no member reports any
   of them, so every one is retired.

No sqi work is lost: the tasks were reclaimed and run elsewhere when the
members went offline, but the fleet is terminated and launched again.
`boot_timeout_seconds` is the knob: a longer one rides out a longer partition.
The server's
[reconnect grace](architecture.md#after-a-broker-outage-the-reconnect-grace)
covers only an outage on the server's side.

### Example scripts

`deploy/capacity/` holds four example providers: `aws-asg.sh` (EC2 Auto Scaling
group), `gcp-mig.sh` (Compute Engine managed instance group) and
`azure-vmss.sh` (Azure scale set) for sized groups, each answering `grow`,
`retire` and `list`; and `ipmi.sh` for a named group of machines with a BMC,
answering `wake` (`chassis power on`) and `retire` (`chassis power soft`). Its
`README.md` lists the rules every implementation must follow.

> **Documentation-derived: not run against a real cloud or BMC.** The scripts
> are tested against stub `aws`, `gcloud`, `az` and `ipmitool` programs that
> record their arguments; that checks the commands they build, not that the
> real tools accept them. Try one against a scratch group first.

- They need `bash` 4.4 or later and `jq`, plus the cloud's CLI or `ipmitool`,
  on the server's `PATH`. Each refuses to run on an older bash (macOS's stock
  `/bin/bash` is 3.2).
- `aws-asg.sh` needs AWS CLI version 2, for `--no-cli-pager`.
- `azure-vmss.sh` supports uniform orchestration only, and its `retire` accepts
  only the bare numeric scale-set instance ID, never a resource ID.
- `gcp-mig.sh` passes `--quiet` to suppress `gcloud`'s delete prompt; the check
  that each instance belongs to the group is the guard.
- `ipmi.sh` passes the BMC password to `ipmitool` through `IPMI_PASSWORD` and
  `-E`, never on the command line.

## Cloud instance IDs

A worker can report the ID of the cloud instance it runs on when it registers.
Set [`worker.cloud_instance_id.source`](worker-configuration.md#workercloud_instance_idsource)
to `aws`, `gcp` or `azure`, and it reads the ID from the provider's metadata
service once at startup: the EC2 instance ID, the Compute Engine instance name,
or the scale-set instance ID. The server stores it as the worker's
`cloud_instance_id` (a registration that reports none keeps the stored value),
shows it in `GET /workers`, sends it to a script as each member's `instance_id`,
and matches it against a script's `list` in orphan cleanup. Every member of a
sized script group needs one. With `external`, it is how your own retirement
tooling maps a retire candidate to an instance.

## Wiring an autoscaler

This section is for groups with `provider: external`, where your own
autoscaler acts on the signal. A script provider is the other way to drive a
cloud group ([The script provider](#the-script-provider)): sqi then calls your
script itself, and does the retirement check described below for you.

> **Documentation-derived: not run against a real cloud.** The AWS, GCP, Azure
> and KEDA sections below are written from the providers' documentation and
> sqi's own behaviour. None of this wiring has been executed against a real Auto
> Scaling group, instance group, scale set or cluster. Treat the snippets as a
> starting point and check them against the provider's current documentation.

The pattern is the same everywhere:

1. Scale out to `desired_machines`. Read `sqi_capacity_desired_machines`
   (or `status.desired_machines`) and set the group's total capacity to it.
2. Enrol launches with the group token in the launch template, with
   `worker.preemption.watch` set for spot capacity.
3. Protect instances from scale-in by default, and take a machine out only by
   name from `retire_candidates`. Set
   [`worker.cloud_instance_id.source`](#cloud-instance-ids) on the instances so
   each worker reports its instance ID, and map a candidate's worker ID to its
   instance through `cloud_instance_id` in `GET /workers/{id}`.
4. Stop the worker service from the termination lifecycle hook, so the
   machine drains instead of dying, with `shutdown_grace_period` and the hook
   timeout set as above.

With `external` there is no server-side retirement check. `retire_candidates`
is advisory: a candidate was idle when it was evaluated, and may be leased work
before your actuator reaches it. Draining protects that work as long as the
grace period and hook timeout cover the longest task. If you cannot guarantee
that, retire only candidates that are still idle when you act (re-read the
status, or disable the worker first and check it holds no task).

A failover or restart forgets machines still booting. The in-memory record of
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
   `desired_machines` already counts the machines you have. It does not work
   for a group that can scale to zero (`min_machines: 0`, as in the example
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

- No first-party cloud SDK. sqi launches no cloud instance itself: a sized group
  grows through your autoscaler (`external`) or your script.
- Real sleep and wake are not exercised in CI. The end-to-end tests run the real
  server and worker binaries, with a command that writes a marker file standing
  in for the suspend and a process restart standing in for the wake.
- The example scripts in `deploy/capacity/` are documentation-derived, tested
  only against stub command-line tools ([Example scripts](#example-scripts)).
- There is no `sqi-load` capacity scenario: the load simulator does not model a
  suspended machine.
- A self-healing cloud group (one that replaces preempted instances by itself)
  is safe only because `grow` never lowers the group's size
  ([Growing a sized group](#growing-a-sized-group)). A script that sets the
  size to `desired` without that check can terminate working instances.
- An `external` group has no degraded state. A group whose image never
  registers is bounded by `max_machines`, `max_step_up` and the cooldown but is
  not backed off; the mismatch list and your cloud's own failed-launch metrics
  are how you notice. With `external` there is also no retirement check:
  `retire_candidates` is advisory ([above](#wiring-an-autoscaler)).
- Orphan cleanup has known blind spots, and a long worker-side network
  partition can end in it retiring a whole sized group
  ([Orphan cleanup](#orphan-cleanup)).
- Sized groups need REST authentication when broker authentication is on
  ([above](#sized-groups-with-broker-authentication-on)).
- An ephemeral member cannot be removed from its group, only retired
  with the worker itself ([above](#sized-groups-with-broker-authentication-off)).
- Multi-queue workers. A worker configured with two or more `queue_ids`
  registers with no single queue affinity, and the server offers it work from
  any queue. The signal reads queue affinity the same way the lease path does,
  so it neither over- nor under-counts against that behaviour, but a group with
  a `queue_ids` allow-list may have members that take work from other queues.
- Idle and pacing state is in memory and restarts with the server or on a
  failover ([above](#retire-candidates)). A sized script group's pacing is
  seeded from its launch record, so a new leader keeps the machines already
  asked for.
