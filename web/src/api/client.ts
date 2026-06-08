// SPDX-License-Identifier: AGPL-3.0-only

/** RFC 7807 problem-details body returned by the server on non-2xx responses. */
export interface ProblemDetail {
  type: string
  title: string
  status: number
  detail: string
  instance?: string
}

/** Typed error thrown by {@link apiFetch} on non-2xx HTTP responses. */
export class ApiError extends Error {
  readonly status: number
  readonly title: string
  readonly detail: string
  readonly instance: string | undefined
  readonly type: string

  constructor(problem: ProblemDetail) {
    super(problem.detail)
    this.name = 'ApiError'
    this.status = problem.status
    this.title = problem.title
    this.detail = problem.detail
    this.instance = problem.instance
    this.type = problem.type
  }
}

const rawBase: string = import.meta.env.VITE_API_BASE_URL ?? '/'
// Strip trailing slash so path concatenation always yields exactly one slash.
const BASE = rawBase.endsWith('/') ? rawBase.slice(0, -1) : rawBase

/**
 * Normalises a HeadersInit value into a plain Record so it can be spread
 * safely onto an object literal. Both Headers instances and string[][] would
 * be silently lost by a bare object spread, which only copies own enumerable
 * properties.
 */
function toHeaderRecord(headers: HeadersInit | undefined): Record<string, string> {
  if (headers == null) return {}
  if (headers instanceof Headers) {
    const result: Record<string, string> = {}
    headers.forEach((value, key) => {
      result[key] = value
    })
    return result
  }
  if (Array.isArray(headers)) {
    const result: Record<string, string> = {}
    for (const [key, value] of headers) {
      result[key] = value
    }
    return result
  }
  return headers
}

/**
 * Fetches a typed response from the sqi REST API.
 *
 * Sets Accept: application/json on every request, and Content-Type:
 * application/json when the request has a body. Both can be overridden by the
 * caller via init.headers.
 *
 * @param path - API path relative to /api/v1, e.g. '/jobs'
 * @param init - optional fetch options (method, body, headers, signal, …)
 * @returns Parsed JSON body cast to T; undefined for 204 No Content.
 * @throws {@link ApiError} on non-2xx HTTP responses
 * @throws TypeError on network failures
 */
export async function apiFetch<T>(path: string, init?: RequestInit): Promise<T> {
  const url = `${BASE}/api/v1${path}`

  const defaultHeaders: Record<string, string> = { Accept: 'application/json' }
  if (init?.body !== undefined) {
    defaultHeaders['Content-Type'] = 'application/json'
  }

  const response = await fetch(url, {
    ...init,
    headers: {
      ...defaultHeaders,
      ...toHeaderRecord(init?.headers),
    },
  })

  if (!response.ok) {
    const ct = response.headers.get('Content-Type') ?? ''
    if (ct.includes('application/problem+json') || ct.includes('application/json')) {
      try {
        const problem = (await response.json()) as ProblemDetail
        throw new ApiError(problem)
      } catch (err) {
        if (err instanceof ApiError) throw err
        // JSON parse failed — fall through to synthetic error.
      }
    }
    throw new ApiError({
      type: 'about:blank',
      title: response.statusText || 'Error',
      status: response.status,
      detail: `HTTP ${response.status}: ${response.statusText || 'Unknown Error'}`,
    })
  }

  if (response.status === 204) {
    return undefined as unknown as T
  }

  return response.json() as Promise<T>
}
