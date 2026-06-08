// SPDX-License-Identifier: AGPL-3.0-only

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { apiFetch, ApiError } from './client'
import type { ProblemDetail } from './client'

// ── Helpers ───────────────────────────────────────────────────────────────────

function makeJsonResponse(
  status: number,
  body: unknown,
  contentType = 'application/json',
): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': contentType },
  })
}

function makeProblemResponse(problem: ProblemDetail): Response {
  return new Response(JSON.stringify(problem), {
    status: problem.status,
    headers: { 'Content-Type': 'application/problem+json' },
  })
}

async function catchError(p: Promise<unknown>): Promise<unknown> {
  try {
    await p
    return undefined
  } catch (e) {
    return e
  }
}

// ── Test setup ────────────────────────────────────────────────────────────────

const fetchMock = vi.fn<typeof fetch>()

beforeEach(() => {
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
})

afterEach(() => {
  vi.restoreAllMocks()
})

// ── Tests ─────────────────────────────────────────────────────────────────────

describe('apiFetch', () => {
  describe('successful responses', () => {
    it('returns parsed JSON for a 200 response', async () => {
      const payload = { id: 'job-1', name: 'render' }
      fetchMock.mockResolvedValueOnce(makeJsonResponse(200, payload))
      const result = await apiFetch<typeof payload>('/jobs/job-1')
      expect(result).toEqual(payload)
    })

    it('returns undefined for 204 No Content', async () => {
      fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))
      const result = await apiFetch<undefined>('/jobs/job-1')
      expect(result).toBeUndefined()
    })
  })

  describe('header attachment', () => {
    it('sends Accept: application/json on every request', async () => {
      fetchMock.mockResolvedValueOnce(makeJsonResponse(200, {}))
      await apiFetch('/jobs')
      expect(fetchMock).toHaveBeenCalledWith(
        expect.any(String),
        expect.objectContaining({
          headers: expect.objectContaining({
            Accept: 'application/json',
          }),
        }),
      )
    })

    it('sends Content-Type: application/json when the request has a body', async () => {
      fetchMock.mockResolvedValueOnce(makeJsonResponse(200, {}))
      await apiFetch('/jobs', { method: 'POST', body: JSON.stringify({ name: 'test' }) })
      expect(fetchMock).toHaveBeenCalledWith(
        expect.any(String),
        expect.objectContaining({
          headers: expect.objectContaining({
            'Content-Type': 'application/json',
            Accept: 'application/json',
          }),
        }),
      )
    })

    it('caller-supplied Content-Type overrides the default and Accept is preserved', async () => {
      fetchMock.mockResolvedValueOnce(makeJsonResponse(200, {}))
      await apiFetch('/jobs', {
        method: 'POST',
        body: 'template: {}',
        headers: { 'Content-Type': 'application/yaml' },
      })
      expect(fetchMock).toHaveBeenCalledWith(
        expect.any(String),
        expect.objectContaining({
          headers: expect.objectContaining({
            'Content-Type': 'application/yaml',
            Accept: 'application/json',
          }),
        }),
      )
    })

    it('does not set Content-Type on requests without a body', async () => {
      fetchMock.mockResolvedValueOnce(makeJsonResponse(200, {}))
      await apiFetch('/jobs')
      const callArgs = fetchMock.mock.lastCall as [string, RequestInit] | undefined
      const headers = callArgs?.[1]?.headers as Record<string, string> | undefined
      expect(headers?.['Content-Type']).toBeUndefined()
      expect(headers?.['Accept']).toBe('application/json')
    })
  })

  describe('URL construction', () => {
    it('prefixes path with /api/v1', async () => {
      fetchMock.mockResolvedValueOnce(makeJsonResponse(200, {}))
      await apiFetch('/workers?status=online')
      const [url] = fetchMock.mock.calls[0] as [string, ...unknown[]]
      expect(url).toContain('/api/v1/workers')
    })
  })

  describe('error handling', () => {
    it('throws ApiError for application/problem+json responses', async () => {
      const problem: ProblemDetail = {
        type: 'about:blank',
        title: 'Not Found',
        status: 404,
        detail: 'job not found',
        instance: 'req-abc',
      }
      fetchMock.mockResolvedValueOnce(makeProblemResponse(problem))
      const err = await catchError(apiFetch('/jobs/missing'))
      expect(err).toBeInstanceOf(ApiError)
    })

    it('ApiError carries the correct RFC 7807 fields', async () => {
      const problem: ProblemDetail = {
        type: 'about:blank',
        title: 'Not Found',
        status: 404,
        detail: 'job not found',
        instance: 'req-abc',
      }
      fetchMock.mockResolvedValueOnce(makeProblemResponse(problem))
      const err = await catchError(apiFetch('/jobs/missing'))
      expect(err).toBeInstanceOf(ApiError)
      const apiErr = err as ApiError
      expect(apiErr.status).toBe(404)
      expect(apiErr.title).toBe('Not Found')
      expect(apiErr.detail).toBe('job not found')
      expect(apiErr.instance).toBe('req-abc')
      expect(apiErr.type).toBe('about:blank')
      expect(apiErr.message).toBe('job not found')
    })

    it('throws ApiError for application/json error responses', async () => {
      const problem: ProblemDetail = {
        type: 'about:blank',
        title: 'Bad Request',
        status: 400,
        detail: 'invalid body',
      }
      fetchMock.mockResolvedValueOnce(makeJsonResponse(400, problem, 'application/json'))
      const err = await catchError(apiFetch('/jobs'))
      expect(err).toBeInstanceOf(ApiError)
      expect((err as ApiError).status).toBe(400)
    })

    it('synthesizes ApiError for non-JSON error responses', async () => {
      fetchMock.mockResolvedValueOnce(
        new Response('Service Unavailable', {
          status: 503,
          statusText: 'Service Unavailable',
          headers: { 'Content-Type': 'text/plain' },
        }),
      )
      const err = await catchError(apiFetch('/jobs'))
      expect(err).toBeInstanceOf(ApiError)
      expect((err as ApiError).status).toBe(503)
    })

    it('falls back to synthetic ApiError when problem+json body is unparseable', async () => {
      fetchMock.mockResolvedValueOnce(
        new Response('not json', {
          status: 500,
          statusText: 'Internal Server Error',
          headers: { 'Content-Type': 'application/problem+json' },
        }),
      )
      const err = await catchError(apiFetch('/jobs'))
      expect(err).toBeInstanceOf(ApiError)
      expect((err as ApiError).status).toBe(500)
    })

    it('propagates TypeError on network failure', async () => {
      fetchMock.mockRejectedValueOnce(new TypeError('Failed to fetch'))
      const err = await catchError(apiFetch('/jobs'))
      expect(err).toBeInstanceOf(TypeError)
    })

    it('ApiError.instance is undefined when not present in the response', async () => {
      const problem: ProblemDetail = {
        type: 'about:blank',
        title: 'Internal Server Error',
        status: 500,
        detail: 'something went wrong',
      }
      fetchMock.mockResolvedValueOnce(makeProblemResponse(problem))
      const err = await catchError(apiFetch('/jobs'))
      expect(err).toBeInstanceOf(ApiError)
      expect((err as ApiError).instance).toBeUndefined()
    })
  })
})
