// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach } from 'vitest'
import { useState } from 'react'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import DependsOnField from './DependsOnField'
import type { Job } from '@/api/types'

const h = vi.hoisted(() => ({ jobs: [] as Job[] }))

const useListJobsMock = vi.fn()

vi.mock('@/api/queries', async (orig) => ({
  ...(await orig<typeof import('@/api/queries')>()),
  useListJobs: (...args: unknown[]) => {
    useListJobsMock(...args)
    return {
      data: { items: h.jobs, total: h.jobs.length, limit: 200, offset: 0 },
      isLoading: false,
      error: null,
    }
  },
}))

function makeJob(overrides: Partial<Job> = {}): Job {
  return {
    id: 'job-a',
    farm_id: 'farm-1',
    queue_id: 'queue-1',
    name: 'Upstream A',
    owner: 'user',
    submitter: 'user',
    priority: 50,
    status: 'running',
    template_format: 'yaml',
    failed_attempts: 0,
    created_at: '2024-01-01T00:00:00Z',
    updated_at: '2024-01-01T00:00:00Z',
    ...overrides,
  }
}

describe('DependsOnField', () => {
  beforeEach(() => {
    useListJobsMock.mockClear()
  })

  it('does not enable the candidate query until a farm is selected', () => {
    h.jobs = []
    render(<DependsOnField farmId={undefined} value={[]} onChange={vi.fn()} />)

    expect(useListJobsMock).toHaveBeenCalledWith(expect.objectContaining({ limit: 200 }), {
      enabled: false,
    })
  })

  it('enables the candidate query once a farm is selected', () => {
    h.jobs = []
    render(<DependsOnField farmId="farm-1" value={[]} onChange={vi.fn()} />)

    expect(useListJobsMock).toHaveBeenCalledWith(
      expect.objectContaining({ farm_id: 'farm-1', limit: 200 }),
      { enabled: true },
    )
  })

  it('lists non-terminal jobs as candidates and excludes terminal ones', () => {
    h.jobs = [
      makeJob({ id: 'job-a', name: 'Upstream A', status: 'running' }),
      makeJob({ id: 'job-b', name: 'Upstream B', status: 'completed' }),
    ]
    render(<DependsOnField farmId="farm-1" value={[]} onChange={vi.fn()} />)

    expect(screen.getByRole('option', { name: /Upstream A/ })).toBeInTheDocument()
    expect(screen.queryByRole('option', { name: /Upstream B/ })).not.toBeInTheDocument()
  })

  it('calls onChange with the selected job IDs', async () => {
    h.jobs = [
      makeJob({ id: 'job-a', name: 'Upstream A' }),
      makeJob({ id: 'job-b', name: 'Upstream B' }),
    ]
    const onChange = vi.fn()
    // A controlled multi-select must reflect each change back into `value`
    // for the next selection to accumulate — mirror that with a tiny stateful
    // wrapper, same as a real host form would (Submit.tsx / ProductSubmit.tsx).
    function Wrapper() {
      const [value, setValue] = useState<string[]>([])
      return (
        <DependsOnField
          farmId="farm-1"
          value={value}
          onChange={(ids) => {
            setValue(ids)
            onChange(ids)
          }}
        />
      )
    }
    const user = userEvent.setup()
    render(<Wrapper />)

    await user.selectOptions(screen.getByLabelText(/depends on jobs/i), ['job-a', 'job-b'])
    expect(onChange).toHaveBeenLastCalledWith(['job-a', 'job-b'])
  })

  it('is disabled when there is no farm selected', () => {
    h.jobs = []
    render(<DependsOnField farmId={undefined} value={[]} onChange={vi.fn()} />)
    expect(screen.getByLabelText(/depends on jobs/i)).toBeDisabled()
  })

  it('is disabled and shows a hint when a farm has no eligible candidates', () => {
    h.jobs = []
    render(<DependsOnField farmId="farm-1" value={[]} onChange={vi.fn()} />)
    expect(screen.getByLabelText(/depends on jobs/i)).toBeDisabled()
    expect(screen.getByText(/no eligible upstream jobs/i)).toBeInTheDocument()
  })

  it('clears the selection when the farm changes', () => {
    h.jobs = [makeJob({ id: 'job-a', name: 'Upstream A' })]
    const onChange = vi.fn()
    const { rerender } = render(
      <DependsOnField farmId="farm-1" value={['job-a']} onChange={onChange} />,
    )
    expect(onChange).not.toHaveBeenCalled()

    rerender(<DependsOnField farmId="farm-2" value={['job-a']} onChange={onChange} />)
    expect(onChange).toHaveBeenCalledWith([])
  })

  it('does not clear the selection on re-render when the farm is unchanged', () => {
    h.jobs = [makeJob({ id: 'job-a', name: 'Upstream A' })]
    const onChange = vi.fn()
    const { rerender } = render(
      <DependsOnField farmId="farm-1" value={['job-a']} onChange={onChange} />,
    )
    rerender(<DependsOnField farmId="farm-1" value={['job-a']} onChange={onChange} />)
    expect(onChange).not.toHaveBeenCalled()
  })
})
