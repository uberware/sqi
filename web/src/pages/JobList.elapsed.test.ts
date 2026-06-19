// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect } from 'vitest'
import { elapsedLabel } from './JobList'
import type { Job } from '@/api/types'

function job(partial: Partial<Job>): Job {
  return { started_at: undefined, completed_at: undefined, ...partial } as Job
}

describe('elapsedLabel', () => {
  const now = Date.parse('2026-06-19T00:01:30.000Z') // 90s after the start below

  it('returns — when the job has not started', () => {
    expect(elapsedLabel(job({}), now)).toBe('—')
  })

  it('measures a running job from started_at to now', () => {
    const started_at = '2026-06-19T00:00:00.000Z'
    expect(elapsedLabel(job({ started_at }), now)).toBe('1m 30s')
  })

  it('measures a completed job from started_at to completed_at, ignoring now', () => {
    const started_at = '2026-06-19T00:00:00.000Z'
    const completed_at = '2026-06-19T00:00:45.000Z'
    expect(elapsedLabel(job({ started_at, completed_at }), now)).toBe('45s')
  })

  it('formats multi-hour durations', () => {
    const started_at = '2026-06-19T00:00:00.000Z'
    const later = Date.parse('2026-06-19T02:05:00.000Z')
    expect(elapsedLabel(job({ started_at }), later)).toBe('2h 5m')
  })
})
