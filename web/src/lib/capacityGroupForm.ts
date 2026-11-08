// SPDX-License-Identifier: AGPL-3.0-or-later

import type { CapacityGroupInput } from '@/api/mutations'
import type { CapacityGroup, CapacityGroupKind, MachineProfile } from '@/api/types'

/** Every control of the capacity-group form that can carry its own error message. */
export type FieldKey =
  | 'farm'
  | 'kind'
  | 'name'
  | 'provider'
  | 'queueIds'
  | 'minMachines'
  | 'maxMachines'
  | 'priority'
  | 'maxStepUp'
  | 'cooldown'
  | 'idle'
  | 'boot'
  | 'retireAction'
  | 'cpuCount'
  | 'ramMb'
  | 'gpuCount'
  | 'gpuMemoryMb'
  | 'os'
  | 'arch'
  | 'computeLocation'
  | 'tags'
  | 'extensions'

export type FieldErrors = Partial<Record<FieldKey, string>>

/** The form's state: every field is the text the person typed. */
export interface FormValues {
  farmId: string
  name: string
  kind: CapacityGroupKind
  provider: string
  enabled: boolean
  queueIds: string[]
  minMachines: string
  maxMachines: string
  priority: string
  maxStepUp: string
  cooldown: string
  idle: string
  boot: string
  retireAction: 'sleep' | 'shutdown'
  cpuCount: string
  ramMb: string
  gpuCount: string
  gpuMemoryMb: string
  os: string
  arch: string
  computeLocation: string
  tags: string
  extensions: string
}

export function defaultValues(farmId: string): FormValues {
  return {
    farmId,
    name: '',
    kind: 'sized',
    provider: 'external',
    enabled: true,
    queueIds: [],
    minMachines: '0',
    maxMachines: '1',
    priority: '0',
    maxStepUp: '1',
    cooldown: '60',
    idle: '600',
    boot: '600',
    retireAction: 'sleep',
    cpuCount: '',
    ramMb: '',
    gpuCount: '',
    gpuMemoryMb: '',
    os: '',
    arch: '',
    computeLocation: '',
    tags: '',
    extensions: '',
  }
}

const text = (n: number | undefined): string => (n === undefined ? '' : String(n))

export function valuesFromGroup(g: CapacityGroup): FormValues {
  const p = g.machine_profile
  return {
    farmId: g.farm_id,
    name: g.name,
    kind: g.kind,
    provider: g.provider,
    enabled: g.enabled,
    queueIds: g.queue_ids,
    minMachines: String(g.min_machines),
    maxMachines: String(g.max_machines),
    priority: String(g.priority),
    maxStepUp: String(g.max_step_up),
    cooldown: String(g.scale_up_cooldown_seconds),
    idle: String(g.idle_timeout_seconds),
    boot: String(g.boot_timeout_seconds),
    retireAction: g.retire_action,
    cpuCount: text(p.cpu_count),
    ramMb: text(p.ram_mb),
    gpuCount: text(p.gpu_count),
    gpuMemoryMb: text(p.gpu_memory_mb),
    os: p.os ?? '',
    arch: p.arch ?? '',
    computeLocation: p.compute_location ?? '',
    tags: formatTags(p.tags),
    extensions: (p.extensions ?? []).join(', '),
  }
}

/** Parses a whole-number field; blank, fractional and non-numeric text give undefined. */
export function wholeNumber(s: string): number | undefined {
  const t = s.trim()
  if (t === '') return undefined
  const n = Number(t)
  return Number.isInteger(n) ? n : undefined
}

export function formatTags(tags: Record<string, string> | undefined): string {
  return Object.entries(tags ?? {})
    .map(([k, v]) => `${k}=${v}`)
    .join('\n')
}

/** Parses one `key=value` per line. A malformed line is reported, not guessed at. */
export function parseTags(s: string): { tags: Record<string, string>; error?: string } {
  const tags: Record<string, string> = {}
  for (const raw of s.split('\n')) {
    const line = raw.trim()
    if (line === '') continue
    const eq = line.indexOf('=')
    if (eq < 1) return { tags, error: `"${line}" is not key=value` }
    tags[line.slice(0, eq).trim()] = line.slice(eq + 1).trim()
  }
  return { tags }
}

export function parseList(s: string): string[] {
  return s
    .split(/[\s,]+/)
    .map((x) => x.trim())
    .filter((x) => x !== '')
}

const REQUIRED_NUMBERS: Array<[FieldKey, keyof FormValues, string, number]> = [
  ['minMachines', 'minMachines', 'Min machines', 0],
  ['maxMachines', 'maxMachines', 'Max machines', 1],
  ['maxStepUp', 'maxStepUp', 'Max step up', 1],
  ['cooldown', 'cooldown', 'Scale-up cooldown', 0],
  ['idle', 'idle', 'Idle timeout', 0],
  ['boot', 'boot', 'Boot timeout', 0],
]

const OPTIONAL_NUMBERS: Array<[FieldKey, keyof FormValues, string]> = [
  ['ramMb', 'ramMb', 'RAM'],
  ['gpuCount', 'gpuCount', 'GPU count'],
  ['gpuMemoryMb', 'gpuMemoryMb', 'GPU memory'],
]

/**
 * Checks what can be checked without the server: required text, whole numbers,
 * the CPU count of a sized group and the tag syntax. The server's own rules
 * (name pattern, timeout floors, min against max) are left to it and come back
 * through {@link mapServerErrors}.
 */
export function validateValues(v: FormValues): FieldErrors {
  const errors: FieldErrors = {}
  if (v.farmId === '') errors.farm = 'Choose a farm.'
  if (v.name.trim() === '') errors.name = 'Enter a name.'
  for (const [key, field, label, min] of REQUIRED_NUMBERS) {
    const n = wholeNumber(String(v[field]))
    if (n === undefined || n < min)
      errors[key] = `${label} must be a whole number of at least ${min}.`
  }
  const priority = wholeNumber(v.priority)
  if (priority === undefined) errors.priority = 'Priority must be a whole number.'
  if (v.kind === 'sized') {
    const cpu = wholeNumber(v.cpuCount)
    if (cpu === undefined || cpu < 1)
      errors.cpuCount = 'A sized group needs a CPU count of at least 1.'
    for (const [key, field, label] of OPTIONAL_NUMBERS) {
      const raw = String(v[field]).trim()
      const n = wholeNumber(raw)
      if (raw !== '' && (n === undefined || n < 0)) errors[key] = `${label} must be a whole number.`
    }
    const { error } = parseTags(v.tags)
    if (error !== undefined) errors.tags = error
  }
  return errors
}

/**
 * Builds the request body. `PUT` replaces every setting on the server, so an
 * edit sends every field this form manages (an omitted `enabled` would become
 * true and an omitted `queue_ids` would become "all queues"), and keeps the
 * parts of the profile the form does not edit.
 */
export function buildInput(
  v: FormValues,
  mode: 'create' | 'edit',
  original?: CapacityGroup,
): CapacityGroupInput {
  return {
    ...(mode === 'create' ? { farm_id: v.farmId } : {}),
    name: v.name.trim(),
    kind: v.kind,
    provider: v.provider,
    enabled: v.enabled,
    queue_ids: v.queueIds,
    min_machines: wholeNumber(v.minMachines) ?? 0,
    max_machines: wholeNumber(v.maxMachines) ?? 0,
    priority: wholeNumber(v.priority) ?? 0,
    max_step_up: wholeNumber(v.maxStepUp) ?? 0,
    scale_up_cooldown_seconds: wholeNumber(v.cooldown) ?? 0,
    idle_timeout_seconds: wholeNumber(v.idle) ?? 0,
    boot_timeout_seconds: wholeNumber(v.boot) ?? 0,
    retire_action: v.retireAction,
    machine_profile: buildProfile(v, original),
  }
}

function buildProfile(v: FormValues, original: CapacityGroup | undefined): MachineProfile {
  // A named group's profile is learned from its members; the form has nothing
  // to say about it, so an edit hands back what it loaded.
  if (v.kind === 'named') return original?.machine_profile ?? {}

  const profile: MachineProfile = { cpu_count: wholeNumber(v.cpuCount) ?? 0 }
  const ram = wholeNumber(v.ramMb)
  if (ram !== undefined) profile.ram_mb = ram
  const gpus = wholeNumber(v.gpuCount)
  if (gpus !== undefined) profile.gpu_count = gpus
  const gpuMem = wholeNumber(v.gpuMemoryMb)
  if (gpuMem !== undefined) profile.gpu_memory_mb = gpuMem
  if (v.os.trim() !== '') profile.os = v.os.trim()
  if (v.arch.trim() !== '') profile.arch = v.arch.trim()
  if (v.computeLocation.trim() !== '') profile.compute_location = v.computeLocation.trim()
  const { tags } = parseTags(v.tags)
  if (Object.keys(tags).length > 0) profile.tags = tags
  const extensions = parseList(v.extensions)
  if (extensions.length > 0) profile.extensions = extensions
  if (original?.machine_profile.expr_limits !== undefined) {
    profile.expr_limits = original.machine_profile.expr_limits
  }
  return profile
}

// The server joins every rule a request breaks, one per line, each starting
// with the name of the setting it concerns.
const SERVER_MESSAGE_FIELDS: Array<[RegExp, FieldKey]> = [
  [/^name\b/, 'name'],
  [/^a capacity group with that name/, 'name'],
  [/^kind\b/, 'kind'],
  [/^provider\b/, 'provider'],
  [/^min_machines\b/, 'minMachines'],
  [/^max_machines\b/, 'maxMachines'],
  [/^max_step_up\b/, 'maxStepUp'],
  [/^scale_up_cooldown/, 'cooldown'],
  [/^idle_timeout/, 'idle'],
  [/^boot_timeout/, 'boot'],
  [/^retire_action\b/, 'retireAction'],
  [/^a sized group's profile must declare cpu_count/, 'cpuCount'],
  [/^queue(_ids\b| ")/, 'queueIds'],
  [/^farm/, 'farm'],
]

/**
 * Sorts a server error message into the field each line is about; lines that
 * name no control come back in `form`.
 */
export function mapServerErrors(detail: string): { fields: FieldErrors; form: string[] } {
  const fields: FieldErrors = {}
  const form: string[] = []
  for (const raw of detail.split('\n')) {
    const line = raw.trim()
    if (line === '') continue
    const hit = SERVER_MESSAGE_FIELDS.find(([re]) => re.test(line))
    if (hit === undefined) {
      form.push(line)
      continue
    }
    const key = hit[1]
    fields[key] = fields[key] === undefined ? line : `${fields[key]} ${line}`
  }
  return { fields, form }
}
