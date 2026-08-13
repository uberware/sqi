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
})
