// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, Routes, Route, useLocation } from 'react-router-dom'
import { ToastProvider } from '@/components/Toast'
import PresetDetail from './PresetDetail'
import type { PresetDetail as PresetDetailType, Product } from '@/api/types'

const fetchMock = vi.fn<typeof fetch>()
beforeEach(() => {
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
})
afterEach(() => vi.restoreAllMocks())

function ok(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function makePreset(over: Partial<PresetDetailType> = {}): PresetDetailType {
  return {
    name: 'nuke-comp',
    title: 'Nuke Composite',
    description: 'Renders a Nuke composite',
    category: 'Compositing',
    version: '1.0.0',
    status: 'not_installed',
    template: 'name: nuke-template',
    format: 'yaml',
    ...over,
  }
}

function makeProduct(over: Partial<Product> = {}): Product {
  return {
    name: 'nuke-comp',
    title: 'Nuke Composite',
    description: 'Renders a Nuke composite',
    category: 'Compositing',
    version: '1.0.0',
    source: 'installed',
    template: 'name: nuke-template',
    format: 'yaml',
    ...over,
  }
}

function LocationDisplay() {
  const loc = useLocation()
  return <div data-testid="location">{loc.pathname}</div>
}

function renderDetail(path: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <ToastProvider>
          <Routes>
            <Route path="/presets/:name" element={<PresetDetail />} />
            <Route path="/products/:name" element={<LocationDisplay />} />
          </Routes>
        </ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('PresetDetail', () => {
  it('renders metadata and the OpenJD template', async () => {
    fetchMock.mockResolvedValueOnce(ok(makePreset()))
    renderDetail('/presets/nuke-comp')
    expect(
      await screen.findByRole('heading', { level: 2, name: 'Nuke Composite' }),
    ).toBeInTheDocument()
    expect(screen.getByLabelText('OpenJD template')).toHaveTextContent('name: nuke-template')
  })

  it('shows Install button when status is not_installed', async () => {
    fetchMock.mockResolvedValueOnce(ok(makePreset({ status: 'not_installed' })))
    renderDetail('/presets/nuke-comp')
    expect(await screen.findByRole('button', { name: 'Install' })).toBeInTheDocument()
  })

  it('shows Update button when status is update_available', async () => {
    fetchMock.mockResolvedValueOnce(ok(makePreset({ status: 'update_available' })))
    renderDetail('/presets/nuke-comp')
    expect(await screen.findByRole('button', { name: 'Update' })).toBeInTheDocument()
  })

  it('shows Reinstall button when status is installed', async () => {
    fetchMock.mockResolvedValueOnce(ok(makePreset({ status: 'installed' })))
    renderDetail('/presets/nuke-comp')
    expect(await screen.findByRole('button', { name: 'Reinstall' })).toBeInTheDocument()
  })

  it('clicking Install calls POST /presets/:name/install and navigates to the product', async () => {
    fetchMock.mockResolvedValueOnce(ok(makePreset()))
    renderDetail('/presets/nuke-comp')
    await screen.findByRole('button', { name: 'Install' })

    fetchMock.mockResolvedValueOnce(ok(makeProduct()))
    await userEvent.click(screen.getByRole('button', { name: 'Install' }))

    await waitFor(() => {
      const installCall = fetchMock.mock.calls.find(
        (c) =>
          typeof c[0] === 'string' &&
          c[0].includes('/presets/nuke-comp/install') &&
          (c[1] as RequestInit | undefined)?.method === 'POST',
      )
      expect(installCall).toBeDefined()
    })
    await waitFor(() =>
      expect(screen.getByTestId('location')).toHaveTextContent('/products/nuke-comp'),
    )
  })
})
