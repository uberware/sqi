// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter, Routes, Route } from 'react-router-dom'
import TaskLogPage from './TaskLogPage'
import * as queries from '@/api/queries'

// Isolate the page from LogViewer's WS/query machinery.
vi.mock('@/components/LogViewer', () => ({
  default: ({ taskId, taskStatus }: { taskId: string; taskStatus: string }) => (
    <div data-testid="log-viewer">{`${taskId}:${taskStatus}`}</div>
  ),
}))

function mockJob(data: unknown) {
  vi.spyOn(queries, 'useGetJob').mockReturnValue({
    data,
    isLoading: false,
    isError: false,
  } as unknown as ReturnType<typeof queries.useGetJob>)
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
})
