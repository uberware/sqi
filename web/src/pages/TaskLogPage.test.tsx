// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter, Routes, Route } from 'react-router'
import TaskLogPage from './TaskLogPage'
import * as queries from '@/api/queries'
import * as diagnostics from '@/api/diagnostics'

// Isolate the page from LogViewer's WS/query machinery.
vi.mock('@/components/LogViewer', () => ({
  default: ({ taskId, taskStatus }: { taskId: string; taskStatus: string }) => (
    <div data-testid="log-viewer">{`${taskId}:${taskStatus}`}</div>
  ),
}))

// The diagnostics fallback panel subscribes to WS and queries diagnostics; stub
// both so it renders deterministically from injected data.
vi.mock('@/ws/context', () => ({ useWebSocket: () => {} }))
vi.mock('@/api/diagnostics', async (importOriginal) => {
  const orig = await importOriginal<typeof import('@/api/diagnostics')>()
  return { ...orig, useDiagnosticsLogs: vi.fn() }
})

function mockJob(data: unknown) {
  vi.spyOn(queries, 'useGetJob').mockReturnValue({
    data,
    isLoading: false,
    isError: false,
  } as unknown as ReturnType<typeof queries.useGetJob>)
}

// The page fetches a task's logs to decide whether to show the diagnostics
// fallback. Default to an empty list unless a test overrides it.
function mockTaskLogs(items: unknown[] = []) {
  vi.spyOn(queries, 'useTaskLogs').mockReturnValue({
    data: { items },
    isLoading: false,
    isError: false,
  } as unknown as ReturnType<typeof queries.useTaskLogs>)
}

function renderAt(path = '/jobs/job1/tasks/task1/logs') {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="/jobs/:id/tasks/:taskId/logs" element={<TaskLogPage />} />
      </Routes>
    </MemoryRouter>,
  )
}

describe('TaskLogPage', () => {
  it('renders a pending viewer while the task is loading', () => {
    vi.spyOn(queries, 'useGetTask').mockReturnValue({
      data: undefined,
      isLoading: true,
      isError: false,
    } as unknown as ReturnType<typeof queries.useGetTask>)
    mockJob(undefined)
    mockTaskLogs()
    renderAt()
    expect(screen.getByTestId('log-viewer')).toHaveTextContent('task1:pending')
  })

  it('renders an error notice and a running viewer on error', () => {
    vi.spyOn(queries, 'useGetTask').mockReturnValue({
      data: undefined,
      isLoading: false,
      isError: true,
    } as unknown as ReturnType<typeof queries.useGetTask>)
    mockJob(undefined)
    mockTaskLogs()
    renderAt()
    expect(screen.getByRole('alert')).toBeInTheDocument()
    expect(screen.getByTestId('log-viewer')).toHaveTextContent('task1:running')
  })

  it('shows "Logs" in the header with the job name (not the task name) beneath it', () => {
    vi.spyOn(queries, 'useGetTask').mockReturnValue({
      data: { name: 'Render', status: 'succeeded' },
      isLoading: false,
      isError: false,
    } as unknown as ReturnType<typeof queries.useGetTask>)
    mockJob({ name: 'frame-render' })
    mockTaskLogs([{ id: 'c1' }])
    renderAt()

    // Header is just "Logs" (display font applies the invert-case styling),
    // and no longer embeds the name.
    const heading = screen.getByRole('heading')
    expect(heading).toHaveTextContent(/^lOGS$/)
    expect(heading.textContent ?? '').not.toContain('rENDER')

    // The subtitle beneath the header shows the short task id and status.
    expect(screen.getByText('Task task1 · succeeded')).toBeInTheDocument()
    // The job name is shown separately (far right, with the back link).
    expect(screen.getByText('frame-render')).toBeInTheDocument()
    expect(screen.getByTestId('log-viewer')).toHaveTextContent('task1:succeeded')
  })

  it('falls back to worker diagnostics when a failed task has no logs', async () => {
    vi.spyOn(queries, 'useGetTask').mockReturnValue({
      data: { name: 'Render', status: 'failed', assigned_worker_id: 'w1' },
      isLoading: false,
      isError: false,
    } as unknown as ReturnType<typeof queries.useGetTask>)
    mockJob({ name: 'frame-render' })
    mockTaskLogs([])
    vi.mocked(diagnostics.useDiagnosticsLogs).mockReturnValue({
      data: {
        records: [
          {
            ts: '2026-06-17T12:00:00Z',
            component: 'worker:w1',
            level: 'ERROR',
            msg: 'executable file not found',
            attrs: { task_id: 'task1' },
          },
        ],
      },
      isLoading: false,
      isError: false,
    } as unknown as ReturnType<typeof diagnostics.useDiagnosticsLogs>)

    renderAt()

    expect(await screen.findByText(/no task output/i)).toBeInTheDocument()
    expect(await screen.findByText('executable file not found')).toBeInTheDocument()
  })
})
