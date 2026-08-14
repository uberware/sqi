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
    item: null,
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

describe('validateParam — RFC 0007 types', () => {
  it('accepts a well-formed list', () => {
    expect(validateParam(param({ type: 'LIST[STRING]' }), '["a","b"]')).toBeNull()
    expect(validateParam(param({ type: 'LIST[INT]' }), '[1,2]')).toBeNull()
    expect(validateParam(param({ type: 'LIST[BOOL]' }), '[true]')).toBeNull()
  })

  it('rejects a value that is not a JSON array', () => {
    expect(validateParam(param({ type: 'LIST[STRING]' }), 'notalist')).toMatch(/list/i)
    expect(validateParam(param({ type: 'LIST[STRING]' }), '{"k":1}')).toMatch(/list/i)
  })

  it('enforces the element count', () => {
    const p = param({ type: 'LIST[STRING]', min_length: 2, max_length: 3 })
    expect(validateParam(p, '["a"]')).toMatch(/at least 2/)
    expect(validateParam(p, '["a","b","c","d"]')).toMatch(/at most 3/)
    expect(validateParam(p, '["a","b"]')).toBeNull()
  })

  it('enforces the element type', () => {
    expect(validateParam(param({ type: 'LIST[INT]' }), '["a"]')).toMatch(/item 1/)
    expect(validateParam(param({ type: 'LIST[INT]' }), '[1.5]')).toMatch(/item 1/)
    expect(validateParam(param({ type: 'LIST[FLOAT]' }), '["a"]')).toMatch(/item 1/)
  })

  it('names the offending row', () => {
    expect(validateParam(param({ type: 'LIST[INT]' }), '[1,2,"x"]')).toMatch(/item 3/)
  })

  it('enforces item bounds', () => {
    const p = param({
      type: 'LIST[INT]',
      item: {
        allowed_values: null,
        min_value: '2',
        max_value: '9',
        min_length: null,
        max_length: null,
        item: null,
      },
    })
    expect(validateParam(p, '[1]')).toMatch(/item 1/)
    expect(validateParam(p, '[10]')).toMatch(/item 1/)
    expect(validateParam(p, '[5]')).toBeNull()
  })

  it('enforces item allowed values', () => {
    const p = param({
      type: 'LIST[STRING]',
      item: {
        allowed_values: ['a', 'b'],
        min_value: null,
        max_value: null,
        min_length: null,
        max_length: null,
        item: null,
      },
    })
    expect(validateParam(p, '["z"]')).toMatch(/item 1/)
    expect(validateParam(p, '["a"]')).toBeNull()
  })

  it('requires a RANGE_EXPR to be non-empty but does not parse it', () => {
    // The <IntRangeExpr> grammar is the server's to enforce; duplicating it
    // here would be a second copy of a spec table in a language the Go tests
    // cannot check.
    expect(validateParam(param({ type: 'RANGE_EXPR', default: '' }), '')).toBeNull()
    expect(validateParam(param({ type: 'RANGE_EXPR' }), '1-10:2')).toBeNull()
    expect(validateParam(param({ type: 'RANGE_EXPR' }), 'not a range')).toBeNull()
  })

  it('does not second-guess a BOOL spelling', () => {
    expect(validateParam(param({ type: 'BOOL' }), 'yes')).toBeNull()
    expect(validateParam(param({ type: 'BOOL' }), 'true')).toBeNull()
  })
})
