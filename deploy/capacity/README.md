<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->

# Example capacity provider scripts

Four example [script providers](../../docs/capacity.md) for sqi's capacity groups. Copy one, read
it, and adapt it to your estate.

| Script | Group kind | Actions |
|---|---|---|
| `aws-asg.sh` | sized | `grow`, `retire`, `list` for an EC2 Auto Scaling group |
| `gcp-mig.sh` | sized | `grow`, `retire`, `list` for a Compute Engine managed instance group |
| `azure-vmss.sh` | sized | `grow`, `retire`, `list` for an Azure scale set (uniform orchestration) |
| `ipmi.sh` | named | `wake` (`chassis power on`) and `retire` (`chassis power soft`) through each machine's BMC |

**These scripts are documentation-derived. They were not run against a real cloud account or a real
BMC.** `examples_test.go` runs each against stub `aws`, `gcloud`, `az` and `ipmitool` programs that
record their arguments, which shows the scripts build the commands their authors intended, not that
the real tools accept them. Try one against a scratch group before pointing a production group at it.

They need `bash` **4.4 or later** and [`jq`](https://jqlang.github.io/jq/), plus the CLI of the
cloud they drive (`aws`, `gcloud`, `az`) or `ipmitool`, on the server's `PATH`. Each script checks
`BASH_VERSINFO` first and, on an older bash (the stock macOS `/bin/bash` is 3.2), exits non-zero
with a message on standard error instead of running; put a newer bash first on the server's `PATH`.

## Configuring a provider

A script is an entry under `capacity.providers` in the server's configuration. The group names it as
`script:<name>`.

```yaml
capacity:
  providers:
    aws-render:
      command: ["/opt/sqi/capacity/aws-asg.sh"] # absolute path; never run through a shell
      timeout: 2m
      env:
        AWS_ASG_NAME: render-asg
        AWS_REGION: us-east-1
```

The server runs the command once per action, with the request as one JSON object on standard input
and its own environment minus every `SQI_*` variable, plus `env`. Each script's header comment names
the variables it reads.

## The contract in short

The authoritative description is the package comment in `internal/capacity/script/script.go`.

- **Request** (standard input, one JSON object): `version` (1), `action` (`wake`, `grow`, `retire`
  or `list`), `group` (`id`, `name`, `kind`), and by action `desired` and `current` (`grow`) or
  `members` (`wake`, `retire`; each has `worker_id` and, when known, `hostname`, `ip_address`,
  `instance_id`, `interfaces`).
- **Response** (standard output, **a single JSON object**): `result` (`ok`, `unsupported` or `error`),
  an optional `message`, and for `list` the `instance_ids`. Print nothing else on standard output:
  send command output and logs to standard error, which the server logs line by line.
- A non-zero exit status is an error. An action the script does not implement answers
  `{"result":"unsupported"}`, and the server falls back.
- An orphaned instance (one that booted and never registered) is retired by a `retire` request whose
  member carries `instance_id` **only**, with no `worker_id`.

## Rules every implementation must follow

- **`grow` must never lower the cloud group's size.** `desired` is absolute, but sqi's `current` is
  its own record and can be *below* the group's real size: a group that replaces preempted instances
  runs more machines than sqi has counted. Setting the size to a smaller `desired` would terminate
  live, working instances. A `grow` must read the group's actual size from the cloud and set
  max(`desired`, actual). When `desired` is no larger than the actual size it answers
  `{"result":"ok","message":"no change: desired <= current size"}` and changes nothing. All three
  sized examples do this.
- **`retire` of a sized group must also lower the group's desired size** by the instances it
  removes (AWS `--should-decrement-desired-capacity`; `delete-instances` on GCP and Azure does it
  implicitly), or the cloud group starts replacements and sqi pays for machines it just retired.
- **`retire` acts only on the instance IDs it is given.** It never lowers anything else, and before
  terminating an instance it checks that the instance belongs to the configured group, answering ok
  with a "not a member, skipped" count rather than touching a foreign instance.
- **Every member of a sized group needs a `cloud_instance_id`.** `retire` names members by it, and a
  script that finds a member without one (or with one it cannot validate) refuses the **whole**
  retire batch rather than guess, which blocks scale-in until it is fixed. Set
  `worker.cloud_instance_id.source` (`aws`, `gcp` or `azure`) in each worker's configuration so it
  reports the ID when it registers.
- **A size that cannot be read ends the script with a non-zero exit.** `grow` compares the request
  with the size the cloud reports; a CLI failure, or a size that is not a whole number, makes the
  script exit non-zero (reason on standard error) and change nothing, never fall through to a
  resize. `desired` and `current` must be whole numbers from 0 to 100000.
- **`list` answers the form a worker reports** as its `cloud_instance_id`: the EC2 instance ID
  (`i-0123456789abcdef0`), the GCE instance name, the Azure scale-set instance ID (the last segment
  of the VM's resource ID, `3`). Azure `retire` accepts **only** that bare ID and refuses a resource
  ID: keeping the last segment of one would let a VM of another scale set pass the membership check
  as an instance of the configured set.
- **Treat the request as data.** Read it with `jq`, validate every instance ID and name against a
  strict pattern before use, pass values as separate arguments, quote everything, and never `eval`
  or build a command string from request text. A request holding an invalid ID is refused whole.
- **Secrets go in `env`, never on the argument line.** The server redacts configured `env` values
  from the script's output and log lines, but the process list is not redacted. `ipmi.sh` takes
  `IPMI_PASSWORD` through the environment (`ipmitool -E`).
- **Do not `set -x`** (or run the script under `bash -x`): it writes every command, and any secret
  in it, to standard error, which the server logs.
