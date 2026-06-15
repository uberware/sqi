// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import {
  queryKeys,
  fetchListJobs,
  fetchGetJob,
  fetchListTasks,
  fetchGetTask,
  fetchListWorkers,
  fetchGetWorker,
  fetchListFarms,
  fetchGetFarm,
  fetchListQueues,
  fetchListUsagePools,
  fetchGetUsagePool,
} from './queries'

// ── Helpers ───────────────────────────────────────────────────────────────────

const fetchMock = vi.fn<typeof fetch>()

beforeEach(() => {
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
})

afterEach(() => {
  vi.restoreAllMocks()
})

function makeOkResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

function listResponse<T>(items: T[] = []) {
  return makeOkResponse({ items, total: items.length, limit: 50, offset: 0 })
}

function calledUrl(): string {
  return (fetchMock.mock.calls[0] as [string, ...unknown[]])[0]
}

// ── queryKeys ─────────────────────────────────────────────────────────────────

describe('queryKeys', () => {
  it('jobs.all equals ["jobs"]', () => {
    expect(queryKeys.jobs.all).toEqual(['jobs'])
  })

  it('jobs.list includes params in key', () => {
    const params = { status: 'running', owner: 'alice' }
    expect(queryKeys.jobs.list(params)).toEqual(['jobs', 'list', params])
  })

  it('jobs.detail includes id in key', () => {
    expect(queryKeys.jobs.detail('job-1')).toEqual(['jobs', 'detail', 'job-1'])
  })

  it('tasks.all equals ["tasks"]', () => {
    expect(queryKeys.tasks.all).toEqual(['tasks'])
  })

  it('tasks.list includes jobId and optional params', () => {
    expect(queryKeys.tasks.list('job-1')).toEqual(['tasks', 'list', 'job-1', undefined])
    const p = { status: 'failed' }
    expect(queryKeys.tasks.list('job-1', p)).toEqual(['tasks', 'list', 'job-1', p])
  })

  it('tasks.detail includes id in key', () => {
    expect(queryKeys.tasks.detail('task-1')).toEqual(['tasks', 'detail', 'task-1'])
  })

  it('workers.all equals ["workers"]', () => {
    expect(queryKeys.workers.all).toEqual(['workers'])
  })

  it('workers.list includes optional params', () => {
    expect(queryKeys.workers.list()).toEqual(['workers', 'list', undefined])
    const p = { status: 'online' }
    expect(queryKeys.workers.list(p)).toEqual(['workers', 'list', p])
  })

  it('workers.detail includes id in key', () => {
    expect(queryKeys.workers.detail('w-1')).toEqual(['workers', 'detail', 'w-1'])
  })

  it('farms.all equals ["farms"]', () => {
    expect(queryKeys.farms.all).toEqual(['farms'])
  })

  it('farms.detail includes id in key', () => {
    expect(queryKeys.farms.detail('farm-1')).toEqual(['farms', 'detail', 'farm-1'])
  })

  it('queues.all equals ["queues"]', () => {
    expect(queryKeys.queues.all).toEqual(['queues'])
  })

  it('queues.list includes farmId', () => {
    expect(queryKeys.queues.list('farm-1')).toEqual(['queues', 'list', 'farm-1'])
  })
})

// ── fetchListJobs ─────────────────────────────────────────────────────────────

describe('fetchListJobs', () => {
  it('calls /api/v1/jobs with no query string when params are empty', async () => {
    fetchMock.mockResolvedValueOnce(listResponse([]))
    await fetchListJobs({})
    expect(calledUrl()).toBe('/api/v1/jobs')
  })

  it('includes defined scalar params in the query string', async () => {
    fetchMock.mockResolvedValueOnce(listResponse([]))
    await fetchListJobs({ status: 'running', owner: 'alice', limit: 10, offset: 20 })
    const url = calledUrl()
    expect(url).toContain('status=running')
    expect(url).toContain('owner=alice')
    expect(url).toContain('limit=10')
    expect(url).toContain('offset=20')
  })

  it('omits undefined params from the query string', async () => {
    fetchMock.mockResolvedValueOnce(listResponse([]))
    await fetchListJobs({ status: 'running' })
    const url = calledUrl()
    expect(url).not.toContain('owner=')
    expect(url).not.toContain('limit=')
    expect(url).not.toContain('offset=')
  })

  it('includes sort_by and sort_dir when set', async () => {
    fetchMock.mockResolvedValueOnce(listResponse([]))
    await fetchListJobs({ sort_by: 'priority', sort_dir: 'desc' })
    const url = calledUrl()
    expect(url).toContain('sort_by=priority')
    expect(url).toContain('sort_dir=desc')
  })
})

// ── fetchGetJob ───────────────────────────────────────────────────────────────

describe('fetchGetJob', () => {
  it('calls /api/v1/jobs/:id', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'job-1' }))
    await fetchGetJob('job-1')
    expect(calledUrl()).toBe('/api/v1/jobs/job-1')
  })

  it('URL-encodes the job id', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'a/b' }))
    await fetchGetJob('a/b')
    expect(calledUrl()).toBe('/api/v1/jobs/a%2Fb')
  })
})

// ── fetchListTasks ────────────────────────────────────────────────────────────

describe('fetchListTasks', () => {
  it('calls /api/v1/jobs/:jobId/tasks without params', async () => {
    fetchMock.mockResolvedValueOnce(listResponse([]))
    await fetchListTasks('job-1')
    expect(calledUrl()).toBe('/api/v1/jobs/job-1/tasks')
  })

  it('includes params when provided', async () => {
    fetchMock.mockResolvedValueOnce(listResponse([]))
    await fetchListTasks('job-1', {
      status: 'running',
      limit: 5,
      sort_by: 'status',
      sort_dir: 'asc',
    })
    const url = calledUrl()
    expect(url).toContain('status=running')
    expect(url).toContain('limit=5')
    expect(url).toContain('sort_by=status')
    expect(url).toContain('sort_dir=asc')
  })
})

// ── fetchGetTask ──────────────────────────────────────────────────────────────

describe('fetchGetTask', () => {
  it('calls /api/v1/tasks/:id', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'task-1' }))
    await fetchGetTask('task-1')
    expect(calledUrl()).toBe('/api/v1/tasks/task-1')
  })
})

// ── fetchListWorkers ──────────────────────────────────────────────────────────

describe('fetchListWorkers', () => {
  it('calls /api/v1/workers without params', async () => {
    fetchMock.mockResolvedValueOnce(listResponse([]))
    await fetchListWorkers()
    expect(calledUrl()).toBe('/api/v1/workers')
  })

  it('includes filter params when provided', async () => {
    fetchMock.mockResolvedValueOnce(listResponse([]))
    await fetchListWorkers({
      status: 'online',
      farm_id: 'farm-1',
      queue_id: 'q-1',
      compute_location: 'dc1',
    })
    const url = calledUrl()
    expect(url).toContain('status=online')
    expect(url).toContain('farm_id=farm-1')
    expect(url).toContain('queue_id=q-1')
    expect(url).toContain('compute_location=dc1')
  })

  it('includes sort params when provided', async () => {
    fetchMock.mockResolvedValueOnce(listResponse([]))
    await fetchListWorkers({ sort_by: 'hostname', sort_dir: 'asc' })
    const url = calledUrl()
    expect(url).toContain('sort_by=hostname')
    expect(url).toContain('sort_dir=asc')
  })
})

// ── fetchGetWorker ────────────────────────────────────────────────────────────

describe('fetchGetWorker', () => {
  it('calls /api/v1/workers/:id', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'worker-1' }))
    await fetchGetWorker('worker-1')
    expect(calledUrl()).toBe('/api/v1/workers/worker-1')
  })
})

// ── fetchListFarms ────────────────────────────────────────────────────────────

describe('fetchListFarms', () => {
  it('calls /api/v1/farms', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse([]))
    await fetchListFarms()
    expect(calledUrl()).toBe('/api/v1/farms')
  })
})

// ── fetchGetFarm ──────────────────────────────────────────────────────────────

describe('fetchGetFarm', () => {
  it('calls /api/v1/farms/:id', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'farm-1' }))
    await fetchGetFarm('farm-1')
    expect(calledUrl()).toBe('/api/v1/farms/farm-1')
  })
})

// ── fetchListQueues ───────────────────────────────────────────────────────────

describe('fetchListQueues', () => {
  it('calls /api/v1/queues?farm_id=:farmId', async () => {
    fetchMock.mockResolvedValueOnce(listResponse([]))
    await fetchListQueues('farm-abc')
    expect(calledUrl()).toBe('/api/v1/queues?farm_id=farm-abc')
  })
})

// ── fetchListUsagePools ───────────────────────────────────────────────────────

describe('fetchListUsagePools', () => {
  it('calls /api/v1/usage-pools', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse([]))
    await fetchListUsagePools()
    expect(calledUrl()).toBe('/api/v1/usage-pools')
  })
})

// ── fetchGetUsagePool ─────────────────────────────────────────────────────────

describe('fetchGetUsagePool', () => {
  it('calls /api/v1/usage-pools/:id', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'pool-1' }))
    await fetchGetUsagePool('pool-1')
    expect(calledUrl()).toBe('/api/v1/usage-pools/pool-1')
  })
})
