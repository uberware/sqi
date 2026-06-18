// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it, vi, afterEach } from 'vitest'
import { fetchDiagnosticsLogs } from '@/api/diagnostics'

afterEach(() => vi.restoreAllMocks())

describe('fetchDiagnosticsLogs', () => {
  it('builds the query string and returns records', async () => {
    const spy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(
        JSON.stringify({
          records: [
            { ts: '2026-06-17T12:00:00Z', component: 'server', level: 'INFO', msg: 'hi' },
          ],
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ),
    )

    const out = await fetchDiagnosticsLogs({ component: 'worker:w1', level: 'WARN', limit: 50 })

    expect(out.records).toHaveLength(1)
    expect(out.records[0]?.msg).toBe('hi')
    const url = spy.mock.calls[0]?.[0] as string
    expect(url).toContain('/api/v1/diagnostics/logs?')
    expect(url).toContain('component=worker%3Aw1')
    expect(url).toContain('level=WARN')
    expect(url).toContain('limit=50')
  })

  it('omits absent params', async () => {
    const spy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ records: [] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    )
    await fetchDiagnosticsLogs({ component: 'server' })
    const url = spy.mock.calls[0]?.[0] as string
    expect(url).toContain('component=server')
    expect(url).not.toContain('level=')
    expect(url).not.toContain('limit=')
  })
})
