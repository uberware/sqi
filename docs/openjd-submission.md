# OpenJD Job Submission

`sqi-server` accepts jobs in the
[OpenJD v2023-09](https://github.com/OpenJobDescription/openjd-specifications)
format, submitted as YAML or JSON to `POST /api/v1/jobs`.

This document shows three progressively richer examples:

1. [Minimal single-task job](#1-minimal-single-task-job) — one step, one task, no parameters.
2. [Parameter-space job](#2-parameter-space-job) — one step expanded into many tasks via a range.
3. [Multi-step job with dependencies](#3-multi-step-job-with-dependencies) — sequential steps,
   shared parameters, and step-level environments.

See [`docs/api.md`](api.md) for the HTTP mechanics (endpoint, query parameters,
response shape, error handling).

---

## Template structure at a glance

```
specificationVersion: "jobtemplate-2023-09"
name: <string>                  # required; may reference job parameters
description: <string>           # optional
parameterDefinitions:           # optional list of job-level parameters
  - name: <Name>
    type: INT | FLOAT | STRING | PATH
    default: <value>            # optional; omitting makes the param required
    allowedValues: [...]        # optional
steps:                          # at least one required
  - name: <Name>
    script:
      actions:
        onRun:
          command: <executable>
          args: [...]           # may reference Param.* and Task.Param.*
    parameterSpace:             # optional; controls task expansion
      taskParameterDefinitions:
        - name: <Name>
          type: INT | FLOAT | STRING | PATH
          range: <range-expression>
      combination: <expr>       # optional; default is Cartesian product of all params
    dependsOn:                  # optional; list of step names that must complete first
      - stepName: <Name>
    stepEnvironments: [...]     # optional; per-step environment blocks
jobEnvironments: [...]          # optional; applied to every step
```

---

## 1. Minimal single-task job

A job with a single step and no parameter space. `sqi-server` creates exactly
one task for the step.

```yaml
specificationVersion: "jobtemplate-2023-09"
name: Hello World

steps:
  - name: Greet
    script:
      actions:
        onRun:
          command: echo
          args:
            - "Hello from sqi!"
```

Submit it:

```sh
curl -s -X POST \
  "http://localhost:8080/api/v1/jobs?farm_id=$FARM_ID&queue_id=$QUEUE_ID&owner=alice" \
  -H "Content-Type: application/x-yaml" \
  --data-binary @hello.yaml | jq '{id, name, status}'
```

---

## 2. Parameter-space job

`parameterSpace` expands a step into one task per parameter combination.
This is the primary mechanism for fan-out workloads such as rendering an
image sequence.

### 2a. Integer range

Renders frames 1–100 — each frame becomes one task.

```yaml
specificationVersion: "jobtemplate-2023-09"
name: "Render {{Param.SceneName}} — frames {{Param.StartFrame}}–{{Param.EndFrame}}"

parameterDefinitions:
  - name: SceneName
    type: STRING
    description: Scene file name without extension
  - name: StartFrame
    type: INT
    default: "1"
  - name: EndFrame
    type: INT
    default: "100"
  - name: OutputDir
    type: PATH
    description: Root directory for rendered frames

steps:
  - name: Render
    script:
      actions:
        onRun:
          command: /usr/bin/blender
          args:
            - "--background"
            - "{{Param.SceneName}}.blend"
            - "--render-output"
            - "{{Param.OutputDir}}/frame_####"
            - "--render-format"
            - "OPEN_EXR"
            - "--render-frame"
            - "{{Task.Param.Frame}}"
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: INT
          range: "{{Param.StartFrame}}-{{Param.EndFrame}}"
```

Submit with explicit parameter values:

```sh
curl -s -X POST \
  "http://localhost:8080/api/v1/jobs?farm_id=$FARM_ID&queue_id=$QUEUE_ID&owner=alice&priority=70" \
  -H "Content-Type: application/x-yaml" \
  --data-binary '
specificationVersion: "jobtemplate-2023-09"
name: "Render hero_shot"

parameterDefinitions:
  - name: SceneName
    type: STRING
  - name: StartFrame
    type: INT
    default: "1"
  - name: EndFrame
    type: INT
    default: "100"
  - name: OutputDir
    type: PATH

steps:
  - name: Render
    script:
      actions:
        onRun:
          command: /usr/bin/blender
          args:
            - "--background"
            - "hero_shot.blend"
            - "--render-output"
            - "/renders/hero_shot/frame_####"
            - "--render-frame"
            - "{{Task.Param.Frame}}"
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: INT
          range: "1-100"
'
```

### 2b. Cartesian product of two ranges

Combine two parameters with `*` to get every pair. The example below simulates
a grid of tile coordinates — useful for tiled rendering or distributed
simulation.

```yaml
specificationVersion: "jobtemplate-2023-09"
name: Tiled Render

steps:
  - name: RenderTile
    script:
      actions:
        onRun:
          command: /opt/renderer/bin/render
          args:
            - "--tile-x"
            - "{{Task.Param.TileX}}"
            - "--tile-y"
            - "{{Task.Param.TileY}}"
    parameterSpace:
      taskParameterDefinitions:
        - name: TileX
          type: INT
          range: "0-3"      # 4 columns → values 0, 1, 2, 3
        - name: TileY
          type: INT
          range: "0-3"      # 4 rows → values 0, 1, 2, 3
      combination: "TileX * TileY"   # 16 tasks total
```

### 2c. Zip (association) of two ranges

Parentheses zip parameters together rather than forming a product. Both ranges
must have the same length.

```yaml
parameterSpace:
  taskParameterDefinitions:
    - name: InputChunk
      type: PATH
      range: "chunk_001.dat chunk_002.dat chunk_003.dat"
    - name: OutputChunk
      type: PATH
      range: "out_001.dat  out_002.dat  out_003.dat"
  combination: "(InputChunk, OutputChunk)"   # 3 tasks, not 9
```

### 2d. Mixed combination

Zip one pair, then cross it with a third range:

```yaml
combination: "(SceneFile, Camera) * Frame"
```

---

## 3. Multi-step job with dependencies

Steps that `dependsOn` earlier steps are held in `pending` state until all
dependency steps reach `completed`. This models sequential pipelines:
download → transcode → upload, or render → composite → encode.

```yaml
specificationVersion: "jobtemplate-2023-09"
name: "VFX Pipeline — {{Param.ShotName}}"

parameterDefinitions:
  - name: ShotName
    type: STRING
  - name: FrameStart
    type: INT
    default: "1001"
  - name: FrameEnd
    type: INT
    default: "1100"
  - name: InputPath
    type: PATH
  - name: OutputPath
    type: PATH

jobEnvironments:
  - name: PipelineEnv
    variables:
      SHOT_NAME: "{{Param.ShotName}}"
      LOG_LEVEL: "info"

steps:
  # ── Step 1: Render ────────────────────────────────────────────────────────
  - name: Render
    script:
      actions:
        onRun:
          command: /opt/vfx/bin/render
          args:
            - "--scene"
            - "{{Param.InputPath}}/{{Param.ShotName}}.ma"
            - "--frame"
            - "{{Task.Param.Frame}}"
            - "--output"
            - "{{Param.OutputPath}}/render/frame.{{Task.Param.Frame}}.exr"
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: INT
          range: "{{Param.FrameStart}}-{{Param.FrameEnd}}"

  # ── Step 2: Composite ─────────────────────────────────────────────────────
  # Runs only after every Render task succeeds.
  - name: Composite
    dependsOn:
      - stepName: Render
    script:
      actions:
        onRun:
          command: /opt/vfx/bin/composite
          args:
            - "--input"
            - "{{Param.OutputPath}}/render/frame.{{Task.Param.Frame}}.exr"
            - "--lut"
            - "/luts/aces_output.cube"
            - "--output"
            - "{{Param.OutputPath}}/comp/frame.{{Task.Param.Frame}}.exr"
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: INT
          range: "{{Param.FrameStart}}-{{Param.FrameEnd}}"

  # ── Step 3: Encode ────────────────────────────────────────────────────────
  # Runs after Composite completes. Single task — no parameter space.
  - name: Encode
    dependsOn:
      - stepName: Composite
    script:
      actions:
        onRun:
          command: /usr/bin/ffmpeg
          args:
            - "-framerate"
            - "24"
            - "-i"
            - "{{Param.OutputPath}}/comp/frame.%04d.exr"
            - "-c:v"
            - "libx264"
            - "-crf"
            - "18"
            - "{{Param.OutputPath}}/{{Param.ShotName}}_v001.mp4"
```

---

## 4. Usage pools

A usage pool is a named concurrency limit that sqi enforces by counting active
claims — it caps how many tasks may run at once against a shared, finite
resource. Software licenses are the original use case (e.g. a 10-seat Arnold
license), but a usage pool works for *any* dimension where you want a ceiling:
a product, a renderer version, a show, or a client code.

Declare a usage pool requirement on a step via an OpenJD host-requirement
amount named `amount.worker.usagepool.<name>`, where `<name>` matches a pool
registered on the Usage Pools page (or via `POST /api/v1/usage-pools`). The
scheduler will not assign a task from that step unless the pool has a free slot,
and the slot is released when the task reaches a terminal state.

Example — cap concurrent render tasks on a show named `starfield` to the
pool's registered limit:

```yaml
specificationVersion: "jobtemplate-2023-09"
name: comp

steps:
  - name: render
    hostRequirements:
      amounts:
        # "starfield" must match a usage pool registered on the server.
        - name: amount.worker.usagepool.starfield
          min: 1
    script:
      actions:
        onRun:
          command: /opt/renderer/bin/render
          args:
            - "--scene"
            - "scene.ma"
            - "--frame"
            - "{{Task.Param.Frame}}"
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: INT
          range: "1-100"
```

A step may list multiple `amount.worker.usagepool.*` amounts. The scheduler
dispatches a task only when **every** named pool has a free slot; if any pool
is at capacity, the task waits. This lets you enforce overlapping limits
simultaneously — for example, a per-show cap and a global license count at
the same time.

> **Gotcha — unregistered pools.** If a step names a pool that is not
> registered on the server, sqi treats that pool as having zero capacity.
> Tasks in that step will never be dispatched. If tasks remain stuck in
> `ready` state longer than expected, check the Usage Pools page to confirm
> every pool referenced in your job template is registered.

Live utilization — slots in use vs. available — is visible on the **Usage
Pools** page and the dashboard.

The optional `server_hint` field on a pool records a license server address
(FLEXlm/RLM) for diagnostics; leave it empty for non-license dimensions.

---

## 5. CPU reservations

`amount.worker.vcpu` `min` is a per-task CPU-core reservation. The scheduler
co-schedules tasks on a worker only while their combined reservations fit the
worker's total core count. This is the same consumable-amount mechanism as
usage pools, applied at the host level.

**Declaring CPU cores:**

```yaml
steps:
  - name: Transcode
    hostRequirements:
      amounts:
        - name: amount.worker.vcpu
          min: 4          # reserve 4 cores per task on the assigned worker
    script:
      actions:
        onRun:
          command: ffmpeg
          args: ["-threads", "4", "-i", "input.mov", "output.mp4"]
    parameterSpace:
      taskParameterDefinitions:
        - name: Chunk
          type: INT
          range: "1-10"
```

With `min: 4` on a 16-core worker, four tasks run in parallel (4 × 4 = 16
cores committed). A 4-core worker runs one task at a time (4 × 1 = 4 cores).

**Omitting `amount.worker.vcpu` reserves the whole machine.** A step with no
`amount.worker.vcpu` amount is treated as requiring all of the worker's cores —
one such task runs per worker at a time. This is the safe default: jobs that
do not declare CPU requirements never oversubscribe a host.

Example — a 4-core worker running one undeclared task vs. four 1-core tasks:

```yaml
# One task at a time (reserves whole machine):
hostRequirements: {}   # or omit hostRequirements entirely

# Four tasks in parallel on a 4-core worker:
hostRequirements:
  amounts:
    - name: amount.worker.vcpu
      min: 1
```

> **Note on `min` defaults:** Only `min` is used as a reservation. A step that
> declares `amount.worker.vcpu` but omits `min` (including a `max`-only
> declaration) defaults to a 1-core reservation — per OpenJD jobtemplate-2023-09,
> an omitted `min` defaults to the capability's reserved minimum, which is 1 for
> `amount.worker.vcpu`. Only omitting `amount.worker.vcpu` entirely reserves the
> whole machine. Declare an explicit `min` to reserve more than one core.

---

## Parameter reference syntax

Inside `command`, `args`, step `variables`, and environment `variables`, use
double-brace syntax to reference values:

| Expression | Resolves to |
|---|---|
| `{{Param.Name}}` | Job-level parameter value |
| `{{Task.Param.Name}}` | Task-level parameter value (from `parameterSpace`) |

---

## Validation errors

`sqi-server` validates templates before persisting them. Errors are returned as
`422 Unprocessable Entity` with a detail message that includes a JSON pointer to
the offending field:

```json
{
  "type": "about:blank",
  "title": "Unprocessable Entity",
  "status": 422,
  "detail": "step 'Composite' dependsOn unknown step 'Rendur'",
  "instance": "a1b2c3d4e5f60708"
}
```

Common validation failures:

- `specificationVersion` missing or not `"jobtemplate-2023-09"`
- No steps defined
- Step name not unique within the job
- `dependsOn` references a step name that does not exist
- `parameterSpace.combination` references a parameter name not in
  `taskParameterDefinitions`
- Zipped parameters (`(A, B)`) have different range lengths
- Required job parameter not provided in the submission query string
- Named storage location referenced in a `PATH` parameter has no root defined
  for the target compute location (see `docs/api.md` — storage locations)

---

## Submitting JSON instead of YAML

All examples above can be submitted as JSON with
`Content-Type: application/json`. The field names match the YAML keys converted
to camelCase:

```json
{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "Hello World",
  "steps": [
    {
      "name": "Greet",
      "script": {
        "actions": {
          "onRun": {
            "command": "echo",
            "args": ["Hello from sqi!"]
          }
        }
      }
    }
  ]
}
```
