// SPDX-License-Identifier: AGPL-3.0-or-later
import { describe, it, expect, vi } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import ProductParamField from './ProductParamField'
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

describe('ProductParamField', () => {
  it('renders a labeled text input and reports changes', () => {
    const onChange = vi.fn()
    render(<ProductParamField param={param({ name: 'Scene' })} value="" onChange={onChange} />)
    const input = screen.getByLabelText(/Scene/)
    fireEvent.change(input, { target: { value: '/a.blend' } })
    expect(onChange).toHaveBeenCalledWith('/a.blend')
  })

  it('renders a select for allowed values', () => {
    render(
      <ProductParamField
        param={param({ name: 'Q', allowed_values: ['draft', 'final'], default: 'final' })}
        value="final"
        onChange={vi.fn()}
      />,
    )
    expect(screen.getByRole('option', { name: 'draft' })).toBeInTheDocument()
  })

  it('omits HIDDEN params', () => {
    const { container } = render(
      <ProductParamField
        param={param({
          user_interface: {
            control: 'HIDDEN',
            label: '',
            group_label: '',
            decimals: null,
          },
        })}
        value=""
        onChange={vi.fn()}
      />,
    )
    expect(container).toBeEmptyDOMElement()
  })

  it('shows an error message', () => {
    render(
      <ProductParamField
        param={param({ name: 'Scene' })}
        value=""
        error="Scene is required"
        onChange={vi.fn()}
      />,
    )
    expect(screen.getByText('Scene is required')).toBeInTheDocument()
  })

  it('renders a list parameter through the row editor', () => {
    render(
      <ProductParamField
        param={param({ name: 'Cameras', type: 'LIST[STRING]' })}
        value='["main"]'
        onChange={vi.fn()}
      />,
    )
    expect(screen.getByRole('button', { name: /add item/i })).toBeInTheDocument()
    expect(screen.getByLabelText('Cameras item 1')).toHaveValue('main')
  })

  it('renders a BOOL parameter as a checkbox writing true/false', () => {
    // RFC 0007 forbids allowedValues on a BOOL, so the checkbox branch's
    // `allowed_values ?? ['false', 'true']` fallback is what supplies the pair.
    // Both spellings are ones the server's parseBoolParamValue accepts. This
    // asserts the fallback rather than assuming it.
    const onChange = vi.fn()
    render(
      <ProductParamField
        param={param({ name: 'UseGpu', type: 'BOOL', allowed_values: null })}
        value="false"
        onChange={onChange}
      />,
    )
    fireEvent.click(screen.getByRole('checkbox'))
    expect(onChange).toHaveBeenCalledWith('true')
  })

  it('still labels a list parameter and shows its error', () => {
    render(
      <ProductParamField
        param={param({ name: 'Cameras', type: 'LIST[STRING]' })}
        value='["main"]'
        error="must have at least 2 items"
        onChange={vi.fn()}
      />,
    )
    expect(screen.getByRole('alert')).toHaveTextContent('must have at least 2 items')
  })
})
