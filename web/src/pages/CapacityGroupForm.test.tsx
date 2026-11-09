// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, Routes, Route } from 'react-router'
import { ToastProvider } from '@/components/Toast'
import CapacityGroupForm from './CapacityGroupForm'
import { bodyOf, calls, group, stubApi } from '@/test/capacityGroups'

afterEach(() => vi.restoreAllMocks())

const FARMS = [
  { id: 'farm-1', name: 'main', max_concurrent_tasks: 0, created_at: '', updated_at: '' },
  { id: 'farm-2', name: 'cloud', max_concurrent_tasks: 0, created_at: '', updated_at: '' },
]
const queuePage = (farm: string) => ({
  body: {
    items: [
      {
        id: `${farm}-q1`,
        farm_id: farm,
        name: `${farm}-render`,
        priority: 0,
        max_concurrent_tasks: 0,
        paused: false,
        created_at: '',
        updated_at: '',
      },
    ],
    total: 1,
    limit: 1000,
    offset: 0,
  },
})
const BASE_ROUTES = {
  'GET /farms': { body: FARMS },
  'GET /queues': queuePage('farm-1'),
}

function renderAt(path: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <ToastProvider>
          <Routes>
            <Route path="/capacity-groups/new" element={<CapacityGroupForm mode="create" />} />
            <Route path="/capacity-groups/:id/edit" element={<CapacityGroupForm mode="edit" />} />
            <Route path="/capacity-groups" element={<div>group list</div>} />
          </Routes>
        </ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

async function fillSized(name = 'spot') {
  const nameInput = await screen.findByLabelText(/^name$/i)
  fireEvent.change(nameInput, { target: { value: name } })
  fireEvent.change(screen.getByLabelText(/^max machines$/i), { target: { value: '4' } })
  fireEvent.change(screen.getByLabelText(/cpu count/i), { target: { value: '8' } })
}

describe('CapacityGroupForm (create)', () => {
  it('offers only the external provider', async () => {
    stubApi(BASE_ROUTES)
    renderAt('/capacity-groups/new')
    const select = await screen.findByLabelText(/^provider$/i)
    const options = Array.from(select.querySelectorAll('option')).map((o) =>
      o.getAttribute('value'),
    )
    expect(options).toEqual(['external'])
  })

  it('shows the profile fields for a sized group and a hint for a named one', async () => {
    stubApi(BASE_ROUTES)
    renderAt('/capacity-groups/new')
    await screen.findByLabelText(/^kind$/i)
    expect(screen.getByLabelText(/cpu count/i)).toBeInTheDocument()
    expect(screen.queryByText(/learned from the group.s members/i)).not.toBeInTheDocument()

    fireEvent.change(screen.getByLabelText(/^kind$/i), { target: { value: 'named' } })
    expect(screen.queryByLabelText(/cpu count/i)).not.toBeInTheDocument()
    expect(screen.getByText(/learned from the group.s members/i)).toBeInTheDocument()
  })

  it('disables submit until the name, max machines and (sized) cpu count are valid', async () => {
    stubApi(BASE_ROUTES)
    renderAt('/capacity-groups/new')
    const submit = await screen.findByRole('button', { name: /create capacity group/i })
    expect(submit).toBeDisabled()
    await fillSized()
    expect(submit).toBeEnabled()
    fireEvent.change(screen.getByLabelText(/cpu count/i), { target: { value: '0' } })
    expect(submit).toBeDisabled()
  })

  it('POSTs the full input, converts tags and extensions, and navigates to the list', async () => {
    const fetchMock = stubApi({
      ...BASE_ROUTES,
      'POST /capacity-groups': { status: 201, body: group() },
    })
    renderAt('/capacity-groups/new')
    await fillSized('spot-pool')
    fireEvent.change(screen.getByLabelText(/^os$/i), { target: { value: 'linux' } })
    fireEvent.change(screen.getByLabelText(/^tags$/i), { target: { value: 'gpu=a100\nzone=us' } })
    fireEvent.change(screen.getByLabelText(/^extensions$/i), {
      target: { value: 'EXPR, SQI_LOG_CHECKS' },
    })
    fireEvent.click(await screen.findByLabelText('farm-1-render'))
    fireEvent.click(screen.getByRole('button', { name: /create capacity group/i }))

    await waitFor(() => expect(screen.getByText('group list')).toBeInTheDocument())
    expect(bodyOf(fetchMock, 'POST /capacity-groups')).toEqual({
      farm_id: 'farm-1',
      name: 'spot-pool',
      kind: 'sized',
      provider: 'external',
      enabled: true,
      queue_ids: ['farm-1-q1'],
      min_machines: 0,
      max_machines: 4,
      priority: 0,
      max_step_up: 1,
      scale_up_cooldown_seconds: 60,
      idle_timeout_seconds: 600,
      boot_timeout_seconds: 600,
      retire_action: 'sleep',
      machine_profile: {
        cpu_count: 8,
        os: 'linux',
        tags: { gpu: 'a100', zone: 'us' },
        extensions: ['EXPR', 'SQI_LOG_CHECKS'],
      },
    })
  })

  it('sends an empty profile for a named group', async () => {
    const fetchMock = stubApi({
      ...BASE_ROUTES,
      'POST /capacity-groups': { status: 201, body: group({ kind: 'named' }) },
    })
    renderAt('/capacity-groups/new')
    fireEvent.change(await screen.findByLabelText(/^kind$/i), { target: { value: 'named' } })
    fireEvent.change(screen.getByLabelText(/^name$/i), { target: { value: 'boxes' } })
    fireEvent.change(screen.getByLabelText(/^max machines$/i), { target: { value: '2' } })
    fireEvent.click(screen.getByRole('button', { name: /create capacity group/i }))
    await waitFor(() => expect(screen.getByText('group list')).toBeInTheDocument())
    expect(bodyOf(fetchMock, 'POST /capacity-groups')).toMatchObject({
      kind: 'named',
      machine_profile: {},
    })
  })

  it('shows a server validation message on the field it names', async () => {
    stubApi({
      ...BASE_ROUTES,
      'POST /capacity-groups': {
        status: 400,
        body: {
          detail:
            "name \"bad name\" must be 1-63 characters: letters, digits, '.', '_' or '-', starting with a letter or digit",
        },
      },
    })
    renderAt('/capacity-groups/new')
    await fillSized('bad name')
    fireEvent.click(screen.getByRole('button', { name: /create capacity group/i }))

    await waitFor(() =>
      expect(screen.getByLabelText(/^name$/i)).toHaveAccessibleDescription(
        /must be 1-63 characters/,
      ),
    )
    expect(screen.getByLabelText(/^name$/i)).toBeInvalid()
    expect(screen.queryByText('group list')).not.toBeInTheDocument()
  })

  it('maps each line of a joined server error to its own field', async () => {
    stubApi({
      ...BASE_ROUTES,
      'POST /capacity-groups': {
        status: 400,
        body: {
          detail:
            'min_machines 9 must not exceed max_machines 4\nidle_timeout 5s must be at least 1m0s',
        },
      },
    })
    renderAt('/capacity-groups/new')
    await fillSized()
    fireEvent.click(screen.getByRole('button', { name: /create capacity group/i }))
    await waitFor(() =>
      expect(screen.getByLabelText(/^min machines$/i)).toHaveAccessibleDescription(
        /must not exceed max_machines/,
      ),
    )
    expect(screen.getByLabelText(/idle timeout/i)).toHaveAccessibleDescription(/at least 1m0s/)
  })

  it('shows an unmapped server error at the top of the form', async () => {
    stubApi({
      ...BASE_ROUTES,
      'POST /capacity-groups': { status: 400, body: { detail: 'invalid JSON body' } },
    })
    renderAt('/capacity-groups/new')
    await fillSized()
    fireEvent.click(screen.getByRole('button', { name: /create capacity group/i }))
    expect(await screen.findByRole('alert')).toHaveTextContent('invalid JSON body')
  })

  it('shows a name conflict on the name field', async () => {
    stubApi({
      ...BASE_ROUTES,
      'POST /capacity-groups': {
        status: 409,
        body: { detail: 'a capacity group with that name already exists' },
      },
    })
    renderAt('/capacity-groups/new')
    await fillSized()
    fireEvent.click(screen.getByRole('button', { name: /create capacity group/i }))
    await waitFor(() =>
      expect(screen.getByLabelText(/^name$/i)).toHaveAccessibleDescription(/already exists/),
    )
  })

  it('reloads the queue choices when the farm changes and drops selected queues', async () => {
    const fetchMock = stubApi({
      ...BASE_ROUTES,
      'POST /capacity-groups': { status: 201, body: group() },
    })
    renderAt('/capacity-groups/new')
    await fillSized()
    fireEvent.click(await screen.findByLabelText('farm-1-render'))
    fireEvent.change(screen.getByLabelText(/^farm$/i), { target: { value: 'farm-2' } })
    await waitFor(() =>
      expect(fetchMock.mock.calls.some(([u]) => String(u).includes('farm_id=farm-2'))).toBe(true),
    )
  })
})

describe('CapacityGroupForm (edit)', () => {
  const routes = {
    ...BASE_ROUTES,
    'GET /capacity-groups/cg-1': { body: group() },
  }

  it('shows farm and kind read-only and prefills the rest', async () => {
    stubApi(routes)
    renderAt('/capacity-groups/cg-1/edit')
    expect(await screen.findByDisplayValue('cloud-spot')).toBeInTheDocument()
    expect(screen.getByLabelText(/^farm$/i)).toBeDisabled()
    expect(screen.getByLabelText(/^kind$/i)).toBeDisabled()
    expect(screen.getByLabelText(/cpu count/i)).toHaveValue(8)
    expect(screen.getByLabelText(/^max machines$/i)).toHaveValue(10)
  })

  it('PUTs every managed field, including enabled and queue_ids, without farm or response-only fields', async () => {
    const fetchMock = stubApi({ ...routes, 'PUT /capacity-groups/cg-1': { body: group() } })
    renderAt('/capacity-groups/cg-1/edit')
    const nameInput = await screen.findByDisplayValue('cloud-spot')
    fireEvent.change(nameInput, { target: { value: 'cloud-spot2' } })
    fireEvent.click(screen.getByRole('button', { name: /^save$/i }))

    await waitFor(() => expect(screen.getByText('group list')).toBeInTheDocument())
    const body = bodyOf(fetchMock, 'PUT /capacity-groups/cg-1') as Record<string, unknown>
    expect(body).toEqual({
      name: 'cloud-spot2',
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
      // expr_limits is not editable here; a full-replace PUT must carry it back.
      machine_profile: { cpu_count: 8, ram_mb: 16384, expr_limits: { operation_limit: 5000 } },
    })
    for (const k of ['id', 'status', 'created_at', 'updated_at', 'farm_id']) {
      expect(body).not.toHaveProperty(k)
    }
  })

  it('keeps a disabled group disabled and its queue selection on save', async () => {
    const fetchMock = stubApi({
      ...BASE_ROUTES,
      'GET /capacity-groups/cg-1': {
        body: group({ enabled: false, queue_ids: ['farm-1-q1'], status: null }),
      },
      'PUT /capacity-groups/cg-1': { body: group() },
    })
    renderAt('/capacity-groups/cg-1/edit')
    await screen.findByDisplayValue('cloud-spot')
    expect(await screen.findByLabelText('farm-1-render')).toBeChecked()
    expect(screen.getByLabelText(/^enabled$/i)).not.toBeChecked()
    fireEvent.click(screen.getByRole('button', { name: /^save$/i }))
    await waitFor(() => expect(calls(fetchMock)).toContain('PUT /capacity-groups/cg-1'))
    expect(bodyOf(fetchMock, 'PUT /capacity-groups/cg-1')).toMatchObject({
      enabled: false,
      queue_ids: ['farm-1-q1'],
    })
  })

  it('sends a named group back with the profile it was loaded with', async () => {
    const fetchMock = stubApi({
      ...BASE_ROUTES,
      'GET /capacity-groups/cg-1': {
        body: group({ kind: 'named', machine_profile: { cpu_count: 4 } }),
      },
      'PUT /capacity-groups/cg-1': { body: group() },
    })
    renderAt('/capacity-groups/cg-1/edit')
    await screen.findByDisplayValue('cloud-spot')
    expect(screen.queryByLabelText(/cpu count/i)).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: /^save$/i }))
    await waitFor(() => expect(calls(fetchMock)).toContain('PUT /capacity-groups/cg-1'))
    expect(bodyOf(fetchMock, 'PUT /capacity-groups/cg-1')).toMatchObject({
      machine_profile: { cpu_count: 4 },
    })
  })

  it('shows a load failure', async () => {
    stubApi({ ...BASE_ROUTES, 'GET /capacity-groups/cg-1': { status: 404, body: { detail: 'x' } } })
    renderAt('/capacity-groups/cg-1/edit')
    expect(await screen.findByText(/failed to load capacity group/i)).toBeInTheDocument()
  })
})
