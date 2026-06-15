// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect } from 'vitest'
import { render } from '@testing-library/react'
import UsageBar from './UsageBar'

function fillWidth(container: HTMLElement): string {
  return (container.querySelector('span > span') as HTMLElement).style.width
}

describe('UsageBar', () => {
  it('sizes the fill to the usage fraction', () => {
    expect(fillWidth(render(<UsageBar inUse={2} max={5} />).container)).toBe('40%')
  })

  it('renders an empty bar at zero usage', () => {
    expect(fillWidth(render(<UsageBar inUse={0} max={5} />).container)).toBe('0%')
  })

  it('clamps the fill at full when over capacity', () => {
    expect(fillWidth(render(<UsageBar inUse={10} max={5} />).container)).toBe('100%')
  })

  it('renders an empty bar when max is zero or negative', () => {
    expect(fillWidth(render(<UsageBar inUse={3} max={0} />).container)).toBe('0%')
    expect(fillWidth(render(<UsageBar inUse={3} max={-1} />).container)).toBe('0%')
  })

  it('merges an extra className onto the track', () => {
    const { container } = render(<UsageBar inUse={1} max={2} className="extra" />)
    expect((container.firstElementChild as HTMLElement).className).toContain('extra')
  })
})
