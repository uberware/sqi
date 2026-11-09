// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest'
import {
  buildInput,
  defaultValues,
  formatTags,
  mapServerErrors,
  parseList,
  parseTags,
  validateValues,
  valuesFromGroup,
  wholeNumber,
} from './capacityGroupForm'
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
        'provider "wake_on_lan" is not available yet',
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
