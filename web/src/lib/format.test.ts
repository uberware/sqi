// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect } from 'vitest'
import { detectFormat } from './format'

describe('detectFormat', () => {
  it('returns json when the body starts with an object', () => {
    expect(detectFormat('  {"name":"x"}')).toBe('json')
  })

  it('returns json when the body starts with an array', () => {
    expect(detectFormat('[1, 2]')).toBe('json')
  })

  it('returns yaml for YAML content', () => {
    expect(detectFormat('name: x')).toBe('yaml')
  })

  it('returns yaml for an empty string', () => {
    expect(detectFormat('')).toBe('yaml')
  })
})
