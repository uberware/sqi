// SPDX-License-Identifier: AGPL-3.0-or-later
import { useState } from 'react'
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

/** A stateful wrapper so a test can drive multiple keystrokes through the
 * same controlled-input cycle the real form uses: onChange feeds back into
 * value, and the component re-renders against that new value, exactly like
 * ProductParamField -> useState in the real form. A test that renders once
 * with a static value and a `vi.fn()` onChange cannot see a controlled-input
 * restore bug, because the input's `value` prop never actually changes. */
function ControlledListParamField({
  param: p,
  initialValue,
}: {
  param: ProductParameter
  initialValue: string
}) {
  const [value, setValue] = useState(initialValue)
  return <ListParamField param={p} id="id" value={value} onChange={setValue} />
}

describe('ListParamField', () => {
  it('renders one input per element', () => {
    render(<ListParamField param={param({})} id="id" value='["a","b"]' onChange={vi.fn()} />)
    const rows = screen.getAllByRole('textbox')
    expect(rows).toHaveLength(2)
    expect(rows[0]).toHaveValue('a')
    expect(rows[1]).toHaveValue('b')
  })

  it('serialises an edit back to canonical JSON', () => {
    const onChange = vi.fn()
    render(<ListParamField param={param({})} id="id" value='["a","b"]' onChange={onChange} />)
    const [firstInput] = screen.getAllByRole('textbox')
    fireEvent.change(firstInput as HTMLElement, { target: { value: 'z' } })
    expect(onChange).toHaveBeenCalledWith('["z","b"]')
  })

  it('adds an element', () => {
    const onChange = vi.fn()
    render(<ListParamField param={param({})} id="id" value='["a"]' onChange={onChange} />)
    fireEvent.click(screen.getByRole('button', { name: /add/i }))
    expect(onChange).toHaveBeenCalledWith('["a",""]')
  })

  it('removes an element', () => {
    const onChange = vi.fn()
    render(<ListParamField param={param({})} id="id" value='["a","b"]' onChange={onChange} />)
    const [firstRemove] = screen.getAllByRole('button', { name: /remove/i })
    fireEvent.click(firstRemove as HTMLElement)
    expect(onChange).toHaveBeenCalledWith('["b"]')
  })

  it('starts from an empty list when the value is empty', () => {
    render(<ListParamField param={param({})} id="id" value="" onChange={vi.fn()} />)
    expect(screen.queryAllByRole('textbox')).toHaveLength(0)
    expect(screen.getByRole('button', { name: /add/i })).toBeInTheDocument()
  })

  // The encoding must match internal/openjd/paramjson.go's TestEncodeListDefault
  // table exactly: no spaces, elements as their JSON types.
  it('encodes numbers as numbers, not strings', () => {
    const onChange = vi.fn()
    render(
      <ListParamField
        param={param({ type: 'LIST[INT]' })}
        id="id"
        value="[1]"
        onChange={onChange}
      />,
    )
    fireEvent.change(screen.getByLabelText('P item 1'), { target: { value: '2' } })
    expect(onChange).toHaveBeenCalledWith('[2]')
  })

  it('encodes floats as numbers, not strings', () => {
    // internal/openjd/paramjson_test.go TestEncodeListDefault's "floats" row:
    // JobParamTypeListFloat, []any{1.0, 2.5} -> `[1,2.5]`.
    const onChange = vi.fn()
    render(
      <ListParamField
        param={param({ type: 'LIST[FLOAT]' })}
        id="id"
        value="[1,2.5]"
        onChange={onChange}
      />,
    )
    fireEvent.change(screen.getByLabelText('P item 2'), { target: { value: '3.5' } })
    expect(onChange).toHaveBeenCalledWith('[1,3.5]')
  })

  it('encodes paths as their raw string form', () => {
    // internal/openjd/paramjson_test.go TestEncodeListDefault's "paths" row:
    // JobParamTypeListPath, []any{"/tmp/a", "/tmp/b"} -> `["/tmp/a","/tmp/b"]`.
    const onChange = vi.fn()
    render(
      <ListParamField
        param={param({ type: 'LIST[PATH]' })}
        id="id"
        value='["/tmp/a","/tmp/b"]'
        onChange={onChange}
      />,
    )
    const rows = screen.getAllByRole('textbox')
    expect(rows[0]).toHaveValue('/tmp/a')
    expect(rows[1]).toHaveValue('/tmp/b')
    fireEvent.change(screen.getByLabelText('P item 2'), { target: { value: '/tmp/c' } })
    expect(onChange).toHaveBeenCalledWith('["/tmp/a","/tmp/c"]')
  })

  it('encodes booleans as booleans, not strings', () => {
    const onChange = vi.fn()
    render(
      <ListParamField
        param={param({ type: 'LIST[BOOL]' })}
        id="id"
        value="[false]"
        onChange={onChange}
      />,
    )
    fireEvent.click(screen.getByRole('checkbox'))
    expect(onChange).toHaveBeenCalledWith('[true]')
  })

  it('renders a checkbox as checked for every accepted truthy spelling', () => {
    // parseBoolParamValue (internal/openjd/validate_paramtypes.go) accepts
    // true/1/1.0/"yes" (case-insensitive) as well as the literal `true` --
    // checked={element === true} only recognised the last of those.
    render(
      <ListParamField
        param={param({ type: 'LIST[BOOL]' })}
        id="id"
        value='[1,"yes"]'
        onChange={vi.fn()}
      />,
    )
    const checkboxes = screen.getAllByRole('checkbox')
    expect(checkboxes[0]).toBeChecked()
    expect(checkboxes[1]).toBeChecked()
  })

  it('keeps a non-numeric draft as text rather than silently dropping it', () => {
    // A half-typed "-" or "1e" is not a number yet. Coercing it to 0 or
    // discarding it would fight the person typing.
    const onChange = vi.fn()
    render(
      <ListParamField
        param={param({ type: 'LIST[INT]' })}
        id="id"
        value="[1]"
        onChange={onChange}
      />,
    )
    fireEvent.change(screen.getByLabelText('P item 1'), { target: { value: '-' } })
    expect(onChange).toHaveBeenCalledWith('["-"]')
  })

  it('lets a LIST[FLOAT] row accept a typed decimal point', () => {
    // Number("1.") is 1, not NaN, so a naive Number.isNaN(n) test treats "1."
    // as already-parsed. Because the rendered `value` prop is String(1) ===
    // "1", React's controlled-input restore then snaps the DOM back to "1",
    // eating the "." the user just typed -- asserting only the onChange
    // argument on a single event cannot see this, since that requires a real
    // render -> onChange -> re-render cycle.
    render(<ControlledListParamField param={param({ type: 'LIST[FLOAT]' })} initialValue="[1]" />)
    const input = screen.getByLabelText('P item 1')

    fireEvent.change(input, { target: { value: '1.' } })
    expect(input).toHaveValue('1.')

    fireEvent.change(input, { target: { value: '1.5' } })
    expect(input).toHaveValue('1.5')
  })

  it('falls back to a raw text field when the value is not valid JSON', () => {
    // A stored default must never be able to break the form.
    const onChange = vi.fn()
    render(<ListParamField param={param({})} id="id" value="not json" onChange={onChange} />)
    const raw = screen.getByRole('textbox')
    expect(raw).toHaveValue('not json')
    expect(screen.queryByRole('button', { name: /add/i })).not.toBeInTheDocument()
    fireEvent.change(raw, { target: { value: '["a"]' } })
    expect(onChange).toHaveBeenCalledWith('["a"]')
  })

  it('falls back to a raw text field when the JSON is not an array', () => {
    render(<ListParamField param={param({})} id="id" value='{"k":1}' onChange={vi.fn()} />)
    expect(screen.getByRole('textbox')).toHaveValue('{"k":1}')
  })

  it('falls back to a raw text field when an element is not a scalar', () => {
    // A stored default can never contain a non-scalar element
    // (encodeListDefault rejects them), but a hand-edited or otherwise
    // malformed value could. Laundering `null` through String() would
    // silently rewrite it every time an unrelated row is edited.
    render(<ListParamField param={param({})} id="id" value='["a",null]' onChange={vi.fn()} />)
    expect(screen.getByRole('textbox')).toHaveValue('["a",null]')
  })

  it('associates the group with the field label via aria-labelledby', () => {
    render(<ListParamField param={param({})} id="field-label" value='["a"]' onChange={vi.fn()} />)
    expect(screen.getByRole('group')).toHaveAttribute('aria-labelledby', 'field-label')
  })

  it('names rows and remove buttons by the parameter label, not the raw name', () => {
    render(
      <ListParamField
        param={param({
          name: 'Cameras',
          user_interface: {
            control: '',
            label: 'Cameras to render',
            group_label: '',
            decimals: null,
          },
        })}
        id="id"
        value='["main"]'
        onChange={vi.fn()}
      />,
    )
    expect(screen.getByLabelText('Cameras to render item 1')).toBeInTheDocument()
    expect(
      screen.getByRole('button', { name: 'Remove Cameras to render item 1' }),
    ).toBeInTheDocument()
  })
})
