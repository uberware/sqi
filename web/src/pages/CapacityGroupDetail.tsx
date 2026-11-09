// SPDX-License-Identifier: AGPL-3.0-or-later

import { useState } from 'react'
import type { ReactNode } from 'react'
import { Link, useNavigate, useParams } from 'react-router'
import PageHeader from '@/components/PageHeader'
import ErrorBanner from '@/components/ErrorBanner'
import IconButton from '@/components/IconButton'
import CopyButton from '@/components/CopyButton'
import RelativeTime from '@/components/RelativeTime'
import StatusBadge from '@/components/StatusBadge'
import { Trash } from '@/components/icons'
import { useToast } from '@/components/Toast'
import {
  useCapacityGroupMembers,
  useCapacityGroupTokens,
  useGetCapacityGroup,
  useListWorkers,
} from '@/api/queries'
import {
  useAddCapacityGroupMember,
  useCreateCapacityGroupToken,
  useDeleteCapacityGroup,
  useRemoveCapacityGroupMember,
  useRevokeCapacityGroupToken,
} from '@/api/mutations'
import { ApiError } from '@/api/client'
import { useAuth } from '@/auth/context'
import { can } from '@/auth/policy'
import type {
  CapacityGroup,
  CapacityGroupAction,
  CapacityGroupDegraded,
  CapacityGroupStatus,
  CapacityGroupTokenCreated,
  PowerState,
  Worker,
} from '@/api/types'
import { EXTERNAL_PROVIDER, WAKE_ON_LAN_PROVIDER } from '@/lib/capacityGroupForm'
import styles from './CapacityGroupDetail.module.css'

const DEFAULT_TOKEN_TTL = '720h'

const workerName = (w: Worker): string => w.name || w.hostname

const POWER_LABELS: Record<PowerState, string> = {
  awake: 'Awake',
  waking: 'Waking',
  retiring: 'Retiring',
  asleep: 'Asleep',
  wake_failed: 'Wake failed',
}

/** A worker's power state as a text label; the colour only reinforces it. */
function PowerBadge({ state }: { state: PowerState }) {
  return <span className={`${styles.power} ${styles[state] ?? ''}`}>{POWER_LABELS[state]}</span>
}

/** True when the worker reported a network interface a wake-on-LAN packet can be addressed with. */
const hasMac = (w: Worker): boolean => w.interfaces.some((i) => i.mac !== '')

/** A grid of label/value pairs. `live` announces changed values to screen readers. */
function StatGrid({ rows, live }: { rows: Array<[string, ReactNode]>; live?: boolean }) {
  return (
    <dl className={styles.statGrid} aria-live={live ? 'polite' : undefined}>
      {rows.map(([k, v]) => (
        <div key={k} className={styles.stat}>
          <dt>{k}</dt>
          <dd>{v}</dd>
        </div>
      ))}
    </dl>
  )
}

// ── Status ────────────────────────────────────────────────────────────────────

/** The group has stopped growing: a polite status, so a poll that finds it is announced. */
function DegradedBanner({ degraded }: { degraded: CapacityGroupDegraded }) {
  return (
    <div className={styles.degraded} role="status" aria-label="Degraded">
      <p className={styles.degradedTitle}>Degraded: {degraded.reason}</p>
      <p className={styles.degradedLine}>
        Since <RelativeTime timestamp={degraded.since} />. No more machines are requested until{' '}
        <RelativeTime timestamp={degraded.backoff_until} />.
      </p>
    </div>
  )
}

function LastAction({ action }: { action: CapacityGroupAction | null }) {
  return (
    <>
      <h3 id="cg-last-action" className={styles.subTitle}>
        Last provider action
      </h3>
      {action === null ? (
        <p className={styles.muted}>No provider action yet.</p>
      ) : (
        <div role="group" aria-labelledby="cg-last-action">
          <StatGrid
            rows={[
              ['Action', action.action],
              ['Result', action.result],
              ['Message', action.message === '' ? '—' : action.message],
              ['When', <RelativeTime key="at" timestamp={action.at} />],
            ]}
          />
        </div>
      )}
    </>
  )
}

function StatusPanel({ group }: { group: CapacityGroup }) {
  const s: CapacityGroupStatus | null = group.status
  return (
    <section className={styles.panel} aria-labelledby="cg-status">
      <h2 id="cg-status" className={styles.sectionTitle}>
        Status
      </h2>
      {s === null ? (
        <p className={styles.muted}>
          {group.enabled
            ? 'Not evaluated yet. The scheduler evaluates a group on its next sweep.'
            : 'This group is disabled, so the scheduler does not evaluate it.'}
        </p>
      ) : (
        <>
          {s.degraded !== null && <DegradedBanner degraded={s.degraded} />}
          <StatGrid
            live
            rows={[
              ['Demand (cores)', s.demand_cores],
              ['Desired machines', s.desired_machines],
              ['Online members', s.online_members],
              ['Offline members', s.offline_members],
              ['Waking', s.waking],
              ['Retiring', s.retiring],
              ['Asleep', s.asleep],
              ['Wake failed', s.wake_failed],
              ...(s.requested_machines !== null
                ? ([['Requested machines', s.requested_machines]] as Array<[string, ReactNode]>)
                : []),
              ['Evaluated', <RelativeTime key="evaluated" timestamp={s.evaluated_at} />],
            ]}
          />
          <p className={styles.help}>
            Desired is the total number of machines the group should have, including machines marked
            for retirement. It is a target to set capacity to, not an amount to add.
          </p>

          {(group.provider !== EXTERNAL_PROVIDER || s.last_action !== null) && (
            <LastAction action={s.last_action} />
          )}

          <h3 className={styles.subTitle}>Retire candidates</h3>
          {s.retire_candidates.length === 0 ? (
            <p className={styles.muted}>
              None. Machines idle past the idle timeout are listed here.
            </p>
          ) : (
            <ul className={styles.plainList}>
              {s.retire_candidates.map((wid) => (
                <li key={wid}>
                  <Link to={`/workers/${wid}`} className={styles.mono}>
                    {wid}
                  </Link>
                </li>
              ))}
            </ul>
          )}

          <h3 className={styles.subTitle}>Profile mismatches</h3>
          {s.mismatches.length === 0 ? (
            <p className={styles.muted}>No online member falls short of the machine profile.</p>
          ) : (
            <table className={styles.table} aria-label="Profile mismatches">
              <thead>
                <tr>
                  <th>Worker</th>
                  <th>Reason</th>
                </tr>
              </thead>
              <tbody>
                {s.mismatches.map((m) => (
                  <tr key={m.worker_id}>
                    <td>
                      <Link to={`/workers/${m.worker_id}`} className={styles.mono}>
                        {m.worker_id}
                      </Link>
                    </td>
                    <td>
                      <span>{m.reason}</span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </>
      )}
    </section>
  )
}

// ── Settings ──────────────────────────────────────────────────────────────────

function profileSummary(g: CapacityGroup): string {
  const p = g.machine_profile
  const parts = [
    p.cpu_count !== undefined ? `${p.cpu_count} CPU` : '',
    p.ram_mb !== undefined ? `${p.ram_mb} MB RAM` : '',
    p.gpu_count !== undefined ? `${p.gpu_count} GPU` : '',
    p.os ?? '',
    p.arch ?? '',
    p.compute_location ?? '',
  ].filter(Boolean)
  return parts.length > 0 ? parts.join(' · ') : 'Learned from members'
}

function SettingsPanel({ group }: { group: CapacityGroup }) {
  const rows: Array<[string, string]> = [
    ['Provider', group.provider],
    ['Enabled', group.enabled ? 'yes' : 'no'],
    [
      'Queues',
      group.queue_ids.length === 0
        ? 'every queue in the farm'
        : `${group.queue_ids.length} selected`,
    ],
    ['Machines', `${group.min_machines} min / ${group.max_machines} max`],
    ['Max step up', String(group.max_step_up)],
    ['Scale-up cooldown', `${group.scale_up_cooldown_seconds} s`],
    ['Idle timeout', `${group.idle_timeout_seconds} s`],
    ['Boot timeout', `${group.boot_timeout_seconds} s`],
    ...(group.provider === WAKE_ON_LAN_PROVIDER
      ? ([['Retire action', group.retire_action]] as Array<[string, string]>)
      : []),
    ['Machine profile', profileSummary(group)],
  ]
  return (
    <section className={styles.panel} aria-labelledby="cg-settings">
      <h2 id="cg-settings" className={styles.sectionTitle}>
        Settings
      </h2>
      <StatGrid rows={rows} />
    </section>
  )
}

// ── Members ───────────────────────────────────────────────────────────────────

function AddMember({ group, members }: { group: CapacityGroup; members: Worker[] }) {
  const { showToast } = useToast()
  const addMember = useAddCapacityGroupMember(group.id)
  const { data } = useListWorkers(
    { farm_id: group.farm_id, sort_by: 'hostname', sort_dir: 'asc', limit: 1000 },
    { poll: false },
  )
  const [pick, setPick] = useState('')
  const memberIds = new Set(members.map((m) => m.id))
  const candidates = (data?.items ?? []).filter((w) => !memberIds.has(w.id))

  async function add() {
    const w = candidates.find((c) => c.id === pick)
    if (w === undefined) return
    try {
      await addMember.mutateAsync(w.id)
      showToast(`${workerName(w)} added to "${group.name}"`, 'success')
      setPick('')
    } catch (e) {
      showToast(e instanceof Error ? e.message : 'Failed to add worker', 'error')
    }
  }

  return (
    <div className={styles.inlineForm}>
      <label htmlFor="cg-add-worker" className={styles.label}>
        Worker to add
      </label>
      <select
        id="cg-add-worker"
        className={styles.select}
        value={pick}
        onChange={(e) => setPick(e.target.value)}
      >
        <option value="">Choose a worker…</option>
        {candidates.map((w) => (
          <option key={w.id} value={w.id}>
            {workerName(w)}
            {w.status === 'online' ? '' : ` (${w.status})`}
          </option>
        ))}
      </select>
      <button
        type="button"
        className={styles.primaryBtn}
        disabled={pick === '' || addMember.isPending}
        onClick={() => void add()}
      >
        Add worker
      </button>
    </div>
  )
}

function MembersSection({ group, canManage }: { group: CapacityGroup; canManage: boolean }) {
  const { showToast } = useToast()
  const { data, isLoading, isError, error } = useCapacityGroupMembers(group.id)
  const removeMember = useRemoveCapacityGroupMember(group.id)
  const [removing, setRemoving] = useState<string | null>(null)
  const members = data ?? []
  const named = group.kind === 'named'
  const editable = named && canManage
  const columns = editable ? 5 : 4
  const wol = group.provider === WAKE_ON_LAN_PROVIDER

  async function remove(w: Worker) {
    setRemoving(w.id)
    try {
      await removeMember.mutateAsync(w.id)
      showToast(`${workerName(w)} removed from "${group.name}"`, 'success')
    } catch (e) {
      showToast(e instanceof Error ? e.message : 'Failed to remove worker', 'error')
    } finally {
      setRemoving(null)
    }
  }

  return (
    <section className={styles.panel} aria-labelledby="cg-members">
      <h2 id="cg-members" className={styles.sectionTitle}>
        Members
      </h2>
      {!named && (
        <p className={styles.help}>
          Members of a sized group join with a group token or by setting{' '}
          <code>worker.capacity_group</code>; they cannot be added by name.
        </p>
      )}
      {isError && (
        <ErrorBanner>
          Failed to load members: {error instanceof Error ? error.message : 'Unknown error'}
        </ErrorBanner>
      )}
      <table className={styles.table} aria-label="Members">
        <thead>
          <tr>
            <th>Worker</th>
            <th>Status</th>
            <th>Power</th>
            <th>Instance</th>
            {editable && <th aria-label="Actions" />}
          </tr>
        </thead>
        <tbody>
          {isLoading && (
            <tr>
              <td colSpan={columns} className={styles.muted}>
                Loading…
              </td>
            </tr>
          )}
          {!isLoading && members.length === 0 && (
            <tr>
              <td colSpan={columns} className={styles.muted}>
                No members yet.
              </td>
            </tr>
          )}
          {members.map((w) => (
            <tr key={w.id}>
              <td>
                <Link to={`/workers/${w.id}`} className={styles.link}>
                  {workerName(w)}
                </Link>
                {wol && !hasMac(w) && <p className={styles.cellNote}>no wake-on-LAN address yet</p>}
              </td>
              <td>
                <StatusBadge status={w.status} />
              </td>
              <td>
                <PowerBadge state={w.power_state} />
                {w.power_state_at !== undefined && w.power_state !== 'awake' && (
                  <>
                    {' '}
                    <span className={styles.cellNote}>
                      since <RelativeTime timestamp={w.power_state_at} />
                    </span>
                  </>
                )}
                {w.power_reason !== undefined && w.power_reason !== '' && (
                  <p className={styles.cellNote}>{w.power_reason}</p>
                )}
              </td>
              <td className={styles.mono}>{w.cloud_instance_id ?? '—'}</td>
              {editable && (
                <td>
                  <IconButton
                    icon={<Trash />}
                    className={styles.rowBtn}
                    busy={removing === w.id}
                    onClick={() => void remove(w)}
                    title="Remove from group"
                    label={`Remove ${workerName(w)} from group`}
                  />
                </td>
              )}
            </tr>
          ))}
        </tbody>
      </table>
      {editable && <AddMember group={group} members={members} />}
    </section>
  )
}

// ── Join tokens ───────────────────────────────────────────────────────────────

/** Why group tokens do not apply on this farm. */
type TokensUnavailable = 'rest-auth-off' | 'broker-auth-off'

/**
 * The server refuses to mint a group token when REST authentication is off
 * (the route is not mounted: 405) or when the broker does not authenticate
 * workers (409 naming worker.capacity_group). Both mean group tokens do not
 * apply on this farm. Any other error returns null.
 */
function tokensUnavailable(err: unknown): TokensUnavailable | null {
  if (!(err instanceof ApiError)) return null
  if (err.status === 405) return 'rest-auth-off'
  if (err.status === 409 && err.detail.includes('worker.capacity_group')) return 'broker-auth-off'
  return null
}

function NewTokenCallout({
  created,
  onDone,
}: {
  created: CapacityGroupTokenCreated
  onDone: () => void
}) {
  return (
    <div className={styles.secretCallout} role="alert">
      <p className={styles.secretWarning}>Copy this token now. It will not be shown again.</p>
      <div className={styles.secretRow}>
        <label htmlFor="cg-new-token" className={styles.label}>
          New join token
        </label>
        <input
          id="cg-new-token"
          className={styles.secretInput}
          readOnly
          value={created.token}
          onFocus={(e) => e.currentTarget.select()}
        />
        <CopyButton value={created.token} display="token" />
      </div>
      <button type="button" className={styles.secondaryBtn} onClick={onDone}>
        Done
      </button>
    </div>
  )
}

function TokensSection({ group }: { group: CapacityGroup }) {
  const { showToast } = useToast()
  const { data, isLoading, isError, error } = useCapacityGroupTokens(group.id)
  const createToken = useCreateCapacityGroupToken(group.id)
  const revokeToken = useRevokeCapacityGroupToken(group.id)

  const [name, setName] = useState('')
  const [expiresIn, setExpiresIn] = useState(DEFAULT_TOKEN_TTL)
  // The raw token is held only in this component's state, never in the query
  // cache or storage, so it is gone when the person leaves the page.
  const [created, setCreated] = useState<CapacityGroupTokenCreated | null>(null)
  const [unavailable, setUnavailable] = useState<TokensUnavailable | null>(null)
  const [createError, setCreateError] = useState<string | null>(null)
  const tokens = data ?? []

  async function create(e: React.FormEvent) {
    e.preventDefault()
    setCreateError(null)
    const trimmedName = name.trim()
    const trimmedTtl = expiresIn.trim()
    try {
      const result = await createToken.mutateAsync({
        ...(trimmedName !== '' ? { name: trimmedName } : {}),
        ...(trimmedTtl !== '' ? { expires_in: trimmedTtl } : {}),
      })
      setCreated(result)
      setName('')
    } catch (err) {
      const reason = tokensUnavailable(err)
      if (reason !== null) setUnavailable(reason)
      else setCreateError(err instanceof Error ? err.message : 'Failed to create token')
    }
  }

  function dismiss() {
    setCreated(null)
    // Drop the mutation's own copy of the result too.
    createToken.reset()
  }

  async function revoke(id: string, label: string) {
    if (!window.confirm(`Revoke token "${label}"? Workers can no longer enroll with it.`)) return
    try {
      await revokeToken.mutateAsync(id)
      showToast(`Token "${label}" revoked`, 'success')
    } catch (err) {
      showToast(err instanceof Error ? err.message : 'Failed to revoke token', 'error')
    }
  }

  return (
    <section className={styles.panel} aria-labelledby="cg-tokens">
      <h2 id="cg-tokens" className={styles.sectionTitle}>
        Join tokens
      </h2>
      <p className={styles.help}>
        A worker enrolls with a token to join this group. Revoking a token does not revoke
        credentials already issued from it: workers that enrolled keep their credentials and stay
        members. To retire one, stop it and remove the worker, which also revokes its credential.
      </p>

      {created !== null && <NewTokenCallout created={created} onDone={dismiss} />}

      {unavailable === 'broker-auth-off' && (
        <p className={styles.note} role="note">
          Join tokens are not available on this server because it does not authenticate workers.
          Workers join this group by setting <code>worker.capacity_group: {group.name}</code> (or{' '}
          <code>SQI_WORKER_CAPACITY_GROUP={group.name}</code>) in their configuration.
        </p>
      )}
      {unavailable === 'rest-auth-off' && (
        <p className={styles.note} role="note">
          Join tokens are not available on this server because REST authentication is off. If the
          broker does not authenticate workers either, workers join this group by setting{' '}
          <code>worker.capacity_group: {group.name}</code> in their configuration. If it does, no
          machine can join a sized group until REST authentication is turned on.
        </p>
      )}
      {unavailable === null && (
        <form className={styles.inlineForm} onSubmit={(e) => void create(e)}>
          <label htmlFor="cg-token-name" className={styles.label}>
            Token name (optional)
          </label>
          <input
            id="cg-token-name"
            className={styles.input}
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
          <label htmlFor="cg-token-ttl" className={styles.label}>
            Expires in
          </label>
          <input
            id="cg-token-ttl"
            className={styles.input}
            value={expiresIn}
            onChange={(e) => setExpiresIn(e.target.value)}
            aria-describedby="cg-token-ttl-hint"
          />
          <button type="submit" className={styles.primaryBtn} disabled={createToken.isPending}>
            Create token
          </button>
          <p id="cg-token-ttl-hint" className={styles.hint}>
            A duration such as 720h (30 days, the default). At most 2160h (90 days).
          </p>
        </form>
      )}
      {createError !== null && (
        <p className={styles.error} role="alert">
          {createError}
        </p>
      )}

      {isError && (
        <ErrorBanner>
          Failed to load tokens: {error instanceof Error ? error.message : 'Unknown error'}
        </ErrorBanner>
      )}
      <table className={styles.table} aria-label="Join tokens">
        <thead>
          <tr>
            <th>Name</th>
            <th>Prefix</th>
            <th>Expires</th>
            <th>Revoked</th>
            <th>Created by</th>
            <th aria-label="Actions" />
          </tr>
        </thead>
        <tbody>
          {isLoading && (
            <tr>
              <td colSpan={6} className={styles.muted}>
                Loading…
              </td>
            </tr>
          )}
          {!isLoading && tokens.length === 0 && (
            <tr>
              <td colSpan={6} className={styles.muted}>
                No tokens yet.
              </td>
            </tr>
          )}
          {tokens.map((t) => {
            const label = t.name || t.prefix
            return (
              <tr key={t.id}>
                <td>{t.name || '—'}</td>
                <td className={styles.mono}>{t.prefix}</td>
                <td>
                  <RelativeTime timestamp={t.expires_at} />
                </td>
                <td>{t.revoked_at !== null ? <span>Revoked</span> : '—'}</td>
                <td>{t.created_by}</td>
                <td>
                  {t.revoked_at === null && (
                    <IconButton
                      icon={<Trash />}
                      className={styles.rowBtn}
                      onClick={() => void revoke(t.id, label)}
                      title="Revoke"
                      label={`Revoke token ${label}`}
                    />
                  )}
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </section>
  )
}

// ── Page ──────────────────────────────────────────────────────────────────────

export default function CapacityGroupDetail() {
  const params = useParams<{ id: string }>()
  const id = params.id ?? ''
  const navigate = useNavigate()
  const { principal } = useAuth()
  const canManage = can(principal, 'infra.manage')
  const { showToast } = useToast()
  const { data: group, isLoading, isError, error } = useGetCapacityGroup(id)
  const deleteGroup = useDeleteCapacityGroup()

  async function handleDelete(g: CapacityGroup) {
    if (!window.confirm(`Delete capacity group "${g.name}"? This cannot be undone.`)) return
    try {
      await deleteGroup.mutateAsync(g.id)
      showToast(`Capacity group "${g.name}" deleted`, 'success')
      navigate('/capacity-groups')
    } catch (e) {
      showToast(e instanceof Error ? e.message : 'Failed to delete capacity group', 'error')
    }
  }

  if (isError) {
    return (
      <div className={styles.page}>
        <ErrorBanner>
          Failed to load capacity group: {error instanceof Error ? error.message : 'Unknown error'}
        </ErrorBanner>
      </div>
    )
  }
  if (isLoading || !group) {
    return (
      <div className={styles.page}>
        <p className={styles.muted}>Loading…</p>
      </div>
    )
  }

  return (
    <div className={styles.page}>
      <PageHeader
        title={group.name}
        subtitle={`${group.kind} group · ${group.provider}${group.enabled ? '' : ' · disabled'}`}
        backTo="/capacity-groups"
        backLabel="Capacity Groups"
        action={
          canManage ? (
            <span className={styles.actions}>
              <Link to={`/capacity-groups/${group.id}/edit`} className={styles.primaryBtn}>
                Edit
              </Link>
              <IconButton
                icon={<Trash />}
                className={styles.rowBtn}
                onClick={() => void handleDelete(group)}
                title="Delete"
                label={`Delete capacity group ${group.name}`}
              />
            </span>
          ) : undefined
        }
      />
      <div className={styles.body}>
        <StatusPanel group={group} />
        <MembersSection group={group} canManage={canManage} />
        {group.kind === 'sized' && canManage && <TokensSection key={group.id} group={group} />}
        <SettingsPanel group={group} />
      </div>
    </div>
  )
}
