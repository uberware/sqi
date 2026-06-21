// SPDX-License-Identifier: AGPL-3.0-or-later

import { render } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { Spinner, Trash, X } from '@/components/icons'

describe('icon module contract', () => {
  it('renders a decorative svg hidden from assistive tech', () => {
    const { container } = render(<X />)
    const svg = container.querySelector('svg')
    expect(svg).not.toBeNull()
    expect(svg).toHaveAttribute('aria-hidden', 'true')
    expect(svg).toHaveAttribute('focusable', 'false')
    expect(svg).toHaveAttribute('viewBox', '0 0 24 24')
  })

  it('defaults to a 16px square and honors the size prop', () => {
    const { container, rerender } = render(<Trash />)
    let svg = container.querySelector('svg')
    expect(svg).toHaveAttribute('width', '16')
    expect(svg).toHaveAttribute('height', '16')

    rerender(<Trash size={24} />)
    svg = container.querySelector('svg')
    expect(svg).toHaveAttribute('width', '24')
    expect(svg).toHaveAttribute('height', '24')
  })

  it('forwards a className onto the svg', () => {
    const { container } = render(<Trash className="custom" />)
    expect(container.querySelector('svg')).toHaveClass('custom')
  })

  it('Spinner merges its animation class with a passed className', () => {
    const { container } = render(<Spinner className="custom" />)
    const svg = container.querySelector('svg')
    expect(svg).toHaveClass('custom')
    // The component-owned animation class is also present (CSS-module hashed).
    expect(svg?.classList.length).toBeGreaterThan(1)
  })
})
