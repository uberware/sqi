// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { renderHook, act } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  fetchSubmitJob,
  fetchCancelJob,
  fetchRetryTask,
  fetchCancelTask,
  fetchDisableWorker,
  fetchEnableWorker,
  fetchRemoveWorker,
  fetchDeleteJob,
  fetchResumeJob,
  useSubmitJob,
  useCancelJob,
  useRetryTask,
  useCancelTask,
  useDisableWorker,
  useEnableWorker,
  useRemoveWorker,
  useDeleteJob,
  useResumeJob,
  fetchCreateStorageLocation,
  fetchUpdateStorageLocation,
  fetchDeleteStorageLocation,
  useInstallPreset,
  useLogout,
} from './mutations'
import { queryKeys } from './queries'

// ── Helpers ───────────────────────────────────────────────────────────────────

const fetchMock = vi.fn<typeof fetch>()

beforeEach(() => {
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
})

afterEach(() => {
  vi.restoreAllMocks()
})

function makeOkResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function calledUrl(): string {
  return (fetchMock.mock.calls[0] as [string, ...unknown[]])[0]
}
function calledInit(): RequestInit {
  return (fetchMock.mock.calls[0] as [string, RequestInit])[1]
}

function makeClient() {
  return new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
}

function wrapper(client: QueryClient) {
  return function Wrapper({ children }: { children: React.ReactNode }) {
    return <QueryClientProvider client={client}>{children}</QueryClientProvider>
  }
}

// ── fetchSubmitJob ────────────────────────────────────────────────────────────

describe('fetchSubmitJob', () => {
  it('POSTs to /api/v1/jobs', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'job-1' }))
    await fetchSubmitJob({ farmId: 'f1', queueId: 'q1', template: '{}', format: 'json' })
    expect(calledUrl()).toContain('/api/v1/jobs')
    expect(calledInit().method).toBe('POST')
  })

  it('uses application/json content type for json format', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'job-1' }))
    await fetchSubmitJob({ farmId: 'f1', queueId: 'q1', template: '{}', format: 'json' })
    expect((calledInit().headers as Record<string, string>)['Content-Type']).toBe(
      'application/json',
    )
  })

  it('uses application/yaml content type for yaml format', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'job-1' }))
    await fetchSubmitJob({ farmId: 'f1', queueId: 'q1', template: 'name: test', format: 'yaml' })
    expect((calledInit().headers as Record<string, string>)['Content-Type']).toBe(
      'application/yaml',
    )
  })

  it('includes farm_id and queue_id in query string', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'job-1' }))
    await fetchSubmitJob({
      farmId: 'farm-abc',
      queueId: 'queue-xyz',
      template: '{}',
      format: 'json',
    })
    const url = calledUrl()
    expect(url).toContain('farm_id=farm-abc')
    expect(url).toContain('queue_id=queue-xyz')
  })

  it('includes optional fields in query string when provided', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'job-1' }))
    await fetchSubmitJob({
      farmId: 'f1',
      queueId: 'q1',
      template: '{}',
      format: 'json',
      owner: 'alice',
      submitter: 'bob',
      priority: 10,
      project: 'vfx',
    })
    const url = calledUrl()
    expect(url).toContain('owner=alice')
    expect(url).toContain('submitter=bob')
    expect(url).toContain('priority=10')
    expect(url).toContain('project=vfx')
  })

  it('omits optional fields when undefined', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'job-1' }))
    await fetchSubmitJob({ farmId: 'f1', queueId: 'q1', template: '{}', format: 'json' })
    const url = calledUrl()
    expect(url).not.toContain('owner=')
    expect(url).not.toContain('priority=')
  })

  it('includes retry-policy overrides in the query string when provided', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'job-1' }))
    await fetchSubmitJob({
      farmId: 'f1',
      queueId: 'q1',
      template: '{}',
      format: 'json',
      maxAttempts: 5,
      retryDelaySeconds: 30,
      failureLimit: 10,
    })
    const url = calledUrl()
    expect(url).toContain('max_attempts=5')
    expect(url).toContain('retry_delay_seconds=30')
    expect(url).toContain('failure_limit=10')
  })

  it('omits retry-policy overrides when undefined', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'job-1' }))
    await fetchSubmitJob({ farmId: 'f1', queueId: 'q1', template: '{}', format: 'json' })
    const url = calledUrl()
    expect(url).not.toContain('max_attempts=')
    expect(url).not.toContain('retry_delay_seconds=')
    expect(url).not.toContain('failure_limit=')
  })

  it('sends the template as the request body', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'job-1' }))
    const template = '{"name":"render"}'
    await fetchSubmitJob({ farmId: 'f1', queueId: 'q1', template, format: 'json' })
    expect(calledInit().body).toBe(template)
  })
})

// ── fetchCancelJob ────────────────────────────────────────────────────────────

describe('fetchCancelJob', () => {
  it('sends POST to /api/v1/jobs/:id/cancel', async () => {
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))
    await fetchCancelJob('job-1')
    expect(calledUrl()).toBe('/api/v1/jobs/job-1/cancel')
    expect(calledInit().method).toBe('POST')
  })
})

// ── fetchDeleteJob ────────────────────────────────────────────────────────────

describe('fetchDeleteJob', () => {
  it('sends DELETE to /api/v1/jobs/:id', async () => {
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))
    await fetchDeleteJob('job-1')
    expect(calledUrl()).toBe('/api/v1/jobs/job-1')
    expect(calledInit().method).toBe('DELETE')
  })
})

// ── fetchResumeJob ────────────────────────────────────────────────────────────

describe('fetchResumeJob', () => {
  it('sends PATCH to /api/v1/jobs/:id with { action: "resume" }', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'job-1', status: 'running' }))
    await fetchResumeJob('job-1')
    expect(calledUrl()).toBe('/api/v1/jobs/job-1')
    expect(calledInit().method).toBe('PATCH')
    expect(JSON.parse(calledInit().body as string)).toEqual({ action: 'resume' })
  })
})

// ── fetchRetryTask ────────────────────────────────────────────────────────────

describe('fetchRetryTask', () => {
  it('sends POST to /api/v1/tasks/:id/retry', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ task_id: 'task-1', status: 'pending' }))
    await fetchRetryTask('task-1')
    expect(calledUrl()).toBe('/api/v1/tasks/task-1/retry')
    expect(calledInit().method).toBe('POST')
  })
})

// ── fetchCancelTask ───────────────────────────────────────────────────────────

describe('fetchCancelTask', () => {
  it('sends POST to /api/v1/tasks/:id/cancel', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ task_id: 'task-1', status: 'canceled' }))
    await fetchCancelTask('task-1')
    expect(calledUrl()).toBe('/api/v1/tasks/task-1/cancel')
    expect(calledInit().method).toBe('POST')
  })
})

// ── fetchDisableWorker ────────────────────────────────────────────────────────

describe('fetchDisableWorker', () => {
  it('sends POST to /api/v1/workers/:id/disable', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'w-1', status: 'disabled' }))
    await fetchDisableWorker('w-1')
    expect(calledUrl()).toBe('/api/v1/workers/w-1/disable')
    expect(calledInit().method).toBe('POST')
  })
})

// ── fetchEnableWorker ─────────────────────────────────────────────────────────

describe('fetchEnableWorker', () => {
  it('sends POST to /api/v1/workers/:id/enable', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'w-1', status: 'online' }))
    await fetchEnableWorker('w-1')
    expect(calledUrl()).toBe('/api/v1/workers/w-1/enable')
    expect(calledInit().method).toBe('POST')
  })
})

// ── fetchRemoveWorker ─────────────────────────────────────────────────────────

describe('fetchRemoveWorker', () => {
  it('sends DELETE to /api/v1/workers/:id', async () => {
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))
    await fetchRemoveWorker('w-1')
    expect(calledUrl()).toBe('/api/v1/workers/w-1')
    expect(calledInit().method).toBe('DELETE')
  })
})

// ── mutation hooks ────────────────────────────────────────────────────────────

describe('useSubmitJob', () => {
  it('invalidates jobs on success', async () => {
    const client = makeClient()
    const spy = vi.spyOn(client, 'invalidateQueries')
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'job-1' }))

    const { result } = renderHook(() => useSubmitJob(), { wrapper: wrapper(client) })

    await act(async () => {
      await result.current.mutateAsync({
        farmId: 'f1',
        queueId: 'q1',
        template: '{}',
        format: 'json',
      })
    })

    expect(spy).toHaveBeenCalledWith({ queryKey: queryKeys.jobs.all })
  })
})

describe('useCancelJob', () => {
  it('invalidates jobs on success', async () => {
    const client = makeClient()
    const spy = vi.spyOn(client, 'invalidateQueries')
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))

    const { result } = renderHook(() => useCancelJob(), { wrapper: wrapper(client) })

    await act(async () => {
      await result.current.mutateAsync('job-1')
    })

    expect(spy).toHaveBeenCalledWith({ queryKey: queryKeys.jobs.all })
  })

  it('issues POST /jobs/{id}/cancel', async () => {
    const client = makeClient()
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))

    const { result } = renderHook(() => useCancelJob(), { wrapper: wrapper(client) })

    await act(async () => {
      await result.current.mutateAsync('job-1')
    })

    expect(calledUrl()).toContain('/jobs/job-1/cancel')
    expect(calledInit().method).toBe('POST')
  })
})

describe('useDeleteJob', () => {
  it('invalidates jobs on success', async () => {
    const client = makeClient()
    const spy = vi.spyOn(client, 'invalidateQueries')
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))

    const { result } = renderHook(() => useDeleteJob(), { wrapper: wrapper(client) })

    await act(async () => {
      await result.current.mutateAsync('job-1')
    })

    expect(spy).toHaveBeenCalledWith({ queryKey: queryKeys.jobs.all })
  })

  it('issues DELETE /jobs/{id}', async () => {
    const client = makeClient()
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))

    const { result } = renderHook(() => useDeleteJob(), { wrapper: wrapper(client) })

    await act(async () => {
      await result.current.mutateAsync('job-1')
    })

    expect(calledUrl()).toContain('/jobs/job-1')
    expect(calledInit().method).toBe('DELETE')
  })
})

describe('useResumeJob', () => {
  it('invalidates jobs on success', async () => {
    const client = makeClient()
    const spy = vi.spyOn(client, 'invalidateQueries')
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'job-1', status: 'running' }))

    const { result } = renderHook(() => useResumeJob(), { wrapper: wrapper(client) })

    await act(async () => {
      await result.current.mutateAsync('job-1')
    })

    expect(spy).toHaveBeenCalledWith({ queryKey: queryKeys.jobs.all })
  })

  it('issues PATCH /jobs/{id} with { action: "resume" }', async () => {
    const client = makeClient()
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'job-1', status: 'running' }))

    const { result } = renderHook(() => useResumeJob(), { wrapper: wrapper(client) })

    await act(async () => {
      await result.current.mutateAsync('job-1')
    })

    expect(calledUrl()).toBe('/api/v1/jobs/job-1')
    expect(calledInit().method).toBe('PATCH')
    expect(JSON.parse(calledInit().body as string)).toEqual({ action: 'resume' })
  })
})

describe('useRetryTask', () => {
  it('invalidates tasks and jobs on success', async () => {
    const client = makeClient()
    const spy = vi.spyOn(client, 'invalidateQueries')
    fetchMock.mockResolvedValueOnce(makeOkResponse({ task_id: 'task-1', status: 'pending' }))

    const { result } = renderHook(() => useRetryTask(), { wrapper: wrapper(client) })

    await act(async () => {
      await result.current.mutateAsync('task-1')
    })

    expect(spy).toHaveBeenCalledWith({ queryKey: queryKeys.tasks.all })
    expect(spy).toHaveBeenCalledWith({ queryKey: queryKeys.jobs.all })
  })
})

describe('useCancelTask', () => {
  it('invalidates tasks and jobs on success', async () => {
    const client = makeClient()
    const spy = vi.spyOn(client, 'invalidateQueries')
    fetchMock.mockResolvedValueOnce(makeOkResponse({ task_id: 'task-1', status: 'canceled' }))

    const { result } = renderHook(() => useCancelTask(), { wrapper: wrapper(client) })
    await act(async () => {
      await result.current.mutateAsync('task-1')
    })

    expect(spy).toHaveBeenCalledWith({ queryKey: queryKeys.tasks.all })
    expect(spy).toHaveBeenCalledWith({ queryKey: queryKeys.jobs.all })
  })
})

describe('useDisableWorker', () => {
  it('invalidates workers on success', async () => {
    const client = makeClient()
    const spy = vi.spyOn(client, 'invalidateQueries')
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'w-1', status: 'disabled' }))

    const { result } = renderHook(() => useDisableWorker(), { wrapper: wrapper(client) })

    await act(async () => {
      await result.current.mutateAsync('w-1')
    })

    expect(spy).toHaveBeenCalledWith({ queryKey: queryKeys.workers.all })
  })
})

describe('useRemoveWorker', () => {
  it('invalidates workers on success', async () => {
    const client = makeClient()
    const spy = vi.spyOn(client, 'invalidateQueries')
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))

    const { result } = renderHook(() => useRemoveWorker(), { wrapper: wrapper(client) })

    await act(async () => {
      await result.current.mutateAsync('w-1')
    })

    expect(spy).toHaveBeenCalledWith({ queryKey: queryKeys.workers.all })
  })
})

describe('useEnableWorker', () => {
  it('invalidates workers on success', async () => {
    const client = makeClient()
    const spy = vi.spyOn(client, 'invalidateQueries')
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'w-1', status: 'online' }))

    const { result } = renderHook(() => useEnableWorker(), { wrapper: wrapper(client) })

    await act(async () => {
      await result.current.mutateAsync('w-1')
    })

    expect(spy).toHaveBeenCalledWith({ queryKey: queryKeys.workers.all })
  })
})

// ── farm mutation hooks ───────────────────────────────────────────────────────

import {
  useCreateFarm,
  useUpdateFarm,
  useDeleteFarm,
  useCreateQueue,
  useUpdateQueue,
  useDeleteQueue,
} from './mutations'

describe('useCreateFarm', () => {
  it('invalidates farms on success', async () => {
    const client = makeClient()
    const spy = vi.spyOn(client, 'invalidateQueries')
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'farm-1' }))

    const { result } = renderHook(() => useCreateFarm(), { wrapper: wrapper(client) })

    await act(async () => {
      await result.current.mutateAsync({ name: 'render', description: '', max_concurrent_tasks: 0 })
    })

    expect(spy).toHaveBeenCalledWith({ queryKey: queryKeys.farms.all })
  })
})

describe('useUpdateFarm', () => {
  it('invalidates farms on success', async () => {
    const client = makeClient()
    const spy = vi.spyOn(client, 'invalidateQueries')
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'farm-1' }))

    const { result } = renderHook(() => useUpdateFarm(), { wrapper: wrapper(client) })

    await act(async () => {
      await result.current.mutateAsync({
        id: 'farm-1',
        input: { name: 'render', description: '', max_concurrent_tasks: 0 },
      })
    })

    expect(spy).toHaveBeenCalledWith({ queryKey: queryKeys.farms.all })
  })
})

describe('useDeleteFarm', () => {
  it('invalidates farms on success', async () => {
    const client = makeClient()
    const spy = vi.spyOn(client, 'invalidateQueries')
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))

    const { result } = renderHook(() => useDeleteFarm(), { wrapper: wrapper(client) })

    await act(async () => {
      await result.current.mutateAsync('farm-1')
    })

    expect(spy).toHaveBeenCalledWith({ queryKey: queryKeys.farms.all })
  })
})

describe('useCreateQueue', () => {
  it('invalidates queues and farms on success', async () => {
    const client = makeClient()
    const spy = vi.spyOn(client, 'invalidateQueries')
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'queue-1' }))

    const { result } = renderHook(() => useCreateQueue(), { wrapper: wrapper(client) })

    await act(async () => {
      await result.current.mutateAsync({
        farm_id: 'farm-1',
        name: 'lighting',
        description: '',
        priority: 50,
        max_concurrent_tasks: 0,
        paused: false,
      })
    })

    expect(spy).toHaveBeenCalledWith({ queryKey: queryKeys.queues.all })
    expect(spy).toHaveBeenCalledWith({ queryKey: queryKeys.farms.all })
  })
})

describe('useUpdateQueue', () => {
  it('invalidates queues and farms on success', async () => {
    const client = makeClient()
    const spy = vi.spyOn(client, 'invalidateQueries')
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'queue-1' }))

    const { result } = renderHook(() => useUpdateQueue(), { wrapper: wrapper(client) })

    await act(async () => {
      await result.current.mutateAsync({
        id: 'queue-1',
        input: {
          farm_id: 'farm-1',
          name: 'lighting',
          description: '',
          priority: 50,
          max_concurrent_tasks: 0,
          paused: false,
        },
      })
    })

    expect(spy).toHaveBeenCalledWith({ queryKey: queryKeys.queues.all })
    expect(spy).toHaveBeenCalledWith({ queryKey: queryKeys.farms.all })
  })
})

describe('useDeleteQueue', () => {
  it('invalidates queues and farms on success', async () => {
    const client = makeClient()
    const spy = vi.spyOn(client, 'invalidateQueries')
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))

    const { result } = renderHook(() => useDeleteQueue(), { wrapper: wrapper(client) })

    await act(async () => {
      await result.current.mutateAsync('queue-1')
    })

    expect(spy).toHaveBeenCalledWith({ queryKey: queryKeys.queues.all })
    expect(spy).toHaveBeenCalledWith({ queryKey: queryKeys.farms.all })
  })
})

// ── farm mutations ────────────────────────────────────────────────────────────

import { fetchCreateFarm, fetchUpdateFarm, fetchDeleteFarm } from './mutations'

describe('farm mutations', () => {
  it('fetchCreateFarm POSTs JSON to /api/v1/farms', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'farm-1' }))
    await fetchCreateFarm({ name: 'render', description: '', max_concurrent_tasks: 0 })
    expect(calledUrl()).toContain('/api/v1/farms')
    expect(calledInit().method).toBe('POST')
    expect(calledInit().body).toBe(
      JSON.stringify({ name: 'render', description: '', max_concurrent_tasks: 0 }),
    )
  })

  it('fetchUpdateFarm PUTs to /api/v1/farms/:id', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'farm-1' }))
    await fetchUpdateFarm('farm-1', { name: 'r2', description: 'd', max_concurrent_tasks: 5 })
    expect(calledUrl()).toContain('/api/v1/farms/farm-1')
    expect(calledInit().method).toBe('PUT')
  })

  it('fetchDeleteFarm DELETEs /api/v1/farms/:id', async () => {
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))
    await fetchDeleteFarm('farm-1')
    expect(calledUrl()).toContain('/api/v1/farms/farm-1')
    expect(calledInit().method).toBe('DELETE')
  })
})

// ── queue mutations ───────────────────────────────────────────────────────────

import { fetchCreateQueue, fetchUpdateQueue, fetchDeleteQueue } from './mutations'

describe('queue mutations', () => {
  const input = {
    farm_id: 'farm-1',
    name: 'lighting',
    description: '',
    priority: 50,
    max_concurrent_tasks: 0,
    paused: false,
  }

  it('fetchCreateQueue POSTs JSON to /api/v1/queues', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'queue-1' }))
    await fetchCreateQueue(input)
    expect(calledUrl()).toContain('/api/v1/queues')
    expect(calledInit().method).toBe('POST')
    expect(calledInit().body).toBe(JSON.stringify(input))
  })

  it('fetchUpdateQueue PUTs to /api/v1/queues/:id', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'queue-1' }))
    await fetchUpdateQueue('queue-1', input)
    expect(calledUrl()).toContain('/api/v1/queues/queue-1')
    expect(calledInit().method).toBe('PUT')
  })

  it('fetchDeleteQueue DELETEs /api/v1/queues/:id', async () => {
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))
    await fetchDeleteQueue('queue-1')
    expect(calledUrl()).toContain('/api/v1/queues/queue-1')
    expect(calledInit().method).toBe('DELETE')
  })
})

// ── usage pool mutation hooks ─────────────────────────────────────────────────

import {
  useCreateUsagePool,
  useUpdateUsagePool,
  useDeleteUsagePool,
  fetchCreateUsagePool,
  fetchUpdateUsagePool,
  fetchDeleteUsagePool,
} from './mutations'

const usageInput = {
  name: 'arnold',
  server_hint: '',
  max_concurrent: 10,
}

describe('useCreateUsagePool', () => {
  it('invalidates usage pools on success', async () => {
    const client = makeClient()
    const spy = vi.spyOn(client, 'invalidateQueries')
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'pool-1' }))

    const { result } = renderHook(() => useCreateUsagePool(), { wrapper: wrapper(client) })

    await act(async () => {
      await result.current.mutateAsync(usageInput)
    })

    expect(spy).toHaveBeenCalledWith({ queryKey: queryKeys.usagePools.all })
  })
})

describe('useUpdateUsagePool', () => {
  it('invalidates usage pools on success', async () => {
    const client = makeClient()
    const spy = vi.spyOn(client, 'invalidateQueries')
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'pool-1' }))

    const { result } = renderHook(() => useUpdateUsagePool(), { wrapper: wrapper(client) })

    await act(async () => {
      await result.current.mutateAsync({ id: 'pool-1', input: usageInput })
    })

    expect(spy).toHaveBeenCalledWith({ queryKey: queryKeys.usagePools.all })
  })
})

describe('useDeleteUsagePool', () => {
  it('invalidates usage pools on success', async () => {
    const client = makeClient()
    const spy = vi.spyOn(client, 'invalidateQueries')
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))

    const { result } = renderHook(() => useDeleteUsagePool(), { wrapper: wrapper(client) })

    await act(async () => {
      await result.current.mutateAsync('pool-1')
    })

    expect(spy).toHaveBeenCalledWith({ queryKey: queryKeys.usagePools.all })
  })
})

describe('usage pool mutations', () => {
  it('fetchCreateUsagePool POSTs JSON to /api/v1/usage-pools', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'pool-1' }))
    await fetchCreateUsagePool(usageInput)
    expect(calledUrl()).toContain('/api/v1/usage-pools')
    expect(calledInit().method).toBe('POST')
    expect(calledInit().body).toBe(JSON.stringify(usageInput))
  })

  it('fetchUpdateUsagePool PUTs to /api/v1/usage-pools/:id', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'pool-1' }))
    await fetchUpdateUsagePool('pool-1', usageInput)
    expect(calledUrl()).toContain('/api/v1/usage-pools/pool-1')
    expect(calledInit().method).toBe('PUT')
  })

  it('fetchDeleteUsagePool DELETEs /api/v1/usage-pools/:id', async () => {
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))
    await fetchDeleteUsagePool('pool-1')
    expect(calledUrl()).toContain('/api/v1/usage-pools/pool-1')
    expect(calledInit().method).toBe('DELETE')
  })
})

describe('storage location mutations', () => {
  it('POSTs to /storage-locations with the roots map', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'loc-1' }))
    await fetchCreateStorageLocation({
      name: 'nas_shows',
      type: 'filesystem',
      roots: { default: '/mnt/nas/shows' },
    })
    expect(calledUrl()).toContain('/storage-locations')
    expect(calledInit().method).toBe('POST')
    expect(JSON.parse(calledInit().body as string).roots.default).toBe('/mnt/nas/shows')
  })

  it('PUTs to /storage-locations/{id}', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'loc-1' }))
    await fetchUpdateStorageLocation('loc-1', { name: 'nas_shows', type: 's3' })
    expect(calledUrl()).toContain('/storage-locations/loc-1')
    expect(calledInit().method).toBe('PUT')
  })

  it('DELETEs /storage-locations/{id}', async () => {
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))
    await fetchDeleteStorageLocation('loc-1')
    expect(calledUrl()).toContain('/storage-locations/loc-1')
    expect(calledInit().method).toBe('DELETE')
  })
})

// ── useInstallPreset ──────────────────────────────────────────────────────────

describe('useInstallPreset', () => {
  it('POSTs to /presets/:name/install and invalidates presets and products on success', async () => {
    const client = makeClient()
    const spy = vi.spyOn(client, 'invalidateQueries')
    fetchMock.mockResolvedValueOnce(
      makeOkResponse({
        name: 'blender-render',
        title: 'Blender Render',
        description: '',
        category: 'rendering',
        version: '1.0.0',
        source: 'installed',
        template: 'name: x',
        format: 'yaml',
      }),
    )

    const { result } = renderHook(() => useInstallPreset(), { wrapper: wrapper(client) })

    await act(async () => {
      await result.current.mutateAsync('blender-render')
    })

    expect(calledUrl()).toContain('/presets/blender-render/install')
    expect(calledInit().method).toBe('POST')
    expect(spy).toHaveBeenCalledWith({ queryKey: queryKeys.presets.all })
    expect(spy).toHaveBeenCalledWith({ queryKey: queryKeys.products.all })
  })

  it('URL-encodes the preset name in the install path', async () => {
    const client = makeClient()
    fetchMock.mockResolvedValueOnce(makeOkResponse({ name: 'my preset', source: 'installed' }))

    const { result } = renderHook(() => useInstallPreset(), { wrapper: wrapper(client) })

    await act(async () => {
      await result.current.mutateAsync('my preset')
    })

    expect(calledUrl()).toContain('/presets/my%20preset/install')
  })
})

// ── useLogout ─────────────────────────────────────────────────────────────────

describe('useLogout', () => {
  it('drops every other cached query so the next user never sees the previous one’s data', async () => {
    const client = makeClient()
    // Data the signed-in user browsed before logging out.
    client.setQueryData(queryKeys.jobs.all, [{ id: 'job-from-user-a' }])
    client.setQueryData(queryKeys.users.all, [{ id: 'user-a' }])
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))

    const { result } = renderHook(() => useLogout(), { wrapper: wrapper(client) })
    await act(async () => {
      await result.current.mutateAsync()
    })

    expect(client.getQueryData(queryKeys.jobs.all)).toBeUndefined()
    expect(client.getQueryData(queryKeys.users.all)).toBeUndefined()
  })
})
