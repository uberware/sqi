// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import DiagnosticsPanel from '@/components/DiagnosticsPanel'
import * as api from '@/api/diagnostics'
import type { UseQueryResult } from '@tanstack/react-query'
import type { DiagnosticLogsResponse } from '@/api/diagnostics'

vi.mock('@/ws/context', () => ({ useWebSocket: () => {} }))

vi.mock('@/api/diagnostics', async (importOriginal) => {
  const orig = await importOriginal<typeof import('@/api/diagnostics')>()
  return { ...orig, useDiagnosticsLogs: vi.fn() }
})

function makeQueryResult(
  overrides: Partial<UseQueryResult<DiagnosticLogsResponse>>,
): UseQueryResult<DiagnosticLogsResponse> {
  return {
    data: undefined,
    isLoading: false,
    isError: false,
    isPending: false,
    isFetching: false,
    isSuccess: false,
    isLoadingError: false,
    isRefetchError: false,
    isPlaceholderData: false,
    isStale: false,
    isRefetching: false,
    isPaused: false,
    status: 'pending',
    fetchStatus: 'idle',
    error: null,
    dataUpdatedAt: 0,
    errorUpdatedAt: 0,
    failureCount: 0,
    failureReason: null,
    errorUpdateCount: 0,
    refetch: vi.fn(),
    promise: Promise.resolve({ records: [] }),
    ...overrides,
  } as UseQueryResult<DiagnosticLogsResponse>
}

function renderPanel(component: string) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={qc}>
      <DiagnosticsPanel component={component} title="Server log" />
    </QueryClientProvider>,
  )
}

describe('DiagnosticsPanel', () => {
  it('renders fetched records with level and message', async () => {
    vi.mocked(api.useDiagnosticsLogs).mockReturnValue(
      makeQueryResult({
        data: {
          records: [
            {
              ts: '2026-06-17T12:00:00Z',
              component: 'server',
              level: 'ERROR',
              msg: 'process not found',
            },
          ],
        },
        isSuccess: true,
        status: 'success',
      }),
    )
    renderPanel('server')
    expect(await screen.findByText('process not found')).toBeInTheDocument()
    expect(screen.getByText('ERROR')).toBeInTheDocument()
  })

  it('queries all components and skips the component filter when component is empty', async () => {
    vi.mocked(api.useDiagnosticsLogs).mockReturnValue(
      makeQueryResult({
        data: {
          records: [
            {
              ts: '2026-06-17T12:00:00Z',
              component: 'worker:w1',
              level: 'ERROR',
              msg: 'all-components hit',
            },
          ],
        },
        isSuccess: true,
        status: 'success',
      }),
    )
    renderPanel('')
    // The record from a non-matching component is still rendered.
    expect(await screen.findByText('all-components hit')).toBeInTheDocument()
    // The REST params omit `component` entirely so the backend returns all.
    const lastCall = vi.mocked(api.useDiagnosticsLogs).mock.calls.at(-1)
    expect(lastCall?.[0]).not.toHaveProperty('component')
  })

  it('shows an empty state when there are no records', async () => {
    vi.mocked(api.useDiagnosticsLogs).mockReturnValue(
      makeQueryResult({
        data: { records: [] },
        isSuccess: true,
        status: 'success',
      }),
    )
    renderPanel('server')
    expect(await screen.findByText(/no diagnostic logs/i)).toBeInTheDocument()
  })
})
