// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest'
import {
  buildInput,
  defaultValues,
  formatTags,
  mapServerErrors,
  parseList,
  parseTags,
  providerForKind,
  providersFor,
  validateValues,
  valuesFromGroup,
  wholeNumber,
} from './capacityGroupForm'
import type { CapacityProvider } from '@/api/types'
import { group } from '@/test/capacityGroups'

describe('wholeNumber', () => {
  it.each([
    ['7', 7],
    [' 12 ', 12],
    ['', undefined],
    ['1.5', undefined],
    ['abc', undefined],
  ])('%j -> %j', (input, want) => {
    expect(wholeNumber(input)).toBe(want)
  })
})

describe('tags', () => {
  it('round-trips key=value lines', () => {
    const { tags } = parseTags('gpu=a100\n\n zone = us \n')
    expect(tags).toEqual({ gpu: 'a100', zone: 'us' })
    expect(formatTags(tags)).toBe('gpu=a100\nzone=us')
  })

  it('reports a line that is not key=value', () => {
    expect(parseTags('gpu').error).toMatch(/not key=value/)
    expect(parseTags('=x').error).toMatch(/not key=value/)
  })

  it('formats no tags as empty text', () => {
    expect(formatTags(undefined)).toBe('')
  })
})

describe('parseList', () => {
  it('splits on commas and whitespace and drops blanks', () => {
    expect(parseList('EXPR, SQI_LOG_CHECKS  X,,')).toEqual(['EXPR', 'SQI_LOG_CHECKS', 'X'])
  })
})

describe('validateValues', () => {
  const valid = { ...defaultValues('farm-1'), name: 'x', cpuCount: '4' }

  it('accepts a complete sized group', () => {
    expect(validateValues(valid)).toEqual({})
  })

  it('requires a farm, a name, max machines and a CPU count for a sized group', () => {
    const errors = validateValues({
      ...valid,
      farmId: '',
      name: ' ',
      maxMachines: '0',
      cpuCount: '',
    })
    expect(Object.keys(errors).sort()).toEqual(['cpuCount', 'farmId', 'maxMachines', 'name'])
  })

  it('does not ask a named group for a profile', () => {
    expect(validateValues({ ...valid, kind: 'named', cpuCount: '', tags: 'bad' })).toEqual({})
  })

  it('rejects fractional or negative optional numbers and malformed tags', () => {
    const errors = validateValues({ ...valid, ramMb: '1.5', gpuCount: '-1', tags: 'oops' })
    expect(Object.keys(errors).sort()).toEqual(['gpuCount', 'ramMb', 'tags'])
  })
})

const PROVIDERS: CapacityProvider[] = [
  { name: 'external', kinds: ['named', 'sized'] },
  { name: 'wake_on_lan', kinds: ['named'] },
  { name: 'script:burst', kinds: ['sized'] },
  { name: 'script:both', kinds: ['named', 'sized'] },
]

describe('providersFor', () => {
  it.each([
    ['named', ['external', 'wake_on_lan', 'script:both']],
    ['sized', ['external', 'script:burst', 'script:both']],
  ] as const)('offers %s groups %j', (kind, want) => {
    expect(providersFor(kind, PROVIDERS).map((p) => p.name)).toEqual(want)
  })
})

describe('providerForKind', () => {
  it.each([
    ['wake_on_lan', 'sized', 'external'],
    ['script:burst', 'named', 'external'],
    ['script:both', 'named', 'script:both'],
    ['external', 'named', 'external'],
    ['script:gone', 'sized', 'external'],
  ] as const)('keeps %s only if a %s group may name it', (provider, kind, want) => {
    expect(providerForKind(provider, kind, PROVIDERS)).toBe(want)
  })

  it('falls back to external for anything but external while the list is unknown', () => {
    expect(providerForKind('wake_on_lan', 'named', undefined)).toBe('external')
    expect(providerForKind('external', 'sized', undefined)).toBe('external')
  })
})

describe('validateValues provider', () => {
  const valid = { ...defaultValues('farm-1'), name: 'x', cpuCount: '4' }

  it.each(['external', 'script:burst', 'script:both'])(
    'accepts %s for a sized group',
    (provider) => {
      expect(
        validateValues({ ...valid, provider }, { providers: PROVIDERS }).provider,
      ).toBeUndefined()
    },
  )

  it.each(['script:nope', 'wake_on_lan'])('refuses %s for a sized group', (provider) => {
    const message = validateValues({ ...valid, provider }, { providers: PROVIDERS }).provider
    expect(message).toContain(provider)
  })

  it('accepts the provider the group already has even when it is no longer offered', () => {
    const errors = validateValues(
      { ...valid, provider: 'script:gone' },
      { providers: PROVIDERS, original: 'script:gone' },
    )
    expect(errors.provider).toBeUndefined()
  })

  it('accepts only external or the original while the list is unknown', () => {
    expect(
      validateValues({ ...valid, provider: 'external' }, { providers: undefined }).provider,
    ).toBeUndefined()
    expect(
      validateValues({ ...valid, provider: 'script:burst' }, { providers: undefined }).provider,
    ).toBeDefined()
    expect(
      validateValues(
        { ...valid, provider: 'script:burst' },
        { providers: undefined, original: 'script:burst' },
      ).provider,
    ).toBeUndefined()
  })
})

describe('buildInput', () => {
  it('carries farm_id on create and not on edit', () => {
    const v = { ...defaultValues('farm-1'), name: ' x ', cpuCount: '4' }
    expect(buildInput(v)).toHaveProperty('farm_id', 'farm-1')
    expect(buildInput(v, group())).not.toHaveProperty('farm_id')
    expect(buildInput(v).name).toBe('x')
  })

  it('round-trips a loaded group, keeping EXPR limits the form does not edit', () => {
    const g = group({
      machine_profile: { cpu_count: 8, tags: { a: 'b' }, expr_limits: { memory_limit: 9 } },
    })
    const input = buildInput(valuesFromGroup(g), g)
    expect(input.machine_profile).toEqual({
      cpu_count: 8,
      tags: { a: 'b' },
      expr_limits: { memory_limit: 9 },
    })
    expect(input.enabled).toBe(true)
    expect(input.queue_ids).toEqual([])
  })

  it('hands a named group back the profile it loaded', () => {
    const g = group({ kind: 'named', machine_profile: { cpu_count: 2 } })
    expect(buildInput(valuesFromGroup(g), g).machine_profile).toEqual({ cpu_count: 2 })
    expect(buildInput({ ...defaultValues('f'), kind: 'named' }).machine_profile).toEqual({})
  })
})

describe('mapServerErrors', () => {
  it('assigns each line to the field it names', () => {
    const { fields, form } = mapServerErrors(
      [
        'name "x y" must be 1-63 characters',
        'min_machines 9 must not exceed max_machines 4',
        'idle_timeout 5s must be at least 1m0s',
        "a sized group's profile must declare cpu_count of at least 1",
        'queue "q" not found',
        'farm not found',
        'provider "script:burst" is not configured: add it under capacity.providers in the server\'s configuration file',
        'invalid JSON body: json: unknown field "x"',
      ].join('\n'),
    )
    expect(Object.keys(fields).sort()).toEqual([
      'cpuCount',
      'farmId',
      'idle',
      'minMachines',
      'name',
      'provider',
      'queueIds',
    ])
    expect(form).toEqual(['invalid JSON body: json: unknown field "x"'])
  })

  it('joins two lines about one field', () => {
    const { fields } = mapServerErrors('max_machines 0 must be at least 1\nmax_machines 0 again')
    expect(fields.maxMachines).toBe('max_machines 0 must be at least 1 max_machines 0 again')
  })

  it('maps a name conflict to the name field', () => {
    expect(mapServerErrors('a capacity group with that name already exists').fields.name).toMatch(
      /already exists/,
    )
  })
})
