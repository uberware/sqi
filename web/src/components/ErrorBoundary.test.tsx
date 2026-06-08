import { render, screen, fireEvent } from '@testing-library/react'
import type { ReactNode } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ErrorBoundary from '@/components/ErrorBoundary'

function ThrowingChild({ shouldThrow }: { shouldThrow: boolean }) {
  if (shouldThrow) throw new Error('Test render error')
  return <div>Child content</div>
}

describe('ErrorBoundary', () => {
  beforeEach(() => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('renders children when no error occurs', () => {
    render(
      <ErrorBoundary>
        <div>Normal content</div>
      </ErrorBoundary>,
    )
    expect(screen.getByText('Normal content')).toBeInTheDocument()
  })

  it('renders the error alert when a child throws', () => {
    render(
      <ErrorBoundary>
        <ThrowingChild shouldThrow={true} />
      </ErrorBoundary>,
    )
    expect(screen.getByRole('alert')).toBeInTheDocument()
  })

  it('displays the default fallback title', () => {
    render(
      <ErrorBoundary>
        <ThrowingChild shouldThrow={true} />
      </ErrorBoundary>,
    )
    expect(screen.getByText('Something went wrong')).toBeInTheDocument()
  })

  it('displays a custom fallback title when provided', () => {
    render(
      <ErrorBoundary fallbackTitle="View crashed">
        <ThrowingChild shouldThrow={true} />
      </ErrorBoundary>,
    )
    expect(screen.getByText('View crashed')).toBeInTheDocument()
  })

  it('displays the error message', () => {
    render(
      <ErrorBoundary>
        <ThrowingChild shouldThrow={true} />
      </ErrorBoundary>,
    )
    expect(screen.getByText('Test render error')).toBeInTheDocument()
  })

  it('normalizes non-Error thrown values to a string message', () => {
    function StringThrower(): ReactNode {
      throw 'plain string error'
    }
    render(
      <ErrorBoundary>
        <StringThrower />
      </ErrorBoundary>,
    )
    expect(screen.getByText('plain string error')).toBeInTheDocument()
  })

  it('shows a "Reload this section" recovery button', () => {
    render(
      <ErrorBoundary>
        <ThrowingChild shouldThrow={true} />
      </ErrorBoundary>,
    )
    expect(screen.getByRole('button', { name: 'Reload this section' })).toBeInTheDocument()
  })

  it('resets and renders children after clicking "Reload this section"', () => {
    let shouldThrow = true

    function TogglableChild() {
      if (shouldThrow) throw new Error('Toggle error')
      return <div>Recovered</div>
    }

    render(
      <ErrorBoundary>
        <TogglableChild />
      </ErrorBoundary>,
    )

    expect(screen.getByRole('alert')).toBeInTheDocument()

    shouldThrow = false
    fireEvent.click(screen.getByRole('button', { name: 'Reload this section' }))

    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(screen.getByText('Recovered')).toBeInTheDocument()
  })
})
