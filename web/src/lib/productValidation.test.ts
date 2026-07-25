// SPDX-License-Identifier: AGPL-3.0-or-later
import { describe, it, expect } from 'vitest'
import { validateParam, validateAll } from './productValidation'
import type { ProductParameter } from '@/api/types'

function param(over: Partial<ProductParameter>): ProductParameter {
  return {
    name: 'P',
    type: 'STRING',
    description: '',
    default: null,
    allowed_values: null,
    min_value: null,
    max_value: null,
    min_length: null,
    max_length: null,
    object_type: '',
    data_flow: '',
    user_interface: null,
    file_filters: [],
    file_filter_default: null,
    ...over,
  }
}

describe('validateParam', () => {
  it('flags required empties', () => {
    expect(validateParam(param({ default: null }), '')).toMatch(/required/i)
    expect(validateParam(param({ default: '5' }), '')).toBeNull()
  })
  it('checks numeric type and range', () => {
    expect(validateParam(param({ type: 'INT', default: '0' }), 'x')).toMatch(/number/i)
    expect(
      validateParam(param({ type: 'INT', default: '0', min_value: '1', max_value: '10' }), '20'),
    ).toMatch(/at most/i)
    expect(validateParam(param({ type: 'INT', default: '0', min_value: '1' }), '0')).toMatch(
      /at least/i,
    )
    expect(validateParam(param({ type: 'INT', default: '0' }), '5')).toBeNull()
  })
  it('checks string length and allowed values', () => {
    expect(validateParam(param({ min_length: 2, default: '' }), 'a')).toMatch(/at least/i)
    expect(validateParam(param({ max_length: 2, default: '' }), 'abc')).toMatch(/at most/i)
    expect(validateParam(param({ allowed_values: ['a', 'b'], default: 'a' }), 'c')).toMatch(
      /one of/i,
    )
  })
})

describe('validateAll', () => {
  it('returns only failing params', () => {
    const params = [param({ name: 'A', default: null }), param({ name: 'B', default: '1' })]
    const errs = validateAll(params, { A: '', B: 'ok' })
    expect(errs).toHaveProperty('A')
    expect(errs).not.toHaveProperty('B')
  })
})
