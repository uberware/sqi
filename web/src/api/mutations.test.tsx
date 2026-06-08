// SPDX-License-Identifier: AGPL-3.0-only

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { renderHook, act } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  fetchSubmitJob,
  fetchCancelJob,
  fetchRetryTask,
  fetchDisableWorker,
  fetchEnableWorker,
  useSubmitJob,
  useCancelJob,
  useRetryTask,
  useDisableWorker,
  useEnableWorker,
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

  it('sends the template as the request body', async () => {
    fetchMock.mockResolvedValueOnce(makeOkResponse({ id: 'job-1' }))
    const template = '{"name":"render"}'
    await fetchSubmitJob({ farmId: 'f1', queueId: 'q1', template, format: 'json' })
    expect(calledInit().body).toBe(template)
  })
})

// ── fetchCancelJob ────────────────────────────────────────────────────────────

describe('fetchCancelJob', () => {
  it('sends DELETE to /api/v1/jobs/:id', async () => {
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))
    await fetchCancelJob('job-1')
    expect(calledUrl()).toBe('/api/v1/jobs/job-1')
    expect(calledInit().method).toBe('DELETE')
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
