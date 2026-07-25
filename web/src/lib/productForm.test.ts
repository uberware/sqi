// SPDX-License-Identifier: AGPL-3.0-or-later
import { describe, it, expect } from 'vitest'
import { selectWidget, paramLabel, isRequired, defaultJobName, initialValue } from './productForm'
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

describe('selectWidget', () => {
  it('honors explicit controls', () => {
    expect(
      selectWidget(
        param({
          user_interface: {
            control: 'DROPDOWN_LIST',
            label: '',
            group_label: '',
            decimals: null,
            single_step_removal: null,
          },
          allowed_values: ['a', 'b'],
        }),
      ),
    ).toBe('select')
    expect(
      selectWidget(
        param({
          user_interface: {
            control: 'CHECK_BOX',
            label: '',
            group_label: '',
            decimals: null,
            single_step_removal: null,
          },
          allowed_values: ['off', 'on'],
        }),
      ),
    ).toBe('checkbox')
    expect(
      selectWidget(
        param({
          user_interface: {
            control: 'MULTILINE_EDIT',
            label: '',
            group_label: '',
            decimals: null,
            single_step_removal: null,
          },
        }),
      ),
    ).toBe('textarea')
    expect(
      selectWidget(
        param({
          user_interface: {
            control: 'CHIP_INPUT',
            label: '',
            group_label: '',
            decimals: null,
            single_step_removal: null,
          },
        }),
      ),
    ).toBe('chips')
    expect(
      selectWidget(
        param({
          user_interface: {
            control: 'SPIN_BOX',
            label: '',
            group_label: '',
            decimals: null,
            single_step_removal: null,
          },
          type: 'INT',
        }),
      ),
    ).toBe('number')
    expect(
      selectWidget(
        param({
          user_interface: {
            control: 'HIDDEN',
            label: '',
            group_label: '',
            decimals: null,
            single_step_removal: null,
          },
        }),
      ),
    ).toBe('hidden')
  })

  it('renders chooser controls as text inputs until a picker exists', () => {
    for (const control of [
      'CHOOSE_INPUT_FILE',
      'CHOOSE_OUTPUT_FILE',
      'CHOOSE_DIRECTORY',
    ] as const) {
      expect(
        selectWidget(
          param({
            type: 'PATH',
            user_interface: {
              control,
              label: '',
              group_label: '',
              decimals: null,
              single_step_removal: null,
            },
          }),
        ),
      ).toBe('text')
    }
  })

  it('falls back by type when no userInterface', () => {
    expect(selectWidget(param({ allowed_values: ['a', 'b'] }))).toBe('select')
    expect(selectWidget(param({ type: 'INT' }))).toBe('number')
    expect(selectWidget(param({ type: 'FLOAT' }))).toBe('number')
    expect(selectWidget(param({ type: 'PATH' }))).toBe('text')
    expect(selectWidget(param({ type: 'STRING' }))).toBe('text')
  })
})

describe('helpers', () => {
  it('paramLabel prefers the UI label', () => {
    expect(
      paramLabel(
        param({
          name: 'Scene',
          user_interface: {
            control: 'LINE_EDIT',
            label: 'Scene file',
            group_label: '',
            decimals: null,
            single_step_removal: null,
          },
        }),
      ),
    ).toBe('Scene file')
    expect(paramLabel(param({ name: 'Scene' }))).toBe('Scene')
  })
  it('isRequired is true only with no default', () => {
    expect(isRequired(param({ default: null }))).toBe(true)
    expect(isRequired(param({ default: '5' }))).toBe(false)
  })
  it('initialValue uses the default or empty', () => {
    expect(initialValue(param({ default: 'x' }))).toBe('x')
    expect(initialValue(param({ default: null }))).toBe('')
  })
  it('defaultJobName combines title and timestamp', () => {
    const name = defaultJobName('Blender', new Date('2026-06-28T14:32:00Z'))
    expect(name.startsWith('Blender ')).toBe(true)
    expect(name.length).toBeGreaterThan('Blender '.length)
  })
})
