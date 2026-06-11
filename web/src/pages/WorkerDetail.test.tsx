// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, waitFor, act } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { useState } from 'react'
import type { ReactNode } from 'react'
import WorkerDetail from './WorkerDetail'
import { WebSocketProvider } from '@/ws/context'
import type { WorkerDetail as WorkerDetailType } from '@/api/types'

// ── Mock WebSocket ────────────────────────────────────────────────────────────

class MockWebSocket {
  static instances: MockWebSocket[] = []
  static readonly CONNECTING = 0
  static readonly OPEN = 1
  static readonly CLOSING = 2
  static readonly CLOSED = 3

  readyState = MockWebSocket.CONNECTING
  url: string
  sentMessages: string[] = []

  onopen: ((event: Event) => void) | null = null
  onclose: ((event: CloseEvent) => void) | null = null
  onerror: ((event: Event) => void) | null = null
  onmessage: ((event: MessageEvent) => void) | null = null

  constructor(url: string) {
    this.url = url
    MockWebSocket.instances.push(this)
  }

  send(data: string): void {
    this.sentMessages.push(data)
  }

  close(): void {
    this.readyState = MockWebSocket.CLOSED
  }

  simulateOpen(): void {
    this.readyState = MockWebSocket.OPEN
    this.onopen?.(new Event('open'))
  }

  simulateMessage(data: unknown): void {
    const raw = typeof data === 'string' ? data : JSON.stringify(data)
    this.onmessage?.(new MessageEvent('message', { data: raw }))
  }
}

function wsInstance(i: number): MockWebSocket {
  const ws = MockWebSocket.instances[i]
  if (!ws) throw new Error(`No MockWebSocket at index ${i}`)
  return ws
}

// ── Mocks ─────────────────────────────────────────────────────────────────────

const fetchMock = vi.fn<typeof fetch>()

beforeEach(() => {
  MockWebSocket.instances = []
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
  vi.stubGlobal('WebSocket', MockWebSocket)
  vi.useFakeTimers({ shouldAdvanceTime: true })
})

afterEach(() => {
  vi.restoreAllMocks()
  vi.useRealTimers()
})

// ── Fixtures ──────────────────────────────────────────────────────────────────

const BASE_REGISTERED_AT = '2024-01-01T08:00:00Z'

function makeWorker(overrides: Partial<WorkerDetailType> = {}): WorkerDetailType {
  return {
    id: 'worker-aabbccdd',
    farm_id: 'farm-001',
    hostname: 'render-node-01',
    status: 'online',
    gpu: {},
    registered_at: BASE_REGISTERED_AT,
    updated_at: '2024-01-01T10:00:00Z',
    ...overrides,
  }
}

function okJson(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

// ── Test wrapper ──────────────────────────────────────────────────────────────

function makeClient() {
  return new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
}

function Wrapper({
  children,
  workerId = 'worker-aabbccdd',
}: {
  children: ReactNode
  workerId?: string
}) {
  const [client] = useState(makeClient)
  return (
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[`/workers/${workerId}`]}>
        <WebSocketProvider url="ws://test">
          <Routes>
            <Route path="/workers/:id" element={children} />
          </Routes>
        </WebSocketProvider>
      </MemoryRouter>
    </QueryClientProvider>
  )
}

// ── Tests ─────────────────────────────────────────────────────────────────────

describe('WorkerDetail', () => {
  // ── Task 74: Metadata header ─────────────────────────────────────────────────

  describe('metadata header (task 74)', () => {
    it('renders the worker hostname as the page title', async () => {
      fetchMock.mockResolvedValue(okJson(makeWorker({ hostname: 'gpu-node-07' })))

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('gpu-node-07'))
      expect(screen.getByText('gpu-node-07')).toBeInTheDocument()
    })

    it('renders the worker ID in the metadata card', async () => {
      fetchMock.mockResolvedValue(
        okJson(makeWorker({ id: 'abc12345-1111-2222-3333-444444444444' })),
      )

      render(<WorkerDetail />, {
        wrapper: ({ children }) => (
          <Wrapper workerId="abc12345-1111-2222-3333-444444444444">{children}</Wrapper>
        ),
      })

      await waitFor(() => screen.getByText('abc12345-1111-2222-3333-444444444444'))
      expect(screen.getByText('abc12345-1111-2222-3333-444444444444')).toBeInTheDocument()
    })

    it('renders the compute_location as the page subtitle when present', async () => {
      fetchMock.mockResolvedValue(okJson(makeWorker({ compute_location: 'us-east-1' })))

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('us-east-1'))
      expect(screen.getByText('us-east-1')).toBeInTheDocument()
    })

    it('renders the status badge', async () => {
      fetchMock.mockResolvedValue(okJson(makeWorker({ status: 'online' })))

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getAllByLabelText('Status: Online'))
      expect(screen.getAllByLabelText('Status: Online').length).toBeGreaterThan(0)
    })

    it('renders the registered_at timestamp label', async () => {
      fetchMock.mockResolvedValue(okJson(makeWorker()))

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('Registered'))
      expect(screen.getByText('Registered')).toBeInTheDocument()
    })

    it('renders the last_heartbeat_at timestamp label', async () => {
      fetchMock.mockResolvedValue(okJson(makeWorker({ last_heartbeat_at: '2024-01-01T10:30:00Z' })))

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('Last Heartbeat'))
      expect(screen.getByText('Last Heartbeat')).toBeInTheDocument()
    })

    it('renders the Uptime label', async () => {
      fetchMock.mockResolvedValue(okJson(makeWorker()))

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('Uptime'))
      expect(screen.getByText('Uptime')).toBeInTheDocument()
    })

    it('renders the ip_address when present', async () => {
      fetchMock.mockResolvedValue(okJson(makeWorker({ ip_address: '10.0.1.42' })))

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('10.0.1.42'))
      expect(screen.getByText('10.0.1.42')).toBeInTheDocument()
    })

    it('shows a loading state while data is fetching', () => {
      fetchMock.mockReturnValue(new Promise(() => undefined))

      render(<WorkerDetail />, { wrapper: Wrapper })

      expect(screen.getAllByText(/Loading/i).length).toBeGreaterThan(0)
    })

    it('shows an error banner when the fetch fails', async () => {
      fetchMock.mockRejectedValue(new Error('Network error'))

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByRole('alert'))
      expect(screen.getByRole('alert')).toBeInTheDocument()
    })
  })

  // ── Task 75: Capabilities ────────────────────────────────────────────────────

  describe('capabilities section (task 75)', () => {
    it('renders OS in the system capabilities section', async () => {
      fetchMock.mockResolvedValue(okJson(makeWorker({ os: 'Linux', os_version: '5.15' })))

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('OS'))
      expect(screen.getByText('Linux 5.15')).toBeInTheDocument()
    })

    it('renders CPU count in the system section', async () => {
      fetchMock.mockResolvedValue(okJson(makeWorker({ cpu_count: 32 })))

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('CPU Cores'))
      expect(screen.getByText('32')).toBeInTheDocument()
    })

    it('renders RAM in GB when >= 1024 MB', async () => {
      fetchMock.mockResolvedValue(okJson(makeWorker({ ram_mb: 65536 })))

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('64 GB'))
      expect(screen.getByText('64 GB')).toBeInTheDocument()
    })

    it('renders RAM in MB when < 1024 MB', async () => {
      fetchMock.mockResolvedValue(okJson(makeWorker({ ram_mb: 512 })))

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('512 MB'))
      expect(screen.getByText('512 MB')).toBeInTheDocument()
    })

    it('renders GPU model in the system section', async () => {
      fetchMock.mockResolvedValue(
        okJson(makeWorker({ gpu: { model: 'RTX 4090', vram_mb: 24576 } })),
      )

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('RTX 4090'))
      expect(screen.getByText('RTX 4090')).toBeInTheDocument()
    })

    it('renders GPU VRAM in GB', async () => {
      fetchMock.mockResolvedValue(okJson(makeWorker({ gpu: { model: 'A6000', vram_mb: 49152 } })))

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('48 GB'))
      expect(screen.getByText('48 GB')).toBeInTheDocument()
    })

    it('renders custom tags in a separate Tags section', async () => {
      fetchMock.mockResolvedValue(
        okJson(makeWorker({ tags: { maya: '2024', nuke: '15', project: 'vfx' } })),
      )

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('Tags'))
      expect(screen.getByText('maya')).toBeInTheDocument()
      expect(screen.getByText('2024')).toBeInTheDocument()
      expect(screen.getByText('nuke')).toBeInTheDocument()
    })

    it('renders the System section when only GPU VRAM is reported', async () => {
      fetchMock.mockResolvedValue(okJson(makeWorker({ gpu: { vram_mb: 8192 } })))

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('GPU VRAM'))
      expect(screen.getByText('8 GB')).toBeInTheDocument()
    })

    it('shows "No capabilities reported" when worker has no system or tag info', async () => {
      fetchMock.mockResolvedValue(okJson(makeWorker({ gpu: {} })))

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('No capabilities reported.'))
      expect(screen.getByText('No capabilities reported.')).toBeInTheDocument()
    })

    it('does not render Tags section when worker.tags is absent', async () => {
      fetchMock.mockResolvedValue(okJson(makeWorker({ os: 'Linux', gpu: {} })))

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('OS'))
      expect(screen.queryByText('Tags')).not.toBeInTheDocument()
    })
  })

  // ── Task 76: Assigned tasks ──────────────────────────────────────────────────

  describe('active tasks section (task 76)', () => {
    it('shows "No active tasks" when current_task is absent', async () => {
      fetchMock.mockResolvedValue(okJson(makeWorker()))

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('No active tasks.'))
      expect(screen.getByText('No active tasks.')).toBeInTheDocument()
    })

    it('renders the current task card when current_task is present', async () => {
      const worker = makeWorker({
        current_task: {
          id: 'task-11111111',
          job_id: 'job-99999999',
          name: 'render-frame-001',
          status: 'running',
          assigned_at: new Date(Date.now() - 90_000).toISOString(),
        },
      })
      fetchMock.mockResolvedValue(okJson(worker))

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('render-frame-001'))
      expect(screen.getByText('render-frame-001')).toBeInTheDocument()
    })

    it('renders a link to the job detail page for the current task', async () => {
      const worker = makeWorker({
        current_task: {
          id: 'task-abc',
          job_id: 'job-xyz12345',
          name: 'some-task',
          status: 'running',
        },
      })
      fetchMock.mockResolvedValue(okJson(worker))

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByRole('link', { name: /job-xyz1/ }))
      const link = screen.getByRole('link', { name: /job-xyz1/ })
      expect(link).toHaveAttribute('href', '/jobs/job-xyz12345')
    })

    it('renders a status badge for the current task', async () => {
      const worker = makeWorker({
        current_task: {
          id: 'task-aaa',
          job_id: 'job-bbb',
          name: 'active-task',
          status: 'running',
        },
      })
      fetchMock.mockResolvedValue(okJson(worker))

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('active-task'))
      expect(screen.getAllByLabelText('Status: Running').length).toBeGreaterThan(0)
    })

    it('renders the Start Time label for the current task', async () => {
      const worker = makeWorker({
        current_task: {
          id: 'task-t1',
          job_id: 'job-j1',
          name: 'task-name',
          status: 'assigned',
          assigned_at: '2024-01-01T09:00:00Z',
        },
      })
      fetchMock.mockResolvedValue(okJson(worker))

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('Start Time'))
      expect(screen.getByText('Start Time')).toBeInTheDocument()
    })

    it('renders the Elapsed label for the current task', async () => {
      const worker = makeWorker({
        current_task: {
          id: 'task-t2',
          job_id: 'job-j2',
          name: 'another-task',
          status: 'running',
          assigned_at: new Date(Date.now() - 5000).toISOString(),
        },
      })
      fetchMock.mockResolvedValue(okJson(worker))

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('Elapsed'))
      expect(screen.getByText('Elapsed')).toBeInTheDocument()
    })
  })

  // ── Task 77: Enable / Disable toggle ────────────────────────────────────────

  describe('enable/disable toggle (task 77)', () => {
    it('shows Disable button for online worker', async () => {
      fetchMock.mockResolvedValue(okJson(makeWorker({ status: 'online', hostname: 'online-node' })))

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByLabelText('Disable worker online-node'))
      expect(screen.getByLabelText('Disable worker online-node')).toBeInTheDocument()
    })

    it('shows no action button for offline worker', async () => {
      fetchMock.mockResolvedValue(
        okJson(makeWorker({ status: 'offline', hostname: 'offline-node' })),
      )

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getAllByLabelText('Status: Offline'))
      expect(screen.queryByLabelText('Disable worker offline-node')).not.toBeInTheDocument()
      expect(screen.queryByLabelText('Enable worker offline-node')).not.toBeInTheDocument()
    })

    it('shows Enable button for disabled worker', async () => {
      fetchMock.mockResolvedValue(
        okJson(makeWorker({ status: 'disabled', hostname: 'disabled-node' })),
      )

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByLabelText('Enable worker disabled-node'))
      expect(screen.getByLabelText('Enable worker disabled-node')).toBeInTheDocument()
    })

    it('Disable button calls the disable mutation', async () => {
      fetchMock.mockResolvedValue(
        okJson(makeWorker({ id: 'w-111', status: 'online', hostname: 'to-disable' })),
      )

      render(<WorkerDetail />, {
        wrapper: ({ children }) => <Wrapper workerId="w-111">{children}</Wrapper>,
      })

      await waitFor(() => screen.getByLabelText('Disable worker to-disable'))

      fetchMock.mockResolvedValueOnce(okJson({ id: 'w-111', status: 'disabled' }))
      fetchMock.mockResolvedValue(okJson(makeWorker({ id: 'w-111', status: 'disabled' })))

      fireEvent.click(screen.getByLabelText('Disable worker to-disable'))

      await waitFor(() => {
        const calls = fetchMock.mock.calls.flat() as string[]
        expect(
          calls.some((u) => typeof u === 'string' && u.includes('/workers/w-111/disable')),
        ).toBe(true)
      })
    })

    it('Enable button calls the enable mutation', async () => {
      fetchMock.mockResolvedValue(
        okJson(makeWorker({ id: 'w-222', status: 'disabled', hostname: 'to-enable' })),
      )

      render(<WorkerDetail />, {
        wrapper: ({ children }) => <Wrapper workerId="w-222">{children}</Wrapper>,
      })

      await waitFor(() => screen.getByLabelText('Enable worker to-enable'))

      fetchMock.mockResolvedValueOnce(okJson({ id: 'w-222', status: 'online' }))
      fetchMock.mockResolvedValue(okJson(makeWorker({ id: 'w-222', status: 'online' })))

      fireEvent.click(screen.getByLabelText('Enable worker to-enable'))

      await waitFor(() => {
        const calls = fetchMock.mock.calls.flat() as string[]
        expect(
          calls.some((u) => typeof u === 'string' && u.includes('/workers/w-222/enable')),
        ).toBe(true)
      })
    })

    it('Refresh button triggers a re-fetch', async () => {
      fetchMock.mockResolvedValue(okJson(makeWorker()))

      render(<WorkerDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByRole('button', { name: 'Refresh worker' }))
      const callsBefore = fetchMock.mock.calls.length

      fireEvent.click(screen.getByRole('button', { name: 'Refresh worker' }))

      await waitFor(() => {
        expect(fetchMock.mock.calls.length).toBeGreaterThan(callsBefore)
      })
    })
  })

  // ── WebSocket in-place updates ────────────────────────────────────────────────

  describe('websocket status updates', () => {
    it('updates the status badge when a WsWorkerEvent arrives for this worker', async () => {
      fetchMock.mockResolvedValue(okJson(makeWorker({ id: 'worker-aabbccdd', status: 'online' })))

      render(<WorkerDetail />, { wrapper: Wrapper })
      await waitFor(() => screen.getAllByLabelText('Status: Online'))

      act(() => {
        wsInstance(0).simulateOpen()
        wsInstance(0).simulateMessage({
          type: 'push',
          subject: 'workers',
          payload: {
            worker_id: 'worker-aabbccdd',
            status: 'disabled',
          },
          seq: 1,
        })
      })

      await waitFor(() =>
        expect(screen.getAllByLabelText('Status: Disabled').length).toBeGreaterThan(0),
      )
    })

    it('ignores WsWorkerEvents for other workers', async () => {
      fetchMock.mockResolvedValue(okJson(makeWorker({ id: 'worker-aabbccdd', status: 'online' })))

      render(<WorkerDetail />, { wrapper: Wrapper })
      await waitFor(() => screen.getAllByLabelText('Status: Online'))

      act(() => {
        wsInstance(0).simulateOpen()
        wsInstance(0).simulateMessage({
          type: 'push',
          subject: 'workers',
          payload: {
            worker_id: 'some-other-worker',
            status: 'disabled',
          },
          seq: 1,
        })
      })

      // Our worker's badge should remain Online
      expect(screen.getAllByLabelText('Status: Online').length).toBeGreaterThan(0)
      expect(screen.queryByLabelText('Status: Disabled')).not.toBeInTheDocument()
    })
  })
})
