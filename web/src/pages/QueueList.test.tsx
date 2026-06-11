// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { ToastProvider } from '@/components/Toast'
import QueueList from './QueueList'

const fetchMock = vi.fn<typeof fetch>()
beforeEach(() => {
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
})
afterEach(() => vi.restoreAllMocks())

function ok(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <ToastProvider>
          <QueueList />
        </ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('QueueList', () => {
  it('renders queues grouped under their farm', async () => {
    fetchMock.mockResolvedValueOnce(
      ok([
        {
          id: 'farm-1',
          name: 'render',
          max_concurrent_tasks: 0,
          created_at: '2026-01-01T00:00:00Z',
          updated_at: '2026-01-01T00:00:00Z',
        },
      ]),
    )
    fetchMock.mockResolvedValueOnce(
      ok({
        items: [
          {
            id: 'queue-1',
            farm_id: 'farm-1',
            name: 'lighting',
            priority: 50,
            max_concurrent_tasks: 0,
            paused: false,
            created_at: '2026-01-01T00:00:00Z',
            updated_at: '2026-01-01T00:00:00Z',
          },
        ],
        total: 1,
        limit: 1000,
        offset: 0,
      }),
    )
    renderPage()
    expect(await screen.findByRole('link', { name: 'lighting' })).toBeInTheDocument()
    expect(screen.getByText('render')).toBeInTheDocument()
  })

  it('has a New Queue action linking to /queues/new', async () => {
    fetchMock.mockResolvedValueOnce(ok([]))
    renderPage()
    const link = await screen.findByRole('link', { name: /new queue/i })
    expect(link.getAttribute('href')).toBe('/queues/new')
  })
})
