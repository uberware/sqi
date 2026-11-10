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

describe('provider status', () => {
  it('shows the power-state counts and the size a script group last asked for', async () => {
    stubApi({
      ...SIZED,
      'GET /capacity-groups/cg-1': {
        body: group({
          provider: 'script:burst',
          status: status({
            waking: 2,
            retiring: 3,
            asleep: 4,
            wake_failed: 5,
            requested_machines: 7,
          }),
        }),
      },
    })
    renderPage()
    const panel = await screen.findByRole('region', { name: /^status$/i })
    const stat = (label: string) => within(panel).getByText(label).closest('div')
    expect(stat('Waking')).toHaveTextContent('2')
    expect(stat('Retiring')).toHaveTextContent('3')
    expect(stat('Asleep')).toHaveTextContent('4')
    expect(stat('Wake failed')).toHaveTextContent('5')
    expect(stat('Requested machines')).toHaveTextContent('7')
  })

  it('leaves out requested machines for a group that has none', async () => {
    stubApi(SIZED)
    renderPage()
    const panel = await screen.findByRole('region', { name: /^status$/i })
    expect(within(panel).queryByText('Requested machines')).not.toBeInTheDocument()
  })

  it('shows a degraded banner with the reason, when it began and when the backoff ends', async () => {
    stubApi({
      ...SIZED,
      'GET /capacity-groups/cg-1': {
        body: group({
          provider: 'script:burst',
          status: status({
            degraded: {
              reason: 'provider refused: quota exceeded',
              since: '2026-11-07T11:00:00Z',
              backoff_until: '2999-01-01T00:00:00Z',
            },
          }),
        }),
      },
    })
    renderPage()
    const banner = await screen.findByRole('status', { name: /degraded/i })
    expect(banner).toHaveTextContent(/degraded/i)
    expect(banner).toHaveTextContent('provider refused: quota exceeded')
    expect(banner).toHaveTextContent(/since/i)
    expect(banner).toHaveTextContent(/no more machines are requested until/i)
    expect(banner.querySelectorAll('time')).toHaveLength(2)
  })

  it('shows no degraded banner while the group is healthy', async () => {
    stubApi(SIZED)
    renderPage()
    await screen.findByRole('region', { name: /^status$/i })
    expect(screen.queryByRole('status', { name: /degraded/i })).not.toBeInTheDocument()
  })

  it('shows the last provider action with its result and message', async () => {
    stubApi({
      ...SIZED,
      'GET /capacity-groups/cg-1': {
        body: group({
          provider: 'script:burst',
          status: status({
            last_action: {
              action: 'grow',
              result: 'failed',
              message: 'exit status 3: no capacity',
              at: '2026-11-07T11:30:00Z',
            },
          }),
        }),
      },
    })
    renderPage()
    const panel = await screen.findByRole('region', { name: /^status$/i })
    const last = within(panel).getByRole('group', { name: /last provider action/i })
    expect(last).toHaveTextContent('grow')
    expect(last).toHaveTextContent('failed')
    expect(last).toHaveTextContent('exit status 3: no capacity')
    expect(last.querySelector('time')).not.toBeNull()
  })

  it('says a provider group has not acted yet, and leaves the section out for external', async () => {
    stubApi({
      ...SIZED,
      'GET /capacity-groups/cg-1': { body: group({ provider: 'wake_on_lan', kind: 'named' }) },
      'GET /capacity-groups/cg-1/members': { body: [] },
    })
    const { unmount } = renderPage()
    const panel = await screen.findByRole('region', { name: /^status$/i })
    expect(within(panel).getByText(/no provider action yet/i)).toBeInTheDocument()
    unmount()

    stubApi(SIZED)
    renderPage()
    const panel2 = await screen.findByRole('region', { name: /^status$/i })
    expect(within(panel2).queryByText(/last provider action/i)).not.toBeInTheDocument()
  })

  it('shows the retire action in settings only for wake_on_lan', async () => {
    stubApi({
      ...SIZED,
      'GET /capacity-groups/cg-1': {
        body: group({ provider: 'wake_on_lan', kind: 'named', retire_action: 'shutdown' }),
      },
      'GET /capacity-groups/cg-1/members': { body: [] },
    })
    const { unmount } = renderPage()
    const settings = await screen.findByRole('region', { name: /^settings$/i })
    expect(within(settings).getByText('Retire action')).toBeInTheDocument()
    expect(within(settings).getByText('shutdown')).toBeInTheDocument()
    unmount()

    stubApi(SIZED)
    renderPage()
    const settings2 = await screen.findByRole('region', { name: /^settings$/i })
    expect(within(settings2).queryByText('Retire action')).not.toBeInTheDocument()
  })
})

describe('members power state', () => {
  const MEMBERS = [
    worker({ id: 'w-1', hostname: 'render01', name: 'render01' }),
    worker({
      id: 'w-2',
      hostname: 'render02',
      name: 'render02',
      status: 'offline',
      power_state: 'wake_failed',
      power_state_at: '2026-11-07T11:00:00Z',
      power_reason: 'did not register within 10m0s',
      cloud_instance_id: 'i-0abc123',
    }),
    worker({
      id: 'w-3',
      hostname: 'render03',
      name: 'render03',
      status: 'offline',
      power_state: 'asleep',
      power_state_at: '2026-11-07T10:00:00Z',
      has_wake_address: true,
    }),
  ]

  it('shows each member with its power state as text, the reason under it and the instance ID', async () => {
    stubApi({
      ...SIZED,
      'GET /capacity-groups/cg-1': { body: group({ provider: 'script:burst' }) },
      'GET /capacity-groups/cg-1/members': { body: MEMBERS },
    })
    renderPage()
    const table = await screen.findByRole('table', { name: /members/i })
    await within(table).findByText('render03')
    expect(within(table).getByRole('columnheader', { name: /power/i })).toBeInTheDocument()
    expect(within(table).getByRole('columnheader', { name: /instance/i })).toBeInTheDocument()

    const awake = within(table).getByRole('row', { name: /render01/ })
    expect(within(awake).getByLabelText('Power: Awake')).toHaveTextContent('Awake')

    const failed = within(table).getByRole('row', { name: /render02/ })
    expect(within(failed).getByLabelText('Power: Wake failed')).toHaveTextContent('Wake failed')
    // The reason is visible text, not only a tooltip.
    expect(within(failed).getByText('did not register within 10m0s')).toBeVisible()
    expect(within(failed).getByText('i-0abc123')).toBeInTheDocument()
    expect(failed.querySelector('time')).not.toBeNull()

    const asleep = within(table).getByRole('row', { name: /render03/ })
    expect(within(asleep).getByLabelText('Power: Asleep')).toHaveTextContent('Asleep')
  })

  it('hints at a missing wake-on-LAN address only for a wake_on_lan group', async () => {
    stubApi({
      ...NAMED,
      'GET /capacity-groups/cg-1': {
        body: group({ kind: 'named', provider: 'wake_on_lan', machine_profile: {} }),
      },
      'GET /capacity-groups/cg-1/members': { body: MEMBERS },
    })
    const { unmount } = renderPage()
    const table = await screen.findByRole('table', { name: /members/i })
    // render01 and render02 report no interface; render03 has a MAC.
    await within(table).findByText('render03')
    expect(within(table).getAllByText(/no wake-on-lan address yet/i)).toHaveLength(2)
    expect(
      within(within(table).getByRole('row', { name: /render03/ })).queryByText(
        /no wake-on-lan address yet/i,
      ),
    ).not.toBeInTheDocument()
    unmount()

    stubApi({
      ...SIZED,
      'GET /capacity-groups/cg-1/members': { body: MEMBERS },
    })
    renderPage()
    const sized = await screen.findByRole('table', { name: /members/i })
    await within(sized).findByText('render03')
    expect(within(sized).queryByText(/no wake-on-lan address yet/i)).not.toBeInTheDocument()
  })

  it('shows the note for a member the server reports has no wake address', async () => {
    stubApi({
      ...NAMED,
      'GET /capacity-groups/cg-1': {
        body: group({ kind: 'named', provider: 'wake_on_lan', machine_profile: {} }),
      },
      'GET /capacity-groups/cg-1/members': {
        body: [worker({ has_wake_address: false })],
      },
    })
    renderPage()
    expect(await screen.findByText(/no wake-on-lan address yet/i)).toBeInTheDocument()
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

  it('drops the raw token when navigating straight to another group', async () => {
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
      'GET /capacity-groups/cg-2': { body: group({ id: 'cg-2', name: 'other-spot' }) },
      'GET /capacity-groups/cg-2/members': { body: [] },
      'GET /capacity-groups/cg-2/tokens': { body: [] },
    })
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    // Start on cg-2 so it is cached: going back to it then renders at once,
    // without a loading state that would unmount the token section anyway.
    render(
      <QueryClientProvider client={client}>
        <MemoryRouter initialEntries={['/capacity-groups/cg-2']}>
          <ToastProvider>
            <Link to="/capacity-groups/cg-1">go to cloud-spot</Link>
            <Link to="/capacity-groups/cg-2">go to other-spot</Link>
            <Routes>
              <Route path="/capacity-groups/:id" element={<CapacityGroupDetail />} />
            </Routes>
          </ToastProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    )
    await screen.findByRole('heading', { name: /other-spot/i })
    fireEvent.click(screen.getByRole('link', { name: /go to cloud-spot/i }))
    await screen.findByRole('heading', { name: /cloud-spot/i })
    fireEvent.click(await screen.findByRole('button', { name: /create token/i }))
    await screen.findByLabelText(/new join token/i)

    fireEvent.click(screen.getByRole('link', { name: /go to other-spot/i }))
    await screen.findByRole('heading', { name: /other-spot/i })
    await screen.findByRole('button', { name: /create token/i })
    expect(screen.queryByLabelText(/new join token/i)).not.toBeInTheDocument()
    expect(document.body.textContent).not.toContain('sqg_RAWSECRETVALUE')
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
    ['405 (REST auth off)', 405, 'Method Not Allowed', /REST authentication is off/],
    [
      '409 (broker auth off)',
      409,
      'group tokens need nats.auth.enabled; workers on an auth-off farm join with worker.capacity_group',
      /does not authenticate workers/,
    ],
  ])(
    'explains worker.capacity_group when the server answers %s',
    async (_n, code, detail, cause) => {
      stubApi({
        ...SIZED,
        'POST /capacity-groups/cg-1/tokens': { status: code, body: { detail } },
      })
      renderPage()
      fireEvent.click(await screen.findByRole('button', { name: /create token/i }))
      const note = await screen.findByRole('note')
      expect(note).toHaveTextContent('worker.capacity_group')
      expect(note).toHaveTextContent('cloud-spot')
      expect(note).toHaveTextContent(cause)
      expect(screen.queryByRole('button', { name: /create token/i })).not.toBeInTheDocument()
    },
  )

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
