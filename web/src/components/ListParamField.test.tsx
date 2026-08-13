// SPDX-License-Identifier: AGPL-3.0-or-later
import { describe, it, expect, vi } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import ListParamField from './ListParamField'
import type { ProductParameter } from '@/api/types'

function param(over: Partial<ProductParameter>): ProductParameter {
  return {
    name: 'P',
    type: 'LIST[STRING]',
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

describe('ListParamField', () => {
  it('renders one input per element', () => {
    render(<ListParamField param={param({})} value='["a","b"]' onChange={vi.fn()} />)
    const rows = screen.getAllByRole('textbox')
    expect(rows).toHaveLength(2)
    expect(rows[0]).toHaveValue('a')
    expect(rows[1]).toHaveValue('b')
  })

  it('serialises an edit back to canonical JSON', () => {
    const onChange = vi.fn()
    render(<ListParamField param={param({})} value='["a","b"]' onChange={onChange} />)
    const [firstInput] = screen.getAllByRole('textbox')
    fireEvent.change(firstInput as HTMLElement, { target: { value: 'z' } })
    expect(onChange).toHaveBeenCalledWith('["z","b"]')
  })

  it('adds an element', () => {
    const onChange = vi.fn()
    render(<ListParamField param={param({})} value='["a"]' onChange={onChange} />)
    fireEvent.click(screen.getByRole('button', { name: /add/i }))
    expect(onChange).toHaveBeenCalledWith('["a",""]')
  })

  it('removes an element', () => {
    const onChange = vi.fn()
    render(<ListParamField param={param({})} value='["a","b"]' onChange={onChange} />)
    const [firstRemove] = screen.getAllByRole('button', { name: /remove/i })
    fireEvent.click(firstRemove as HTMLElement)
    expect(onChange).toHaveBeenCalledWith('["b"]')
  })

  it('starts from an empty list when the value is empty', () => {
    render(<ListParamField param={param({})} value="" onChange={vi.fn()} />)
    expect(screen.queryAllByRole('textbox')).toHaveLength(0)
    expect(screen.getByRole('button', { name: /add/i })).toBeInTheDocument()
  })

  // The encoding must match internal/openjd/paramjson.go's TestEncodeListDefault
  // table exactly: no spaces, elements as their JSON types.
  it('encodes numbers as numbers, not strings', () => {
    const onChange = vi.fn()
    render(<ListParamField param={param({ type: 'LIST[INT]' })} value="[1]" onChange={onChange} />)
    fireEvent.change(screen.getByLabelText('P item 1'), { target: { value: '2' } })
    expect(onChange).toHaveBeenCalledWith('[2]')
  })

  it('encodes booleans as booleans, not strings', () => {
    const onChange = vi.fn()
    render(
      <ListParamField param={param({ type: 'LIST[BOOL]' })} value="[false]" onChange={onChange} />,
    )
    fireEvent.click(screen.getByRole('checkbox'))
    expect(onChange).toHaveBeenCalledWith('[true]')
  })

  it('keeps a non-numeric draft as text rather than silently dropping it', () => {
    // A half-typed "-" or "1e" is not a number yet. Coercing it to 0 or
    // discarding it would fight the person typing.
    const onChange = vi.fn()
    render(<ListParamField param={param({ type: 'LIST[INT]' })} value="[1]" onChange={onChange} />)
    fireEvent.change(screen.getByLabelText('P item 1'), { target: { value: '-' } })
    expect(onChange).toHaveBeenCalledWith('["-"]')
  })

  it('falls back to a raw text field when the value is not valid JSON', () => {
    // A stored default must never be able to break the form.
    const onChange = vi.fn()
    render(<ListParamField param={param({})} value="not json" onChange={onChange} />)
    const raw = screen.getByRole('textbox')
    expect(raw).toHaveValue('not json')
    expect(screen.queryByRole('button', { name: /add/i })).not.toBeInTheDocument()
    fireEvent.change(raw, { target: { value: '["a"]' } })
    expect(onChange).toHaveBeenCalledWith('["a"]')
  })

  it('falls back to a raw text field when the JSON is not an array', () => {
    render(<ListParamField param={param({})} value='{"k":1}' onChange={vi.fn()} />)
    expect(screen.getByRole('textbox')).toHaveValue('{"k":1}')
  })
})
