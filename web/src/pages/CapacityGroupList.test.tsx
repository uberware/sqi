// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor, fireEvent, within } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router'
import { ToastProvider } from '@/components/Toast'
import CapacityGroupList from './CapacityGroupList'
import type { Principal } from '@/api/types'
import { calls, group, status, stubApi } from '@/test/capacityGroups'

vi.mock('@/auth/context', () => ({
  useAuth: vi.fn(),
}))
import { useAuth } from '@/auth/context'
import { OPERATOR_PERMISSIONS, READ_ONLY_PERMISSIONS } from '@/test/principals'

function principal(permissions: Principal['permissions']): Principal {
  return { subject: 'u', display_name: 'U', roles: [], kind: 'user', permissions }
}

function setPrincipal(p: Principal) {
  ;(useAuth as unknown as ReturnType<typeof vi.fn>).mockReturnValue({
    principal: p,
    status: 'authed',
    refresh: () => {},
  })
}

beforeEach(() => setPrincipal(principal(OPERATOR_PERMISSIONS)))
afterEach(() => vi.restoreAllMocks())

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <ToastProvider>
          <CapacityGroupList />
        </ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('CapacityGroupList', () => {
  it('renders each group with kind, provider, priority, members and desired vs online', async () => {
    stubApi({ 'GET /capacity-groups': { body: [group()] } })
    renderPage()

    const link = await screen.findByRole('link', { name: 'cloud-spot' })
    expect(link.getAttribute('href')).toBe('/capacity-groups/cg-1')
    const row = screen.getByRole('row', { name: /cloud-spot/ })
    expect(within(row).getByText('sized')).toBeInTheDocument()
    expect(within(row).getByText('external')).toBeInTheDocument()
    expect(within(row).getByText('5')).toBeInTheDocument()
    expect(within(row).getByText('2 online / 1 offline')).toBeInTheDocument()
    expect(within(row).getByText('3 desired / 2 online')).toBeInTheDocument()
  })

  it('says "not evaluated yet" for a group whose status is null', async () => {
    stubApi({ 'GET /capacity-groups': { body: [group({ status: null })] } })
    renderPage()
    expect(await screen.findByText(/not evaluated yet/i)).toBeInTheDocument()
  })

  it('says "disabled" rather than "not evaluated yet" for a disabled group', async () => {
    stubApi({ 'GET /capacity-groups': { body: [group({ enabled: false, status: null })] } })
    renderPage()
    await screen.findByRole('link', { name: 'cloud-spot' })
    const row = screen.getByRole('row', { name: /cloud-spot/ })
    expect(within(row).getByText(/^disabled$/i)).toBeInTheDocument()
    expect(within(row).queryByText(/not evaluated yet/i)).not.toBeInTheDocument()
  })

  it('explains that desired counts every machine including retire candidates', async () => {
    stubApi({ 'GET /capacity-groups': { body: [group()] } })
    renderPage()
    await screen.findByRole('link', { name: 'cloud-spot' })
    expect(screen.getByText(/including machines marked for retirement/i)).toBeInTheDocument()
  })

  it('shows an empty state', async () => {
    stubApi({ 'GET /capacity-groups': { body: [] } })
    renderPage()
    expect(await screen.findByText(/no capacity groups yet/i)).toBeInTheDocument()
  })

  it('shows a load error', async () => {
    stubApi({ 'GET /capacity-groups': { status: 500, body: { detail: 'boom' } } })
    renderPage()
    expect(await screen.findByText(/failed to load capacity groups/i)).toBeInTheDocument()
  })

  it('deletes a group after confirmation and surfaces a 409 from the server', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    const fetchMock = stubApi({
      'GET /capacity-groups': { body: [group()] },
      'DELETE /capacity-groups/cg-1': {
        status: 409,
        body: { detail: 'capacity group still has members; remove them first' },
      },
    })
    renderPage()
    fireEvent.click(
      await screen.findByRole('button', { name: /delete capacity group cloud-spot/i }),
    )
    await waitFor(() => expect(calls(fetchMock)).toContain('DELETE /capacity-groups/cg-1'))
    expect(await screen.findByText(/still has members/i)).toBeInTheDocument()
  })

  it('does not delete when the confirmation is declined', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(false)
    const fetchMock = stubApi({ 'GET /capacity-groups': { body: [group()] } })
    renderPage()
    fireEvent.click(
      await screen.findByRole('button', { name: /delete capacity group cloud-spot/i }),
    )
    expect(calls(fetchMock)).not.toContain('DELETE /capacity-groups/cg-1')
  })

  it('reports a successful delete', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    stubApi({
      'GET /capacity-groups': { body: [group()] },
      'DELETE /capacity-groups/cg-1': { status: 204 },
    })
    renderPage()
    fireEvent.click(
      await screen.findByRole('button', { name: /delete capacity group cloud-spot/i }),
    )
    expect(await screen.findByText(/capacity group "cloud-spot" deleted/i)).toBeInTheDocument()
  })

  describe('role gating (infra.manage)', () => {
    it('offers New and Delete to a manager', async () => {
      stubApi({ 'GET /capacity-groups': { body: [group({ status: status() })] } })
      renderPage()
      const newLink = await screen.findByRole('link', { name: /new capacity group/i })
      expect(newLink.getAttribute('href')).toBe('/capacity-groups/new')
      expect(
        await screen.findByRole('button', { name: /delete capacity group cloud-spot/i }),
      ).toBeInTheDocument()
    })

    it('hides New and Delete from a read-only principal', async () => {
      setPrincipal(principal(READ_ONLY_PERMISSIONS))
      stubApi({ 'GET /capacity-groups': { body: [group()] } })
      renderPage()
      await screen.findByRole('link', { name: 'cloud-spot' })
      expect(screen.queryByRole('link', { name: /new capacity group/i })).not.toBeInTheDocument()
      expect(
        screen.queryByRole('button', { name: /delete capacity group/i }),
      ).not.toBeInTheDocument()
    })
  })
})
