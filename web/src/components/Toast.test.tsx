// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, act, fireEvent } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ToastProvider, useToast } from './Toast'

// ── Helper components ─────────────────────────────────────────────────────────

function Trigger({ message }: { message: string }) {
  const { showToast } = useToast()
  return (
    <button type="button" onClick={() => showToast(message)}>
      Show toast
    </button>
  )
}

function TriggerWithSeverity({
  message,
  severity,
}: {
  message: string
  severity: 'success' | 'error' | 'info'
}) {
  const { showToast } = useToast()
  return (
    <button type="button" onClick={() => showToast(message, severity)}>
      Show toast
    </button>
  )
}

// ── Tests ─────────────────────────────────────────────────────────────────────

describe('ToastProvider / useToast', () => {
  afterEach(() => {
    vi.useRealTimers()
  })

  it('shows a toast message after showToast is called', async () => {
    const user = userEvent.setup()
    render(
      <ToastProvider>
        <Trigger message="Hello, world!" />
      </ToastProvider>,
    )

    await user.click(screen.getByRole('button', { name: 'Show toast' }))

    expect(screen.getByText('Hello, world!')).toBeInTheDocument()
  })

  it('applies the success severity class', async () => {
    const user = userEvent.setup()
    render(
      <ToastProvider>
        <TriggerWithSeverity message="Saved!" severity="success" />
      </ToastProvider>,
    )

    await user.click(screen.getByRole('button', { name: 'Show toast' }))

    const status = screen.getByRole('status')
    const toast = status.firstChild as HTMLElement
    expect(toast.className).toMatch(/toast--success/)
  })

  it('applies the error severity class', async () => {
    const user = userEvent.setup()
    render(
      <ToastProvider>
        <TriggerWithSeverity message="Something went wrong" severity="error" />
      </ToastProvider>,
    )

    await user.click(screen.getByRole('button', { name: 'Show toast' }))

    const status = screen.getByRole('status')
    const toast = status.firstChild as HTMLElement
    expect(toast.className).toMatch(/toast--error/)
  })

  it('applies the info severity class (default)', async () => {
    const user = userEvent.setup()
    render(
      <ToastProvider>
        <Trigger message="FYI" />
      </ToastProvider>,
    )

    await user.click(screen.getByRole('button', { name: 'Show toast' }))

    const status = screen.getByRole('status')
    const toast = status.firstChild as HTMLElement
    expect(toast.className).toMatch(/toast--info/)
  })

  it('dismisses a toast when the close button is clicked', async () => {
    const user = userEvent.setup()
    render(
      <ToastProvider>
        <Trigger message="Dismiss me" />
      </ToastProvider>,
    )

    await user.click(screen.getByRole('button', { name: 'Show toast' }))
    expect(screen.getByText('Dismiss me')).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'Dismiss notification' }))

    expect(screen.queryByText('Dismiss me')).not.toBeInTheDocument()
  })

  it('auto-dismisses after 5 000 ms', () => {
    vi.useFakeTimers()

    render(
      <ToastProvider>
        <Trigger message="Auto gone" />
      </ToastProvider>,
    )

    // fireEvent is synchronous — no async timing issues with fake timers.
    fireEvent.click(screen.getByRole('button', { name: 'Show toast' }))
    expect(screen.getByText('Auto gone')).toBeInTheDocument()

    act(() => {
      vi.advanceTimersByTime(5_001)
    })

    expect(screen.queryByText('Auto gone')).not.toBeInTheDocument()
  })

  it('stacks multiple simultaneous toasts', async () => {
    const user = userEvent.setup()
    render(
      <ToastProvider>
        <Trigger message="First" />
        <Trigger message="Second" />
      </ToastProvider>,
    )

    const triggers = screen.getAllByRole('button', { name: 'Show toast' })
    const [first, second] = triggers
    expect(first).toBeInTheDocument()
    expect(second).toBeInTheDocument()
    await user.click(first as HTMLElement)
    await user.click(second as HTMLElement)

    expect(screen.getByText('First')).toBeInTheDocument()
    expect(screen.getByText('Second')).toBeInTheDocument()
    expect(screen.getAllByRole('button', { name: 'Dismiss notification' })).toHaveLength(2)
  })

  it('throws when useToast is used outside ToastProvider', () => {
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {})

    function BadConsumer() {
      useToast()
      return null
    }

    expect(() => render(<BadConsumer />)).toThrow('useToast must be used inside <ToastProvider>')

    consoleError.mockRestore()
  })
})
