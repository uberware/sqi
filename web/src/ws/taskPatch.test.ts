// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect } from 'vitest'
import { patchTaskFromEvent, isProgressOnly } from './taskPatch'
import type { Task } from '@/api/types'
import type { WsTaskEvent } from './events'

function task(overrides: Partial<Task> = {}): Task {
  return {
    id: 't1',
    job_id: 'j1',
    step_id: 's1',
    name: 'task.0',
    status: 'running',
    created_at: '2024-01-01T00:00:00Z',
    updated_at: '2024-01-01T00:00:01Z',
    ...overrides,
  }
}

function event(overrides: Partial<WsTaskEvent> = {}): WsTaskEvent {
  return {
    job_id: 'j1',
    task_id: 't1',
    status: 'running',
    updated_at: '2024-01-01T00:00:02Z',
    ...overrides,
  }
}

describe('patchTaskFromEvent', () => {
  it('sets progress and status_text when present on the event', () => {
    const out = patchTaskFromEvent(task(), event({ progress: 43, status_text: 'frame 3' }))
    expect(out.progress).toBe(43)
    expect(out.status_text).toBe('frame 3')
    expect(out.updated_at).toBe('2024-01-01T00:00:02Z')
  })

  it('keeps previous progress values when the event omits them', () => {
    const out = patchTaskFromEvent(
      task({ progress: 10, status_text: 'old', progress_at: '2024-01-01T00:00:00Z' }),
      event(),
    )
    expect(out.progress).toBe(10)
    expect(out.status_text).toBe('old')
    expect(out.progress_at).toBe('2024-01-01T00:00:00Z')
  })

  it.each(['pending', 'ready', 'assigned'] as const)(
    'clears progress fields on a %s event',
    (status) => {
      const out = patchTaskFromEvent(
        task({ progress: 10, status_text: 'old', progress_at: '2024-01-01T00:00:00Z' }),
        event({ status }),
      )
      expect(out.status).toBe(status)
      expect('progress' in out).toBe(false)
      expect('status_text' in out).toBe(false)
      expect('progress_at' in out).toBe(false)
    },
  )

  it('does not clear progress on a terminal event', () => {
    const out = patchTaskFromEvent(task({ progress: 90 }), event({ status: 'succeeded' }))
    expect(out.progress).toBe(90)
  })

  it('sets assigned_worker_id only when the event carries a worker', () => {
    expect(patchTaskFromEvent(task(), event({ worker_id: 'w1' })).assigned_worker_id).toBe('w1')
    expect(patchTaskFromEvent(task({ assigned_worker_id: 'w0' }), event()).assigned_worker_id).toBe(
      'w0',
    )
  })

  it('sets unschedulable_reason when present', () => {
    const out = patchTaskFromEvent(
      task(),
      event({ status: 'ready', unschedulable_reason: 'no gpu' }),
    )
    expect(out.unschedulable_reason).toBe('no gpu')
  })

  it('drops unschedulable_reason when absent from the event', () => {
    const out = patchTaskFromEvent(task({ unschedulable_reason: 'no gpu' }), event())
    expect('unschedulable_reason' in out).toBe(false)
  })

  it('does not mutate the previous task', () => {
    const prev = task({ progress: 5, unschedulable_reason: 'x' })
    patchTaskFromEvent(prev, event({ status: 'ready' }))
    expect(prev.progress).toBe(5)
    expect(prev.unschedulable_reason).toBe('x')
  })
})

describe('isProgressOnly', () => {
  it('is true when the status is unchanged', () => {
    expect(isProgressOnly(task({ status: 'running' }), event({ status: 'running' }))).toBe(true)
  })
  it('is false when the status changed', () => {
    expect(isProgressOnly(task({ status: 'running' }), event({ status: 'failed' }))).toBe(false)
  })
  it('is false when the previous task is unknown', () => {
    expect(isProgressOnly(undefined, event())).toBe(false)
  })
})
