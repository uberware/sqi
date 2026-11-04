// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, within, act } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router'
import Servers from './Servers'
import type { ServerMember, ServersResponse } from '@/api/types'

const fetchMock = vi.fn<typeof fetch>()

beforeEach(() => {
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
})
afterEach(() => {
  vi.useRealTimers()
  vi.restoreAllMocks()
})

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': status >= 400 ? 'application/problem+json' : 'application/json' },
  })
}

function member(overrides: Partial<ServerMember> = {}): ServerMember {
  return {
    instance_id: 'i-1',
    name: 'sqi-a',
    host: 'host-a',
    pid: 100,
    version: 'v0.9.0',
    role: 'leader',
    started_at: '2026-11-03T08:00:00Z',
    last_seen_seconds_ago: 2,
    broker_ok: true,
    ...overrides,
  }
}

function response(overrides: Partial<ServersResponse> = {}): ServersResponse {
  return { ha: true, epoch: 7, servers: [member()], ...overrides }
}

/** The table body rows, in document order. */
function bodyRows(): HTMLElement[] {
  const [, ...rows] = within(screen.getByRole('table', { name: 'Servers' })).getAllByRole('row')
  return rows
}

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <Servers />
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('Servers', () => {
  it('lists servers with the leader first and a role badge for each', async () => {
    fetchMock.mockResolvedValueOnce(
      json(
        response({
          servers: [
            member(),
            member({ instance_id: 'i-2', name: 'sqi-b', host: 'host-b', role: 'standby' }),
          ],
        }),
      ),
    )
    renderPage()

    await screen.findByText('sqi-b')
    const [first, second, ...rest] = bodyRows()
    expect(rest).toHaveLength(0)
    if (!first || !second) throw new Error('expected two rows')
    expect(within(first).getByText('sqi-a')).toBeInTheDocument()
    expect(within(first).getByText('Leader')).toBeInTheDocument()
    expect(within(first).getByText('host-a')).toBeInTheDocument()
    expect(within(first).getByText('v0.9.0')).toBeInTheDocument()
    expect(within(first).getByText('2 s ago')).toBeInTheDocument()
    expect(within(first).getByText('ok')).toBeInTheDocument()
    expect(within(second).getByText('sqi-b')).toBeInTheDocument()
    expect(within(second).getByText('Standby')).toBeInTheDocument()
    expect(screen.getByText(/epoch 7/i)).toBeInTheDocument()
  })

  it('shows "missing" servers with how long ago they were last seen', async () => {
    fetchMock.mockResolvedValueOnce(
      json(
        response({
          servers: [
            member(),
            member({
              instance_id: 'i-3',
              name: 'sqi-c',
              role: 'missing',
              last_seen_seconds_ago: 125,
              broker_ok: false,
            }),
          ],
        }),
      ),
    )
    renderPage()

    const row = (await screen.findByText('sqi-c')).closest('tr')
    if (!row) throw new Error('expected a row for sqi-c')
    expect(within(row).getByText('Missing')).toBeInTheDocument()
    expect(within(row).getByText('2 min ago')).toBeInTheDocument()
    expect(within(row).getByText('unreachable')).toBeInTheDocument()
  })

  it('reports a long-missing server in hours', async () => {
    fetchMock.mockResolvedValueOnce(
      json(
        response({
          servers: [
            member({ name: 'sqi-d', role: 'missing', last_seen_seconds_ago: 3 * 3600 + 5 }),
          ],
        }),
      ),
    )
    renderPage()
    expect(await screen.findByText('3 h ago')).toBeInTheDocument()
  })

  it('explains high availability is off when ha is false', async () => {
    fetchMock.mockResolvedValueOnce(json(response({ ha: false, epoch: 0 })))
    renderPage()

    expect(
      await screen.findByText('High availability is off. This server runs alone.'),
    ).toBeInTheDocument()
    expect(screen.getByText('sqi-a')).toBeInTheDocument()
    expect(screen.queryByText(/epoch/i)).not.toBeInTheDocument()
  })

  it('refetches every 10 seconds', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    fetchMock.mockImplementation(() => Promise.resolve(json(response())))
    renderPage()
    await screen.findByText('sqi-a')
    expect(fetchMock).toHaveBeenCalledTimes(1)

    await act(async () => {
      await vi.advanceTimersByTimeAsync(9_000)
    })
    expect(fetchMock).toHaveBeenCalledTimes(1)

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_500)
    })
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })

  it('shows the error state when the request fails', async () => {
    fetchMock.mockResolvedValueOnce(
      json(
        {
          type: 'about:blank',
          title: 'Service Unavailable',
          status: 503,
          detail: 'this server is a standby',
        },
        503,
      ),
    )
    renderPage()

    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(/failed to load servers/i)
    expect(alert).toHaveTextContent('this server is a standby')
  })

  it('has a back link to the Admin hub', async () => {
    fetchMock.mockResolvedValueOnce(json(response()))
    renderPage()
    await screen.findByText('sqi-a')
    expect(screen.getByRole('link', { name: /admin/i }).getAttribute('href')).toBe('/admin')
  })
})
