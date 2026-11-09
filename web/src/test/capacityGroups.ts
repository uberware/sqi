// SPDX-License-Identifier: AGPL-3.0-or-later

// Canned capacity-group wire objects and a path-routed fetch mock for the
// capacity-group page tests. Pages poll and fan out several requests, so a
// router keyed on method + path survives call-order changes that a chain of
// mockResolvedValueOnce would not.

import { vi } from 'vitest'
import type { CapacityGroup, CapacityGroupStatus, Worker } from '@/api/types'

export function status(overrides: Partial<CapacityGroupStatus> = {}): CapacityGroupStatus {
  return {
    demand_cores: 16,
    desired_machines: 3,
    online_members: 2,
    offline_members: 1,
    waking: 0,
    retiring: 0,
    asleep: 0,
    wake_failed: 0,
    wake_candidates: [],
    retire_candidates: [],
    mismatches: [],
    requested_machines: null,
    degraded: null,
    last_action: null,
    evaluated_at: '2026-11-07T12:00:00Z',
    ...overrides,
  }
}

export function group(overrides: Partial<CapacityGroup> = {}): CapacityGroup {
  return {
    id: 'cg-1',
    farm_id: 'farm-1',
    name: 'cloud-spot',
    kind: 'sized',
    provider: 'external',
    enabled: true,
    queue_ids: [],
    min_machines: 0,
    max_machines: 10,
    priority: 5,
    max_step_up: 2,
    scale_up_cooldown_seconds: 60,
    idle_timeout_seconds: 600,
    boot_timeout_seconds: 900,
    retire_action: 'sleep',
    machine_profile: { cpu_count: 8, ram_mb: 16384, expr_limits: { operation_limit: 5000 } },
    created_at: '2026-11-01T00:00:00Z',
    updated_at: '2026-11-01T00:00:00Z',
    status: status(),
    ...overrides,
  }
}

export function worker(overrides: Partial<Worker> = {}): Worker {
  return {
    id: 'w-1',
    farm_id: 'farm-1',
    hostname: 'render01',
    name: 'render01',
    gpu: { count: 0 },
    status: 'online',
    removable: false,
    power_state: 'awake',
    interfaces: [],
    registered_at: '2026-11-01T00:00:00Z',
    updated_at: '2026-11-01T00:00:00Z',
    ...overrides,
  }
}

export interface MockReply {
  status?: number
  body?: unknown
}

export type Routes = Record<string, MockReply | ((init: RequestInit | undefined) => MockReply)>

/**
 * Installs a fetch stub answering from `routes`, keyed `"METHOD /path"` where
 * the path is the API path without the `/api/v1` prefix. An unrouted request
 * answers 404 so a stray call fails the test visibly. Returns the mock so a
 * test can inspect calls.
 */
export function stubApi(routes: Routes) {
  const fetchMock = vi.fn<typeof fetch>((input, init) => {
    const url = new URL(String(input), 'http://localhost')
    const path = url.pathname.replace(/^\/api\/v1/, '')
    const key = `${init?.method ?? 'GET'} ${path}`
    const route = routes[key]
    if (route === undefined) {
      return Promise.resolve(problem(404, `no mock for ${key}`))
    }
    const reply = typeof route === 'function' ? route(init) : route
    const code = reply.status ?? 200
    if (code >= 400) {
      return Promise.resolve(problem(code, String((reply.body as { detail?: string })?.detail)))
    }
    return Promise.resolve(
      new Response(code === 204 ? null : JSON.stringify(reply.body), {
        status: code,
        headers: { 'Content-Type': 'application/json' },
      }),
    )
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

function problem(code: number, detail: string): Response {
  return new Response(
    JSON.stringify({ type: 'about:blank', title: 'Error', status: code, detail }),
    { status: code, headers: { 'Content-Type': 'application/problem+json' } },
  )
}

/** The calls the stub saw, as `"METHOD /path"` strings. */
export function calls(fetchMock: ReturnType<typeof stubApi>): string[] {
  return fetchMock.mock.calls.map(([input, init]) => {
    const url = new URL(String(input), 'http://localhost')
    return `${init?.method ?? 'GET'} ${url.pathname.replace(/^\/api\/v1/, '')}`
  })
}

/** The parsed JSON body of the first call matching `"METHOD /path"`. */
export function bodyOf(fetchMock: ReturnType<typeof stubApi>, key: string): unknown {
  const i = calls(fetchMock).indexOf(key)
  const init = fetchMock.mock.calls[i]?.[1]
  return init?.body === undefined ? undefined : JSON.parse(init.body as string)
}
