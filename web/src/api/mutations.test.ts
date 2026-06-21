// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach } from 'vitest'
import { fetchRetryJob } from './mutations'
import { apiFetch } from './client'

vi.mock('./client', () => ({ apiFetch: vi.fn() }))

describe('fetchRetryJob', () => {
  beforeEach(() => vi.mocked(apiFetch).mockReset())

  it('POSTs to /jobs/{id}/retry and returns the summary', async () => {
    vi.mocked(apiFetch).mockResolvedValue({ job_id: 'j1', retried: 3 })
    const res = await fetchRetryJob('j1')
    expect(apiFetch).toHaveBeenCalledWith('/jobs/j1/retry', { method: 'POST' })
    expect(res).toEqual({ job_id: 'j1', retried: 3 })
  })

  it('URL-encodes the id', async () => {
    vi.mocked(apiFetch).mockResolvedValue({ job_id: 'a/b', retried: 0 })
    await fetchRetryJob('a/b')
    expect(apiFetch).toHaveBeenCalledWith('/jobs/a%2Fb/retry', { method: 'POST' })
  })
})
