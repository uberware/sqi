// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach } from 'vitest'
import {
  fetchRetryJob,
  fetchSubmitJob,
  fetchCreateProduct,
  fetchUpdateProduct,
  fetchDeleteProduct,
  fetchSubmitProductJob,
} from './mutations'
import type { ProductInput } from './mutations'
import { apiFetch } from './client'

vi.mock('./client', () => ({ apiFetch: vi.fn() }))

describe('fetchSubmitJob', () => {
  beforeEach(() => vi.mocked(apiFetch).mockReset())

  it('appends repeated depends_on params', async () => {
    vi.mocked(apiFetch).mockResolvedValue({ id: 'job-1' })
    await fetchSubmitJob({
      farmId: 'f',
      queueId: 'q',
      template: '{}',
      format: 'json',
      dependsOn: ['a', 'b'],
    })
    const url = vi.mocked(apiFetch).mock.calls[0]?.[0] as string
    expect(url).toContain('depends_on=a')
    expect(url).toContain('depends_on=b')
  })

  it('omits depends_on when undefined', async () => {
    vi.mocked(apiFetch).mockResolvedValue({ id: 'job-1' })
    await fetchSubmitJob({ farmId: 'f', queueId: 'q', template: '{}', format: 'json' })
    const url = vi.mocked(apiFetch).mock.calls[0]?.[0] as string
    expect(url).not.toContain('depends_on')
  })
})

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

describe('product mutations', () => {
  beforeEach(() => vi.mocked(apiFetch).mockReset())

  const input: ProductInput = {
    name: 'my-render',
    title: 'My Render',
    description: '',
    readme: '',
    category: '',
    version: '',
    template: 'name: x',
    format: 'yaml',
  }

  it('fetchCreateProduct POSTs JSON to /products', async () => {
    vi.mocked(apiFetch).mockResolvedValue({ name: 'my-render' })
    await fetchCreateProduct(input)
    expect(apiFetch).toHaveBeenCalledWith('/products', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(input),
    })
  })

  it('fetchUpdateProduct PUTs to the URL-encoded name', async () => {
    vi.mocked(apiFetch).mockResolvedValue({ name: 'studio/maya' })
    await fetchUpdateProduct('studio/maya', input)
    expect(apiFetch).toHaveBeenCalledWith('/products/studio%2Fmaya', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(input),
    })
  })

  it('fetchDeleteProduct DELETEs the URL-encoded name', async () => {
    vi.mocked(apiFetch).mockResolvedValue(undefined)
    await fetchDeleteProduct('studio/maya')
    expect(apiFetch).toHaveBeenCalledWith('/products/studio%2Fmaya', { method: 'DELETE' })
  })
})

describe('fetchSubmitProductJob', () => {
  beforeEach(() => vi.mocked(apiFetch).mockReset())

  it('posts name + params to the product jobs path', async () => {
    vi.mocked(apiFetch).mockResolvedValue({ id: 'job-1', name: 'Shot010' })
    await fetchSubmitProductJob({
      productName: 'blender',
      name: 'Shot010',
      farmId: 'f1',
      queueId: 'q1',
      parameters: { Scene: '/a.blend' },
    })
    expect(apiFetch).toHaveBeenCalledWith(
      '/products/blender/jobs',
      expect.objectContaining({ method: 'POST' }),
    )
    const body = JSON.parse((vi.mocked(apiFetch).mock.calls[0]?.[1] as RequestInit).body as string)
    expect(body).toMatchObject({
      name: 'Shot010',
      farm_id: 'f1',
      queue_id: 'q1',
      parameters: { Scene: '/a.blend' },
    })
  })

  it('includes depends_on in the JSON body when set', async () => {
    vi.mocked(apiFetch).mockResolvedValue({ id: 'job-1', name: 'Shot010' })
    await fetchSubmitProductJob({
      productName: 'blender',
      name: 'Shot010',
      farmId: 'f1',
      queueId: 'q1',
      parameters: {},
      dependsOn: ['job-a', 'job-b'],
    })
    const body = JSON.parse((vi.mocked(apiFetch).mock.calls[0]?.[1] as RequestInit).body as string)
    expect(body).toMatchObject({ depends_on: ['job-a', 'job-b'] })
  })

  it('omits depends_on from the JSON body when undefined', async () => {
    vi.mocked(apiFetch).mockResolvedValue({ id: 'job-1', name: 'Shot010' })
    await fetchSubmitProductJob({
      productName: 'blender',
      name: 'Shot010',
      farmId: 'f1',
      queueId: 'q1',
      parameters: {},
    })
    const body = JSON.parse((vi.mocked(apiFetch).mock.calls[0]?.[1] as RequestInit).body as string)
    expect(body).not.toHaveProperty('depends_on')
  })
})
