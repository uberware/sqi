// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect } from 'vitest'
import type { JobStatus, TaskCounts } from '@/api/types'
import {
  canCancelJob,
  canPauseJob,
  canResumeJob,
  canRetryJob,
  retryableTaskCount,
} from './jobStatus'

const ALL: JobStatus[] = [
  'pending',
  'running',
  'paused',
  'blocked',
  'completed',
  'failed',
  'canceled',
]

function counts(overrides: Partial<TaskCounts> = {}): TaskCounts {
  return {
    total: 0,
    pending: 0,
    ready: 0,
    assigned: 0,
    running: 0,
    succeeded: 0,
    failed: 0,
    canceled: 0,
    ...overrides,
  }
}

const statusesWhere = (pred: (job: { status: JobStatus }) => boolean) =>
  ALL.filter((status) => pred({ status }))

describe('job action rules', () => {
  it('offers pause for pending and running jobs', () => {
    expect(statusesWhere(canPauseJob)).toEqual(['pending', 'running'])
  })

  it('offers resume for paused jobs', () => {
    expect(statusesWhere(canResumeJob)).toEqual(['paused'])
  })

  it('offers cancel for pending, running and paused jobs', () => {
    expect(statusesWhere(canCancelJob)).toEqual(['pending', 'running', 'paused'])
  })

  it('counts failed and canceled tasks as retryable', () => {
    expect(retryableTaskCount(counts({ failed: 2, canceled: 1, succeeded: 4 }))).toBe(3)
    expect(retryableTaskCount(undefined)).toBe(0)
  })

  it('offers retry whenever a job has a retryable task, whatever its status', () => {
    expect(canRetryJob({ status: 'running', task_counts: counts({ canceled: 1 }) })).toBe(true)
    expect(canRetryJob({ status: 'failed', task_counts: counts({ failed: 1 }) })).toBe(true)
    expect(canRetryJob({ status: 'completed', task_counts: counts({ succeeded: 3 }) })).toBe(false)
    expect(canRetryJob({ status: 'pending' })).toBe(false)
  })
})
