// SPDX-License-Identifier: AGPL-3.0-only

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor, act, fireEvent } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { ReactNode } from 'react'
import LogViewer from './LogViewer'
import { WebSocketProvider } from '@/ws/context'
import type { TaskLogsResponse } from '@/api/types'

// ── Mock fetchTaskLogs ────────────────────────────────────────────────────────

vi.mock('@/api/queries', async (importOriginal) => {
  const orig = await importOriginal<typeof import('@/api/queries')>()
  return { ...orig, fetchTaskLogs: vi.fn() }
})

const { fetchTaskLogs } = await import('@/api/queries')
const fetchTaskLogsMock = vi.mocked(fetchTaskLogs)

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

function wsInstance(): MockWebSocket {
  const ws = MockWebSocket.instances[0]
  if (!ws) throw new Error('No MockWebSocket instance')
  return ws
}

// ── Clipboard mock ────────────────────────────────────────────────────────────

const clipboardWriteText = vi.fn<(text: string) => Promise<void>>()

// ── Shared fixture data ───────────────────────────────────────────────────────

function makeLogsResponse(override?: Partial<TaskLogsResponse>): TaskLogsResponse {
  return {
    items: [
      {
        id: 'c1',
        task_id: 'task-1',
        attempt_id: 'att-1',
        seq_num: 1,
        nats_seq: 1,
        stream: 'stdout',
        data: 'Hello world\n',
        at: '2026-01-01T00:00:00Z',
        received_at: '2026-01-01T00:00:00Z',
      },
      {
        id: 'c2',
        task_id: 'task-1',
        attempt_id: 'att-1',
        seq_num: 2,
        nats_seq: 2,
        stream: 'stderr',
        data: 'Error: something went wrong\n',
        at: '2026-01-01T00:00:01Z',
        received_at: '2026-01-01T00:00:01Z',
      },
    ],
    after_nats_seq: 0,
    limit: 500,
    ...override,
  }
}

// ── Test wrapper ──────────────────────────────────────────────────────────────

function Wrapper({ children }: { children: ReactNode }) {
  return <WebSocketProvider url="ws://test">{children}</WebSocketProvider>
}

function renderViewer(
  taskId = 'task-1',
  taskStatus: 'running' | 'succeeded' | 'pending' = 'succeeded',
) {
  return render(<LogViewer taskId={taskId} taskStatus={taskStatus} />, { wrapper: Wrapper })
}

// ── Setup / teardown ──────────────────────────────────────────────────────────

beforeEach(() => {
  MockWebSocket.instances = []
  vi.stubGlobal('WebSocket', MockWebSocket)
  clipboardWriteText.mockResolvedValue(undefined)
  // jsdom does not expose navigator.clipboard by default — define it as a
  // configurable property so we can spy on writeText.
  Object.defineProperty(navigator, 'clipboard', {
    value: { writeText: clipboardWriteText },
    configurable: true,
    writable: true,
  })
  fetchTaskLogsMock.mockReset()
  // jsdom does not implement scrollIntoView — provide a no-op stub.
  Element.prototype.scrollIntoView = vi.fn()
})

afterEach(() => {
  vi.restoreAllMocks()
})

// ── Tests ─────────────────────────────────────────────────────────────────────

describe('LogViewer', () => {
  // ── Rendering ───────────────────────────────────────────────────────────────

  describe('rendering', () => {
    it('shows a loading state while the fetch is in flight', async () => {
      // Never resolves so we can observe the loading state.
      fetchTaskLogsMock.mockReturnValue(new Promise(() => {}))
      renderViewer()
      expect(screen.getByText('Loading logs…')).toBeInTheDocument()
    })

    it('renders each log chunk as visible lines', async () => {
      fetchTaskLogsMock.mockResolvedValue(makeLogsResponse())
      renderViewer()
      await waitFor(() => expect(screen.getByText('Hello world')).toBeInTheDocument())
      expect(screen.getByText('Error: something went wrong')).toBeInTheDocument()
    })

    it('renders line numbers in the gutter', async () => {
      fetchTaskLogsMock.mockResolvedValue(makeLogsResponse())
      renderViewer()
      await waitFor(() => expect(screen.getByText('1')).toBeInTheDocument())
      expect(screen.getByText('2')).toBeInTheDocument()
    })

    it('shows empty state when the response has no items', async () => {
      fetchTaskLogsMock.mockResolvedValue({ items: [], after_nats_seq: 0, limit: 500 })
      renderViewer()
      await waitFor(() => expect(screen.getByText('No log output yet.')).toBeInTheDocument())
    })

    it('shows an error message when the fetch rejects', async () => {
      fetchTaskLogsMock.mockRejectedValue(new Error('network error'))
      renderViewer()
      await waitFor(() => expect(screen.getByText('network error')).toBeInTheDocument())
    })

    it('shows the "Load more" button when the server returned a full page', async () => {
      // Return exactly INITIAL_LIMIT (500) items to trigger hasMore=true.
      const items = Array.from({ length: 500 }, (_, i) => ({
        id: `c${i}`,
        task_id: 'task-1',
        attempt_id: 'att-1',
        seq_num: i + 1,
        nats_seq: i + 1,
        stream: 'stdout' as const,
        data: `line ${i + 1}\n`,
        at: '2026-01-01T00:00:00Z',
        received_at: '2026-01-01T00:00:00Z',
      }))
      fetchTaskLogsMock.mockResolvedValue({ items, after_nats_seq: 0, limit: 500 })
      renderViewer()
      await waitFor(() => expect(screen.getByText('Load more')).toBeInTheDocument())
    })

    it('does not show the "Load more" button when fewer than INITIAL_LIMIT items were returned', async () => {
      fetchTaskLogsMock.mockResolvedValue(makeLogsResponse())
      renderViewer()
      await waitFor(() => expect(screen.queryByText('Load more')).not.toBeInTheDocument())
    })
  })

  // ── ANSI code handling ───────────────────────────────────────────────────────

  describe('ANSI escape code handling', () => {
    it('strips ANSI escape codes when copying to clipboard', async () => {
      fetchTaskLogsMock.mockResolvedValue({
        items: [
          {
            id: 'c1',
            task_id: 'task-1',
            attempt_id: 'att-1',
            seq_num: 1,
            nats_seq: 1,
            stream: 'stdout' as const,
            data: '\x1b[31mred text\x1b[0m',
            at: '2026-01-01T00:00:00Z',
            received_at: '2026-01-01T00:00:00Z',
          },
        ],
        after_nats_seq: 0,
        limit: 500,
      })
      renderViewer()

      // Wait for data to load so the Copy button becomes enabled.
      const copyBtn = await screen.findByRole('button', { name: /copy log text/i })
      await waitFor(() => expect(copyBtn).not.toBeDisabled())

      act(() => {
        fireEvent.click(copyBtn)
      })

      // Verify clipboard was written with plain text (no ANSI codes).
      await waitFor(() => {
        expect(clipboardWriteText).toHaveBeenCalledWith('red text')
      })
    })

    it('renders ANSI color codes as HTML spans in the log display', async () => {
      fetchTaskLogsMock.mockResolvedValue({
        items: [
          {
            id: 'c1',
            task_id: 'task-1',
            attempt_id: 'att-1',
            seq_num: 1,
            nats_seq: 1,
            stream: 'stdout' as const,
            data: '\x1b[32mgreen\x1b[0m',
            at: '2026-01-01T00:00:00Z',
            received_at: '2026-01-01T00:00:00Z',
          },
        ],
        after_nats_seq: 0,
        limit: 500,
      })
      const { container } = renderViewer()

      await waitFor(() => {
        const span = container.querySelector('span[style*="color"]')
        expect(span).not.toBeNull()
      })
    })
  })

  // ── stdout vs stderr distinction ─────────────────────────────────────────────

  describe('stdout vs stderr gutter color distinction', () => {
    it('applies streamStdout class to stdout log lines', async () => {
      fetchTaskLogsMock.mockResolvedValue(makeLogsResponse())
      const { container } = renderViewer()

      await waitFor(() => screen.getByText('Hello world'))

      // The first rendered line (stdout) should carry the streamStdout CSS module class.
      const stdoutLines = container.querySelectorAll('[class*="streamStdout"]')
      expect(stdoutLines.length).toBeGreaterThan(0)
    })

    it('applies streamStderr class to stderr log lines', async () => {
      fetchTaskLogsMock.mockResolvedValue(makeLogsResponse())
      const { container } = renderViewer()

      await waitFor(() => screen.getByText('Error: something went wrong'))

      const stderrLines = container.querySelectorAll('[class*="streamStderr"]')
      expect(stderrLines.length).toBeGreaterThan(0)
    })
  })

  // ── Auto-scroll behaviour ────────────────────────────────────────────────────

  describe('auto-scroll', () => {
    it('renders the "Pause scroll" button when auto-scroll is active', async () => {
      fetchTaskLogsMock.mockResolvedValue(makeLogsResponse())
      renderViewer()
      await waitFor(() => expect(screen.getByText('Hello world')).toBeInTheDocument())
      expect(screen.getByRole('button', { name: /pause scroll/i })).toBeInTheDocument()
    })

    it('toggles to "Resume scroll" when the pause button is clicked', async () => {
      const user = userEvent.setup()
      fetchTaskLogsMock.mockResolvedValue(makeLogsResponse())
      renderViewer()

      await waitFor(() => expect(screen.getByText('Hello world')).toBeInTheDocument())
      await user.click(screen.getByRole('button', { name: /pause scroll/i }))

      expect(screen.getByRole('button', { name: /resume scroll/i })).toBeInTheDocument()
    })

    it('shows the "Jump to bottom" bar when paused and a live WS chunk arrives', async () => {
      const user = userEvent.setup()
      fetchTaskLogsMock.mockResolvedValue(makeLogsResponse())
      renderViewer('task-1', 'running')

      await waitFor(() => expect(screen.getByText('Hello world')).toBeInTheDocument())

      // Pause auto-scroll.
      await user.click(screen.getByRole('button', { name: /pause scroll/i }))

      // Simulate a live WS push.
      act(() => {
        wsInstance().simulateOpen()
        wsInstance().simulateMessage({
          type: 'push',
          subject: 'tasks/task-1/logs',
          payload: {
            task_id: 'task-1',
            attempt_id: 'att-1',
            seq_num: 99,
            stream: 'stdout',
            data: 'live chunk\n',
            at: '2026-01-01T00:01:00Z',
          },
          seq: 1,
        })
      })

      await waitFor(() => expect(screen.getByText('Jump to bottom')).toBeInTheDocument())
    })

    it('hides the jump bar and resets unread count when auto-scroll is resumed', async () => {
      const user = userEvent.setup()
      fetchTaskLogsMock.mockResolvedValue(makeLogsResponse())
      renderViewer('task-1', 'running')

      await waitFor(() => expect(screen.getByText('Hello world')).toBeInTheDocument())

      // Pause and trigger a WS push.
      await user.click(screen.getByRole('button', { name: /pause scroll/i }))
      act(() => {
        wsInstance().simulateOpen()
        wsInstance().simulateMessage({
          type: 'push',
          subject: 'tasks/task-1/logs',
          payload: {
            task_id: 'task-1',
            attempt_id: 'att-1',
            seq_num: 99,
            stream: 'stdout',
            data: 'live chunk\n',
            at: '2026-01-01T00:01:00Z',
          },
          seq: 1,
        })
      })

      await waitFor(() => expect(screen.getByText('Jump to bottom')).toBeInTheDocument())

      // Resume scroll.
      await user.click(screen.getByRole('button', { name: /resume scroll/i }))

      await waitFor(() => expect(screen.queryByText('Jump to bottom')).not.toBeInTheDocument())
    })

    it('does not show live indicator for a terminal task', async () => {
      fetchTaskLogsMock.mockResolvedValue(makeLogsResponse())
      renderViewer('task-1', 'succeeded')
      await waitFor(() => screen.getByText('Hello world'))
      expect(screen.queryByText('Live')).not.toBeInTheDocument()
    })

    it('shows the live indicator when the task is running', async () => {
      fetchTaskLogsMock.mockResolvedValue(makeLogsResponse())
      renderViewer('task-1', 'running')
      await waitFor(() => screen.getByText('Hello world'))
      expect(screen.getByText('Live')).toBeInTheDocument()
    })
  })

  // ── Load more ─────────────────────────────────────────────────────────────────

  describe('load more', () => {
    it('appends the next page of chunks when "Load more" is clicked', async () => {
      const user = userEvent.setup()
      const firstPage = Array.from({ length: 500 }, (_, i) => ({
        id: `c${i}`,
        task_id: 'task-1',
        attempt_id: 'att-1',
        seq_num: i + 1,
        nats_seq: i + 1,
        stream: 'stdout' as const,
        data: `initial-${i + 1}\n`,
        at: '2026-01-01T00:00:00Z',
        received_at: '2026-01-01T00:00:00Z',
      }))
      const secondPage = [
        {
          id: 'extra1',
          task_id: 'task-1',
          attempt_id: 'att-1',
          seq_num: 501,
          nats_seq: 501,
          stream: 'stdout' as const,
          data: 'extra-501\n',
          at: '2026-01-01T00:01:00Z',
          received_at: '2026-01-01T00:01:00Z',
        },
      ]
      fetchTaskLogsMock
        .mockResolvedValueOnce({ items: firstPage, after_nats_seq: 0, limit: 500 })
        .mockResolvedValueOnce({ items: secondPage, after_nats_seq: 500, limit: 500 })

      renderViewer()
      const loadMoreBtn = await screen.findByRole('button', { name: /load more/i })
      await user.click(loadMoreBtn)

      await waitFor(() => expect(screen.getByText('extra-501')).toBeInTheDocument())
    })

    it('does not render duplicate lines when loadMore returns a chunk already delivered by WS', async () => {
      const user = userEvent.setup()
      const firstPage = Array.from({ length: 500 }, (_, i) => ({
        id: `c${i}`,
        task_id: 'task-1',
        attempt_id: 'att-1',
        seq_num: i + 1,
        nats_seq: i + 1,
        stream: 'stdout' as const,
        data: `line-${i + 1}\n`,
        at: '2026-01-01T00:00:00Z',
        received_at: '2026-01-01T00:00:00Z',
      }))
      // Second page includes nats_seq 501 which was already pushed via WS.
      const secondPage = [
        {
          id: 'rest-501',
          task_id: 'task-1',
          attempt_id: 'att-1',
          seq_num: 501,
          nats_seq: 501,
          stream: 'stdout' as const,
          data: 'dedup-me\n',
          at: '2026-01-01T00:01:00Z',
          received_at: '2026-01-01T00:01:00Z',
        },
      ]
      fetchTaskLogsMock
        .mockResolvedValueOnce({ items: firstPage, after_nats_seq: 0, limit: 500 })
        .mockResolvedValueOnce({ items: secondPage, after_nats_seq: 500, limit: 500 })

      renderViewer('task-1', 'running')
      await screen.findByRole('button', { name: /load more/i })

      // Simulate WS delivering seq 501 before the HTTP loadMore call.
      act(() => {
        wsInstance().simulateOpen()
        wsInstance().simulateMessage({
          type: 'push',
          subject: 'tasks/task-1/logs',
          payload: {
            task_id: 'task-1',
            attempt_id: 'att-1',
            seq_num: 501,
            stream: 'stdout',
            data: 'dedup-me\n',
            at: '2026-01-01T00:01:00Z',
          },
          seq: 1,
        })
      })

      await waitFor(() => expect(screen.getByText('dedup-me')).toBeInTheDocument())

      // Click Load more — the HTTP response also contains seq 501.
      await user.click(screen.getByRole('button', { name: /load more/i }))

      await waitFor(() => {
        // Should appear exactly once.
        expect(screen.getAllByText('dedup-me')).toHaveLength(1)
      })
    })
  })

  // ── Clipboard error handling ───────────────────────────────────────────────────

  describe('clipboard error handling', () => {
    it('does not show any error UI when clipboard.writeText rejects', async () => {
      clipboardWriteText.mockRejectedValue(new Error('clipboard denied'))
      fetchTaskLogsMock.mockResolvedValue(makeLogsResponse())
      const { container } = renderViewer()

      const copyBtn = await screen.findByRole('button', { name: /copy log text/i })
      await waitFor(() => expect(copyBtn).not.toBeDisabled())
      act(() => {
        fireEvent.click(copyBtn)
      })

      // Wait a tick to let any async rejection settle.
      await waitFor(() => expect(clipboardWriteText).toHaveBeenCalled())
      // No error alert should appear — failure is silently swallowed by design.
      expect(container.querySelector('[role="alert"]')).toBeNull()
    })
  })

  // ── WS dedup ──────────────────────────────────────────────────────────────────

  describe('WS dedup', () => {
    it('does not render a duplicate line when the same seq_num arrives twice', async () => {
      fetchTaskLogsMock.mockResolvedValue(makeLogsResponse())
      renderViewer('task-1', 'running')
      await waitFor(() => screen.getByText('Hello world'))

      const push = {
        type: 'push',
        subject: 'tasks/task-1/logs',
        payload: {
          task_id: 'task-1',
          attempt_id: 'att-1',
          seq_num: 99,
          stream: 'stdout',
          data: 'duplicate-line\n',
          at: '2026-01-01T00:05:00Z',
        },
        seq: 1,
      }

      act(() => {
        wsInstance().simulateOpen()
        wsInstance().simulateMessage(push)
        wsInstance().simulateMessage({ ...push, seq: 2 }) // same payload, second delivery
      })

      await waitFor(() => expect(screen.getAllByText('duplicate-line')).toHaveLength(1))
    })
  })

  // ── Multiline chunks ──────────────────────────────────────────────────────────

  describe('multiline chunks', () => {
    it('splits a single chunk with multiple newlines into multiple numbered lines', async () => {
      fetchTaskLogsMock.mockResolvedValue({
        items: [
          {
            id: 'c1',
            task_id: 'task-1',
            attempt_id: 'att-1',
            seq_num: 1,
            nats_seq: 1,
            stream: 'stdout' as const,
            data: 'line A\nline B\nline C\n',
            at: '2026-01-01T00:00:00Z',
            received_at: '2026-01-01T00:00:00Z',
          },
        ],
        after_nats_seq: 0,
        limit: 500,
      })
      renderViewer()

      await waitFor(() => {
        expect(screen.getByText('line A')).toBeInTheDocument()
        expect(screen.getByText('line B')).toBeInTheDocument()
        expect(screen.getByText('line C')).toBeInTheDocument()
      })
      // Line numbers should be 1, 2, 3
      expect(screen.getByText('3')).toBeInTheDocument()
    })
  })
})
