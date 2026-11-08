// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor, fireEvent, within } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, Routes, Route, Link } from 'react-router'
import { ToastProvider } from '@/components/Toast'
import CapacityGroupDetail from './CapacityGroupDetail'
import type { CapacityGroupToken, Principal } from '@/api/types'
import { bodyOf, calls, group, status, stubApi, worker } from '@/test/capacityGroups'

vi.mock('@/auth/context', () => ({
  useAuth: vi.fn(),
}))
import { useAuth } from '@/auth/context'
import { OPERATOR_PERMISSIONS, READ_ONLY_PERMISSIONS } from '@/test/principals'

function setPermissions(permissions: Principal['permissions']) {
  ;(useAuth as unknown as ReturnType<typeof vi.fn>).mockReturnValue({
    principal: { subject: 'u', display_name: 'U', roles: [], kind: 'user', permissions },
    status: 'authed',
    refresh: () => {},
  })
}

beforeEach(() => setPermissions(OPERATOR_PERMISSIONS))
afterEach(() => vi.restoreAllMocks())

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={['/capacity-groups/cg-1']}>
        <ToastProvider>
          <Routes>
            <Route path="/capacity-groups/:id" element={<CapacityGroupDetail />} />
            <Route
              path="/capacity-groups"
              element={
                <div>
                  group list <Link to="/capacity-groups/cg-1">open cloud-spot</Link>
                </div>
              }
            />
            <Route path="/capacity-groups/:id/edit" element={<div>edit page</div>} />
          </Routes>
        </ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

const LIVE = { body: { items: [], total: 0, limit: 1000, offset: 0 } }

function token(overrides: Partial<CapacityGroupToken> = {}): CapacityGroupToken {
  return {
    id: 't-1',
    name: 'pool bootstrap',
    prefix: 'sqg_ab12',
    expires_at: '2027-01-01T00:00:00Z',
    revoked_at: null,
    created_by: 'alice',
    created_at: '2026-11-01T00:00:00Z',
    ...overrides,
  }
}

const SIZED = {
  'GET /capacity-groups/cg-1': { body: group() },
  'GET /capacity-groups/cg-1/members': { body: [worker()] },
  'GET /capacity-groups/cg-1/tokens': { body: [token()] },
  'GET /workers': LIVE,
}

const NAMED = {
  'GET /capacity-groups/cg-1': { body: group({ kind: 'named', machine_profile: {} }) },
  'GET /capacity-groups/cg-1/members': {
    body: [
      worker(),
      worker({ id: 'w-3', hostname: 'render03', name: 'render03', status: 'offline' }),
    ],
  },
  'GET /workers': {
    body: {
      items: [
        worker(),
        worker({ id: 'w-2', hostname: 'render02', name: 'render02' }),
        worker({ id: 'w-3', hostname: 'render03', name: 'render03', status: 'offline' }),
      ],
      total: 3,
      limit: 1000,
      offset: 0,
    },
  },
}

describe('status panel', () => {
  it('shows demand, desired, members, retire candidates, mismatches and when it was evaluated', async () => {
    stubApi({
      ...SIZED,
      'GET /capacity-groups/cg-1': {
        body: group({
          status: status({
            demand_cores: 24,
            desired_machines: 4,
            online_members: 3,
            offline_members: 1,
            retire_candidates: ['w-9'],
            mismatches: [
              { worker_id: 'w-7', reason: 'cpu_count 2 is below 8; os "windows" is not "linux"' },
            ],
          }),
        }),
      },
    })
    renderPage()
    const panel = await screen.findByRole('region', { name: /^status$/i })
    expect(within(panel).getByText('24')).toBeInTheDocument()
    expect(within(panel).getByText('4')).toBeInTheDocument()
    expect(within(panel).getByText('3')).toBeInTheDocument()
    expect(within(panel).getByText('1')).toBeInTheDocument()
    expect(within(panel).getByRole('link', { name: 'w-9' })).toBeInTheDocument()
    expect(within(panel).getByRole('link', { name: 'w-7' })).toBeInTheDocument()
    expect(within(panel).getByText(/cpu_count 2 is below 8/)).toBeInTheDocument()
    expect(within(panel).getByText(/including machines marked for retirement/i)).toBeInTheDocument()
    expect(panel.querySelector('time')).not.toBeNull()
  })

  it('says "not evaluated yet" when the status is null', async () => {
    stubApi({ ...SIZED, 'GET /capacity-groups/cg-1': { body: group({ status: null }) } })
    renderPage()
    const panel = await screen.findByRole('region', { name: /^status$/i })
    expect(within(panel).getByText(/not evaluated yet/i)).toBeInTheDocument()
  })

  it('says the group is disabled rather than unevaluated', async () => {
    stubApi({
      ...SIZED,
      'GET /capacity-groups/cg-1': { body: group({ enabled: false, status: null }) },
    })
    renderPage()
    const panel = await screen.findByRole('region', { name: /^status$/i })
    expect(within(panel).getByText(/group is disabled/i)).toBeInTheDocument()
  })

  it('shows a load error for a missing group', async () => {
    stubApi({
      'GET /capacity-groups/cg-1': { status: 404, body: { detail: 'capacity group not found' } },
    })
    renderPage()
    expect(await screen.findByText(/failed to load capacity group/i)).toBeInTheDocument()
  })
})

describe('members of a named group', () => {
  it('lists members and removes one through DELETE', async () => {
    const fetchMock = stubApi({
      ...NAMED,
      'DELETE /capacity-groups/cg-1/members/w-3': { status: 204 },
    })
    renderPage()
    const table = await screen.findByRole('table', { name: /members/i })
    expect(await within(table).findByRole('link', { name: 'render03' })).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: /remove render03 from group/i }))
    await waitFor(() =>
      expect(calls(fetchMock)).toContain('DELETE /capacity-groups/cg-1/members/w-3'),
    )
  })

  it('adds a worker picked from the worker list, offering only non-members', async () => {
    const fetchMock = stubApi({
      ...NAMED,
      'PUT /capacity-groups/cg-1/members/w-2': { body: worker({ id: 'w-2' }) },
    })
    renderPage()
    const picker = await screen.findByLabelText(/worker to add/i)
    await waitFor(() => expect(within(picker).getAllByRole('option').length).toBeGreaterThan(1))
    const options = within(picker)
      .getAllByRole('option')
      .map((o) => o.textContent)
    expect(options.some((t) => t?.includes('render02'))).toBe(true)
    expect(options.some((t) => t?.includes('render03'))).toBe(false)

    const add = screen.getByRole('button', { name: /add worker/i })
    expect(add).toBeDisabled()
    fireEvent.change(picker, { target: { value: 'w-2' } })
    fireEvent.click(add)
    await waitFor(() => expect(calls(fetchMock)).toContain('PUT /capacity-groups/cg-1/members/w-2'))
    // The worker list is scoped to the group's farm.
    expect(
      fetchMock.mock.calls.some(
        ([u]) => String(u).includes('/workers') && String(u).includes('farm_id=farm-1'),
      ),
    ).toBe(true)
  })

  it('shows the server reason when adding is refused', async () => {
    stubApi({
      ...NAMED,
      'PUT /capacity-groups/cg-1/members/w-2': {
        status: 409,
        body: { detail: 'the worker is already in another capacity group' },
      },
    })
    renderPage()
    const picker = await screen.findByLabelText(/worker to add/i)
    await waitFor(() => expect(within(picker).getAllByRole('option').length).toBeGreaterThan(1))
    fireEvent.change(picker, { target: { value: 'w-2' } })
    fireEvent.click(screen.getByRole('button', { name: /add worker/i }))
    expect(await screen.findByText(/already in another capacity group/i)).toBeInTheDocument()
  })

  it('hides the token section entirely for a named group', async () => {
    const fetchMock = stubApi(NAMED)
    renderPage()
    await screen.findByRole('table', { name: /members/i })
    expect(screen.queryByRole('region', { name: /join tokens/i })).not.toBeInTheDocument()
    expect(calls(fetchMock).some((c) => c.includes('/tokens'))).toBe(false)
  })
})

describe('members of a sized group', () => {
  it('are read-only: no add picker and no remove buttons', async () => {
    stubApi(SIZED)
    renderPage()
    const table = await screen.findByRole('table', { name: /members/i })
    expect(await within(table).findByRole('link', { name: 'render01' })).toBeInTheDocument()
    expect(screen.queryByLabelText(/worker to add/i)).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /remove .* from group/i })).not.toBeInTheDocument()
  })
})

describe('join tokens (sized groups)', () => {
  it('lists tokens with name, prefix, expiry, revoked state and creator', async () => {
    stubApi({
      ...SIZED,
      'GET /capacity-groups/cg-1/tokens': {
        body: [
          token(),
          token({ id: 't-2', name: 'old', prefix: 'sqg_cd34', revoked_at: '2026-11-02T00:00:00Z' }),
        ],
      },
    })
    renderPage()
    const table = await screen.findByRole('table', { name: /join tokens/i })
    expect(await within(table).findByText('pool bootstrap')).toBeInTheDocument()
    expect(within(table).getByText('sqg_ab12', { selector: 'td' })).toBeInTheDocument()
    expect(within(table).getAllByText('alice')).toHaveLength(2)
    expect(within(table).getByText(/^revoked$/i, { selector: 'span' })).toBeInTheDocument()
    // Only the live token has a revoke control.
    expect(screen.getByRole('button', { name: /revoke token pool bootstrap/i })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /revoke token old/i })).not.toBeInTheDocument()
  })

  it('mints a token with a 720h default, shows it once, and drops it on navigation', async () => {
    const fetchMock = stubApi({
      ...SIZED,
      'POST /capacity-groups/cg-1/tokens': {
        status: 201,
        body: {
          id: 't-9',
          token: 'sqg_RAWSECRETVALUE',
          prefix: 'sqg_RAWS',
          name: 'fresh',
          expires_at: '2026-12-07T00:00:00Z',
          created_at: '2026-11-07T00:00:00Z',
        },
      },
    })
    renderPage()
    fireEvent.change(await screen.findByLabelText(/token name/i), { target: { value: 'fresh' } })
    expect(screen.getByLabelText(/expires in/i)).toHaveValue('720h')
    fireEvent.click(screen.getByRole('button', { name: /create token/i }))

    const field = await screen.findByLabelText(/new join token/i)
    expect(field).toHaveValue('sqg_RAWSECRETVALUE')
    expect(field).toHaveAttribute('readonly')
    expect(screen.getByText(/will not be shown again/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /copy token/i })).toBeInTheDocument()
    expect(bodyOf(fetchMock, 'POST /capacity-groups/cg-1/tokens')).toEqual({
      name: 'fresh',
      expires_in: '720h',
    })

    // Navigate away and come back: the raw token is gone.
    fireEvent.click(screen.getByRole('link', { name: /capacity groups/i }))
    fireEvent.click(await screen.findByRole('link', { name: /open cloud-spot/i }))
    await screen.findByRole('button', { name: /create token/i })
    expect(screen.queryByDisplayValue('sqg_RAWSECRETVALUE')).not.toBeInTheDocument()
    expect(screen.queryByText(/will not be shown again/i)).not.toBeInTheDocument()
    expect(document.body.textContent).not.toContain('sqg_RAWSECRETVALUE')
  })

  it('clears the token when Done is pressed', async () => {
    stubApi({
      ...SIZED,
      'POST /capacity-groups/cg-1/tokens': {
        status: 201,
        body: {
          id: 't-9',
          token: 'sqg_RAWSECRETVALUE',
          prefix: 'sqg_RAWS',
          name: '',
          expires_at: '2026-12-07T00:00:00Z',
          created_at: '2026-11-07T00:00:00Z',
        },
      },
    })
    renderPage()
    fireEvent.click(await screen.findByRole('button', { name: /create token/i }))
    await screen.findByLabelText(/new join token/i)
    fireEvent.click(screen.getByRole('button', { name: /done/i }))
    expect(screen.queryByLabelText(/new join token/i)).not.toBeInTheDocument()
  })

  it('omits an empty name from the request', async () => {
    const fetchMock = stubApi({
      ...SIZED,
      'POST /capacity-groups/cg-1/tokens': {
        status: 400,
        body: { detail: 'expires_in must be at most 2160h0m0s' },
      },
    })
    renderPage()
    fireEvent.change(await screen.findByLabelText(/expires in/i), { target: { value: '9999h' } })
    fireEvent.click(screen.getByRole('button', { name: /create token/i }))
    await waitFor(() => expect(calls(fetchMock)).toContain('POST /capacity-groups/cg-1/tokens'))
    expect(bodyOf(fetchMock, 'POST /capacity-groups/cg-1/tokens')).toEqual({ expires_in: '9999h' })
    expect(await screen.findByText(/expires_in must be at most/i)).toBeInTheDocument()
  })

  it('revokes a token and says credentials already issued are not revoked', async () => {
    const fetchMock = stubApi({
      ...SIZED,
      'DELETE /capacity-groups/cg-1/tokens/t-1': { status: 204 },
    })
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    renderPage()
    expect(
      await screen.findByText(/does not revoke credentials already issued/i),
    ).toBeInTheDocument()
    fireEvent.click(await screen.findByRole('button', { name: /revoke token pool bootstrap/i }))
    await waitFor(() =>
      expect(calls(fetchMock)).toContain('DELETE /capacity-groups/cg-1/tokens/t-1'),
    )
  })

  it.each([
    ['405 (REST auth off)', 405, 'Method Not Allowed'],
    [
      '409 (broker auth off)',
      409,
      'group tokens need nats.auth.enabled; workers on an auth-off farm join with worker.capacity_group',
    ],
  ])('explains worker.capacity_group when the server answers %s', async (_n, code, detail) => {
    stubApi({
      ...SIZED,
      'POST /capacity-groups/cg-1/tokens': { status: code, body: { detail } },
    })
    renderPage()
    fireEvent.click(await screen.findByRole('button', { name: /create token/i }))
    const note = await screen.findByRole('note')
    expect(note).toHaveTextContent('worker.capacity_group')
    expect(note).toHaveTextContent('cloud-spot')
    expect(screen.queryByRole('button', { name: /create token/i })).not.toBeInTheDocument()
  })

  it('shows any other token failure as an error, keeping the button', async () => {
    stubApi({
      ...SIZED,
      'POST /capacity-groups/cg-1/tokens': {
        status: 500,
        body: { detail: 'failed to create group token' },
      },
    })
    renderPage()
    fireEvent.click(await screen.findByRole('button', { name: /create token/i }))
    expect(await screen.findByText(/failed to create group token/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /create token/i })).toBeInTheDocument()
  })
})

describe('role gating', () => {
  it('shows a manager the edit link and delete button', async () => {
    stubApi(SIZED)
    renderPage()
    const edit = await screen.findByRole('link', { name: /^edit$/i })
    expect(edit.getAttribute('href')).toBe('/capacity-groups/cg-1/edit')
    expect(screen.getByRole('button', { name: /delete capacity group/i })).toBeInTheDocument()
  })

  it('gives a read-only principal no edit, delete, member or token controls', async () => {
    setPermissions(READ_ONLY_PERMISSIONS)
    const fetchMock = stubApi(NAMED)
    renderPage()
    await screen.findByRole('table', { name: /members/i })
    expect(screen.queryByRole('link', { name: /^edit$/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /delete capacity group/i })).not.toBeInTheDocument()
    expect(screen.queryByLabelText(/worker to add/i)).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /remove .* from group/i })).not.toBeInTheDocument()
    expect(calls(fetchMock).some((c) => c.includes('/tokens'))).toBe(false)
  })

  it('hides the token section from a read-only principal on a sized group', async () => {
    setPermissions(READ_ONLY_PERMISSIONS)
    const fetchMock = stubApi(SIZED)
    renderPage()
    await screen.findByRole('table', { name: /members/i })
    expect(screen.queryByRole('region', { name: /join tokens/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /create token/i })).not.toBeInTheDocument()
    expect(calls(fetchMock).some((c) => c.includes('/tokens'))).toBe(false)
    // The read-only view still shows the status and members.
    expect(screen.getByRole('region', { name: /^status$/i })).toBeInTheDocument()
  })
})
