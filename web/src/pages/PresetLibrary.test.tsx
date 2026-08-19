// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, Routes, Route } from 'react-router'
import { ToastProvider } from '@/components/Toast'
import PresetLibrary from './PresetLibrary'
import type { PresetListItem } from '@/api/types'

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

function err503(): Response {
  return new Response(
    JSON.stringify({
      type: 'about:blank',
      title: 'Service Unavailable',
      status: 503,
      detail: 'Preset library not configured',
    }),
    {
      status: 503,
      headers: { 'Content-Type': 'application/problem+json' },
    },
  )
}

function makePreset(over: Partial<PresetListItem> = {}): PresetListItem {
  return {
    name: 'nuke-comp',
    title: 'Nuke Composite',
    description: 'Renders a Nuke composite',
    category: 'Compositing',
    version: '1.0.0',
    status: 'not_installed',
    ...over,
  }
}

function renderPage(route = '/presets') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[route]}>
        <ToastProvider>
          <Routes>
            <Route path="/presets" element={<PresetLibrary />} />
          </Routes>
        </ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('PresetLibrary', () => {
  it('renders a row per preset with its status tag', async () => {
    fetchMock.mockResolvedValueOnce(
      ok([
        makePreset({ name: 'nuke-comp', title: 'Nuke Composite', status: 'not_installed' }),
        makePreset({ name: 'houdini-sim', title: 'Houdini Sim', status: 'installed' }),
        makePreset({ name: 'maya-render', title: 'Maya Render', status: 'update_available' }),
      ]),
    )
    renderPage()
    expect(await screen.findByRole('link', { name: 'Nuke Composite' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Houdini Sim' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Maya Render' })).toBeInTheDocument()
    expect(screen.getByText('Not installed')).toBeInTheDocument()
    expect(screen.getByText('Installed')).toBeInTheDocument()
    expect(screen.getByText('Update available')).toBeInTheDocument()
  })

  it('groups presets by category with a heading per category, sorted alphabetically', async () => {
    fetchMock.mockResolvedValueOnce(
      ok([
        makePreset({ name: 'r1', title: 'R One', category: 'Rendering' }),
        makePreset({ name: 'c1', title: 'C One', category: 'Compositing' }),
        makePreset({ name: 't1', title: 'T One', category: 'Testing' }),
      ]),
    )
    renderPage()
    await screen.findByRole('link', { name: 'R One' })
    const headings = screen.getAllByRole('heading', { level: 2 }).map((h) => h.textContent)
    expect(headings).toEqual(['Compositing', 'Rendering', 'Testing'])
  })

  it('does not render a Category column', async () => {
    fetchMock.mockResolvedValueOnce(ok([makePreset()]))
    renderPage()
    await screen.findByRole('link', { name: 'Nuke Composite' })
    expect(screen.queryByRole('columnheader', { name: 'Category' })).not.toBeInTheDocument()
  })

  it('shows the not-configured state when GET /presets returns 503', async () => {
    fetchMock.mockResolvedValueOnce(err503())
    renderPage()
    expect(await screen.findByText(/no preset library/i)).toBeInTheDocument()
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
  })

  it('clicking Refresh sends a request whose URL contains ?refresh=true', async () => {
    fetchMock.mockResolvedValueOnce(ok([makePreset()]))
    renderPage()
    await screen.findByRole('link', { name: 'Nuke Composite' })

    fetchMock.mockResolvedValueOnce(ok([makePreset({ title: 'Nuke Composite (updated)' })]))
    await userEvent.click(screen.getByRole('button', { name: /refresh/i }))

    await waitFor(() => {
      const refreshCall = fetchMock.mock.calls.find(
        (c) => typeof c[0] === 'string' && c[0].includes('?refresh=true'),
      )
      expect(refreshCall).toBeDefined()
    })
  })

  it('filters presets as you type and hides emptied categories', async () => {
    fetchMock.mockResolvedValueOnce(
      ok([
        makePreset({ name: 'nuke-comp', title: 'Nuke Composite', category: 'Compositing' }),
        makePreset({ name: 'maya-render', title: 'Maya Render', category: 'Rendering' }),
      ]),
    )
    renderPage()
    await screen.findByRole('link', { name: 'Nuke Composite' })

    await userEvent.type(screen.getByRole('searchbox'), 'maya')
    await waitFor(() =>
      expect(screen.queryByRole('link', { name: 'Nuke Composite' })).not.toBeInTheDocument(),
    )
    expect(screen.getByRole('link', { name: 'Maya Render' })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Compositing' })).not.toBeInTheDocument()
  })

  it('shows a no-match message when the filter empties the library', async () => {
    fetchMock.mockResolvedValueOnce(ok([makePreset()]))
    renderPage('/presets?search=zzz')
    expect(await screen.findByText(/No presets match/)).toBeInTheDocument()
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
  })

  it('does not render the search box in the not-configured state', async () => {
    fetchMock.mockResolvedValueOnce(err503())
    renderPage()
    await screen.findByText(/no preset library/i)
    expect(screen.queryByRole('searchbox')).not.toBeInTheDocument()
  })
})

describe('PresetLibrary readme button', () => {
  it('links to the preset detail page with the readme tab open', async () => {
    fetchMock.mockResolvedValue(ok([makePreset({ name: 'nuke-comp' })]))
    renderPage()
    const link = await screen.findByRole('link', { name: /view readme for nuke-comp/i })
    expect(link).toHaveAttribute('href', '/presets/nuke-comp?tab=readme')
  })

  // The remote library index deliberately carries no readme field, so a list
  // row cannot know whether one exists. The button is therefore always
  // enabled and the detail page shows whatever is actually there. If this ever
  // becomes a disabled-state test, the index format changed — read the spec.
  it('is enabled even though the index carries no readme', async () => {
    fetchMock.mockResolvedValue(ok([makePreset({ name: 'nuke-comp' })]))
    renderPage()
    const link = await screen.findByRole('link', { name: /view readme for nuke-comp/i })
    expect(link).not.toHaveAttribute('aria-disabled', 'true')
    expect(screen.queryByRole('button', { name: /view readme for nuke-comp/i })).toBeNull()
  })
})
