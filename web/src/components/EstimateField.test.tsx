// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import EstimateField from './EstimateField'

const fetchMock = vi.fn<typeof fetch>()
beforeEach(() => {
  fetchMock.mockReset()
  fetchMock.mockImplementation(() =>
    Promise.resolve(
      new Response(JSON.stringify({ id: 'j1' }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    ),
  )
  vi.stubGlobal('fetch', fetchMock)
})
afterEach(() => {
  vi.unstubAllGlobals()
})

function Wrapper({ children }: { children: ReactNode }) {
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } })
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>
}

function lastBody(): unknown {
  const call = fetchMock.mock.calls.at(-1)
  const init = call?.[1]
  return typeof init?.body === 'string' ? JSON.parse(init.body) : undefined
}

describe('EstimateField', () => {
  it('is read-only without editable', () => {
    render(<EstimateField jobId="j1" seconds={3600} editable={false} />, { wrapper: Wrapper })
    expect(screen.getByText('1h 0m')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /edit/i })).not.toBeInTheDocument()
  })

  it('shows a dash with no estimate', () => {
    render(<EstimateField jobId="j1" seconds={undefined} editable={false} />, { wrapper: Wrapper })
    expect(screen.getByText('—')).toBeInTheDocument()
  })

  it('saves a parsed estimate', async () => {
    const user = userEvent.setup()
    render(<EstimateField jobId="j1" seconds={3600} editable />, { wrapper: Wrapper })
    await user.click(screen.getByRole('button', { name: /edit/i }))
    const input = screen.getByLabelText('Estimated run time')
    await user.clear(input)
    await user.type(input, '1h 30m')
    await user.click(screen.getByRole('button', { name: /save/i }))
    await waitFor(() => expect(fetchMock).toHaveBeenCalled())
    expect(lastBody()).toEqual({ estimated_seconds: 5400 })
    expect(String(fetchMock.mock.calls[0]?.[0])).toContain('/jobs/j1')
    expect(fetchMock.mock.calls[0]?.[1]?.method).toBe('PATCH')
  })

  it('clears the estimate with an empty input', async () => {
    const user = userEvent.setup()
    render(<EstimateField jobId="j1" seconds={3600} editable />, { wrapper: Wrapper })
    await user.click(screen.getByRole('button', { name: /edit/i }))
    await user.clear(screen.getByLabelText('Estimated run time'))
    await user.click(screen.getByRole('button', { name: /save/i }))
    await waitFor(() => expect(fetchMock).toHaveBeenCalled())
    expect(lastBody()).toEqual({ estimated_seconds: null })
  })

  it('rejects unparseable input without a request', async () => {
    const user = userEvent.setup()
    render(<EstimateField jobId="j1" seconds={undefined} editable />, { wrapper: Wrapper })
    await user.click(screen.getByRole('button', { name: /edit/i }))
    await user.type(screen.getByLabelText('Estimated run time'), 'abc')
    await user.click(screen.getByRole('button', { name: /save/i }))
    expect(screen.getByText('Use h, m or s, e.g. 1h 30m')).toBeInTheDocument()
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('cancels without a request', async () => {
    const user = userEvent.setup()
    render(<EstimateField jobId="j1" seconds={3600} editable />, { wrapper: Wrapper })
    await user.click(screen.getByRole('button', { name: /edit/i }))
    await user.click(screen.getByRole('button', { name: /cancel/i }))
    expect(screen.getByText('1h 0m')).toBeInTheDocument()
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('prefills exactly and saving unchanged text keeps the stored value', async () => {
    const user = userEvent.setup()
    render(<EstimateField jobId="j1" seconds={3661} editable />, { wrapper: Wrapper })
    await user.click(screen.getByRole('button', { name: /edit/i }))
    expect(screen.getByLabelText('Estimated run time')).toHaveValue('1h 1m 1s')
    await user.click(screen.getByRole('button', { name: /save/i }))
    await waitFor(() => expect(fetchMock).toHaveBeenCalled())
    expect(lastBody()).toEqual({ estimated_seconds: 3661 })
  })

  it('rejects an out-of-range value with the hint', async () => {
    const user = userEvent.setup()
    render(<EstimateField jobId="j1" seconds={undefined} editable />, { wrapper: Wrapper })
    await user.click(screen.getByRole('button', { name: /edit/i }))
    await user.type(screen.getByLabelText('Estimated run time'), '99999999999h')
    await user.click(screen.getByRole('button', { name: /save/i }))
    expect(screen.getByText('Use h, m or s, e.g. 1h 30m')).toBeInTheDocument()
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('moves focus into the input on edit and back to Edit on cancel', async () => {
    const user = userEvent.setup()
    render(<EstimateField jobId="j1" seconds={3600} editable />, { wrapper: Wrapper })
    await user.click(screen.getByRole('button', { name: /edit/i }))
    expect(screen.getByLabelText('Estimated run time')).toHaveFocus()
    await user.click(screen.getByRole('button', { name: /cancel/i }))
    expect(screen.getByRole('button', { name: /edit/i })).toHaveFocus()
  })

  it('returns focus to Edit after a successful save', async () => {
    const user = userEvent.setup()
    render(<EstimateField jobId="j1" seconds={3600} editable />, { wrapper: Wrapper })
    await user.click(screen.getByRole('button', { name: /edit/i }))
    await user.click(screen.getByRole('button', { name: /save/i }))
    await waitFor(() => expect(screen.getByRole('button', { name: /edit/i })).toHaveFocus())
  })
})
