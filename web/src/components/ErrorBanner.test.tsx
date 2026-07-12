// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import ErrorBanner from './ErrorBanner'

describe('ErrorBanner', () => {
  it('renders its message as an alert', () => {
    render(<ErrorBanner>Failed to load farms: boom</ErrorBanner>)
    expect(screen.getByRole('alert')).toHaveTextContent('Failed to load farms: boom')
  })

  it('renders element children in the warning variant as an alert', () => {
    render(
      <ErrorBanner variant="warning">
        <span>Auto-parked — too many failures</span>
        <button type="button">Resume</button>
      </ErrorBanner>,
    )
    const alert = screen.getByRole('alert')
    expect(alert).toHaveTextContent('Auto-parked — too many failures')
    expect(screen.getByRole('button', { name: 'Resume' })).toBeInTheDocument()
    expect(alert.className).toMatch(/warning/)
  })

  it('uses the error palette by default', () => {
    render(<ErrorBanner>boom</ErrorBanner>)
    expect(screen.getByRole('alert').className).toMatch(/error/)
  })
})
