// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import ErrorBanner from './ErrorBanner'

describe('ErrorBanner', () => {
  it('renders its message as an alert', () => {
    render(<ErrorBanner>Failed to load farms: boom</ErrorBanner>)
    expect(screen.getByRole('alert')).toHaveTextContent('Failed to load farms: boom')
  })
})
