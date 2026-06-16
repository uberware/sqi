// SPDX-License-Identifier: AGPL-3.0-or-later

import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { ThemeProvider, useTheme, resolveInitialTheme } from '@/theme/context'
import {
  installLocalStorageMock,
  setMatchMedia,
  resetThemeDom,
  type LocalStorageMock,
} from '@/theme/test-utils'

function TestConsumer() {
  const { theme, toggleTheme } = useTheme()
  return (
    <button type="button" onClick={toggleTheme}>
      theme:{theme}
    </button>
  )
}

let localStorageMock: LocalStorageMock

beforeEach(() => {
  localStorageMock = installLocalStorageMock()
  resetThemeDom()
})

afterEach(() => {
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('resolveInitialTheme', () => {
  it('returns dark when no saved pref and OS prefers dark', () => {
    setMatchMedia(true)
    expect(resolveInitialTheme()).toBe('dark')
  })

  it('returns light when no saved pref and OS prefers light', () => {
    setMatchMedia(false)
    expect(resolveInitialTheme()).toBe('light')
  })

  it('returns the saved pref even when OS prefers the opposite', () => {
    setMatchMedia(true)
    localStorage.setItem('sqi:theme', 'light')
    expect(resolveInitialTheme()).toBe('light')
  })

  it('falls through to OS pref when stored value is unrecognised', () => {
    setMatchMedia(true)
    localStorage.setItem('sqi:theme', 'purple')
    expect(resolveInitialTheme()).toBe('dark')
  })
})

describe('ThemeProvider / useTheme', () => {
  it('applies the resolved theme to <html> data-theme on mount', () => {
    setMatchMedia(true)
    render(
      <ThemeProvider>
        <TestConsumer />
      </ThemeProvider>,
    )
    expect(document.documentElement.dataset.theme).toBe('dark')
  })

  it('toggleTheme flips theme, persists to localStorage, and updates data-theme', async () => {
    setMatchMedia(false)
    const user = userEvent.setup()
    render(
      <ThemeProvider>
        <TestConsumer />
      </ThemeProvider>,
    )
    expect(screen.getByRole('button')).toHaveTextContent('theme:light')

    await user.click(screen.getByRole('button'))

    expect(screen.getByRole('button')).toHaveTextContent('theme:dark')
    expect(localStorage.getItem('sqi:theme')).toBe('dark')
    expect(document.documentElement.dataset.theme).toBe('dark')
  })

  it('toggle is bidirectional — second click returns to original theme', async () => {
    setMatchMedia(false)
    const user = userEvent.setup()
    render(
      <ThemeProvider>
        <TestConsumer />
      </ThemeProvider>,
    )

    await user.click(screen.getByRole('button'))
    expect(screen.getByRole('button')).toHaveTextContent('theme:dark')

    await user.click(screen.getByRole('button'))

    expect(screen.getByRole('button')).toHaveTextContent('theme:light')
    expect(localStorage.getItem('sqi:theme')).toBe('light')
    expect(document.documentElement.dataset.theme).toBe('light')
  })

  it('does not write to localStorage on mount', () => {
    setMatchMedia(false)
    render(
      <ThemeProvider>
        <TestConsumer />
      </ThemeProvider>,
    )

    expect(localStorageMock.setItem).not.toHaveBeenCalled()
  })

  it('useTheme throws when used outside ThemeProvider', () => {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {})
    expect(() => render(<TestConsumer />)).toThrow(/ThemeProvider/)
    spy.mockRestore()
  })
})
