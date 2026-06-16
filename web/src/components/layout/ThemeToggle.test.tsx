// SPDX-License-Identifier: AGPL-3.0-or-later

import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { ThemeProvider } from '@/theme/context'
import ThemeToggle from '@/components/layout/ThemeToggle'
import { installLocalStorageMock, setMatchMedia, resetThemeDom } from '@/theme/test-utils'

function renderToggle() {
  return render(
    <ThemeProvider>
      <ThemeToggle />
    </ThemeProvider>,
  )
}

beforeEach(() => {
  installLocalStorageMock()
  resetThemeDom()
})

afterEach(() => {
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('ThemeToggle', () => {
  it('renders a switch with an accessible name', () => {
    setMatchMedia(false)
    renderToggle()
    expect(screen.getByRole('switch', { name: /dark mode/i })).toBeInTheDocument()
  })

  it('reflects light theme with aria-checked=false', () => {
    setMatchMedia(false)
    renderToggle()
    expect(screen.getByRole('switch')).toHaveAttribute('aria-checked', 'false')
  })

  it('reflects dark theme with aria-checked=true', () => {
    setMatchMedia(true)
    renderToggle()
    expect(screen.getByRole('switch')).toHaveAttribute('aria-checked', 'true')
  })

  it('toggles theme on click', async () => {
    setMatchMedia(false)
    const user = userEvent.setup()
    renderToggle()
    const sw = screen.getByRole('switch')
    expect(sw).toHaveAttribute('aria-checked', 'false')
    await user.click(sw)
    expect(sw).toHaveAttribute('aria-checked', 'true')
    await user.click(sw)
    expect(sw).toHaveAttribute('aria-checked', 'false')
  })

  it('toggles theme via keyboard (Enter)', async () => {
    setMatchMedia(false)
    const user = userEvent.setup()
    renderToggle()
    const sw = screen.getByRole('switch')
    sw.focus()
    await user.keyboard('{Enter}')
    expect(sw).toHaveAttribute('aria-checked', 'true')
  })

  it('toggles theme via keyboard (Space)', async () => {
    setMatchMedia(false)
    const user = userEvent.setup()
    renderToggle()
    const sw = screen.getByRole('switch')
    sw.focus()
    await user.keyboard(' ')
    expect(sw).toHaveAttribute('aria-checked', 'true')
  })
})
