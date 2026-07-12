// SPDX-License-Identifier: AGPL-3.0-or-later

import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import type { Farm } from '@/api/types'
import PageHeader from '@/components/PageHeader'
import RetryPolicyFields from '@/components/RetryPolicyFields'
import { useToast } from '@/components/Toast'
import { useGetQueue, useListFarms } from '@/api/queries'
import { parseOptionalInt } from '@/lib/parse'
import { useCreateQueue, useUpdateQueue } from '@/api/mutations'
import type { QueueInput } from '@/api/mutations'
import styles from './QueueForm.module.css'

interface Props {
  mode: 'create' | 'edit'
}

interface Defaults {
  farmId: string
  name: string
  description: string
  priority: string
  maxConcurrent: string
  paused: boolean
  maxAttempts: string
  retryDelaySeconds: string
  failureLimit: string
}

interface InnerProps {
  mode: 'create' | 'edit'
  id: string
  farms: Farm[]
  defaults: Defaults
}

function QueueFormInner({ mode, id, farms, defaults }: InnerProps) {
  const navigate = useNavigate()
  const { showToast } = useToast()
  const createQueue = useCreateQueue()
  const updateQueue = useUpdateQueue()

  const [farmId, setFarmId] = useState(defaults.farmId)
  const [name, setName] = useState(defaults.name)
  const [description, setDescription] = useState(defaults.description)
  const [priority, setPriority] = useState(defaults.priority)
  const [maxConcurrent, setMaxConcurrent] = useState(defaults.maxConcurrent)
  const [paused, setPaused] = useState(defaults.paused)
  const [maxAttempts, setMaxAttempts] = useState(defaults.maxAttempts)
  const [retryDelaySeconds, setRetryDelaySeconds] = useState(defaults.retryDelaySeconds)
  const [failureLimit, setFailureLimit] = useState(defaults.failureLimit)

  const isPending = createQueue.isPending || updateQueue.isPending
  const canSubmit = name.trim().length > 0 && farmId !== '' && !isPending

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!canSubmit) return
    const parsedMaxAttempts = parseOptionalInt(maxAttempts)
    const parsedRetryDelaySeconds = parseOptionalInt(retryDelaySeconds)
    const parsedFailureLimit = parseOptionalInt(failureLimit)
    const input: QueueInput = {
      farm_id: farmId,
      name: name.trim(),
      description: description.trim(),
      priority: Math.trunc(Number(priority) || 0),
      max_concurrent_tasks: Math.trunc(Number(maxConcurrent) || 0),
      paused,
      ...(parsedMaxAttempts !== undefined ? { max_attempts: parsedMaxAttempts } : {}),
      ...(parsedRetryDelaySeconds !== undefined
        ? { retry_delay_seconds: parsedRetryDelaySeconds }
        : {}),
      ...(parsedFailureLimit !== undefined ? { failure_limit: parsedFailureLimit } : {}),
    }
    try {
      if (mode === 'create') {
        await createQueue.mutateAsync(input)
        showToast(`Queue "${input.name}" created`, 'success')
      } else {
        await updateQueue.mutateAsync({ id, input })
        showToast(`Queue "${input.name}" saved`, 'success')
      }
      navigate('/queues')
    } catch (err) {
      showToast(err instanceof Error ? err.message : 'Save failed', 'error')
    }
  }

  return (
    <div className={styles.page}>
      <PageHeader
        title={mode === 'create' ? 'New Queue' : 'Edit Queue'}
        subtitle="Queue policy overrides its farm's defaults"
      />

      <form className={styles.form} onSubmit={(e) => void handleSubmit(e)} noValidate>
        <div className={styles.field}>
          <label htmlFor="farm" className={styles.label}>
            Farm
          </label>
          <select
            id="farm"
            className={styles.select}
            value={farmId}
            onChange={(e) => setFarmId(e.target.value)}
            required
            aria-required="true"
            disabled={mode === 'edit'}
          >
            {farms.map((farm) => (
              <option key={farm.id} value={farm.id}>
                {farm.name}
              </option>
            ))}
          </select>
        </div>

        <div className={styles.field}>
          <label htmlFor="name" className={styles.label}>
            Name
          </label>
          <input
            id="name"
            className={styles.input}
            value={name}
            onChange={(e) => setName(e.target.value)}
            required
            aria-required="true"
          />
        </div>

        <div className={styles.field}>
          <label htmlFor="description" className={styles.label}>
            Description
          </label>
          <textarea
            id="description"
            className={styles.textarea}
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            rows={3}
          />
        </div>

        <div className={styles.field}>
          <label htmlFor="priority" className={styles.label}>
            Priority
          </label>
          <input
            id="priority"
            type="number"
            className={styles.input}
            value={priority}
            onChange={(e) => setPriority(e.target.value)}
          />
        </div>

        <div className={styles.field}>
          <label htmlFor="maxConcurrent" className={styles.label}>
            Max concurrent tasks (0 = unlimited)
          </label>
          <input
            id="maxConcurrent"
            type="number"
            min={0}
            className={styles.input}
            value={maxConcurrent}
            onChange={(e) => setMaxConcurrent(e.target.value)}
          />
        </div>

        <div className={styles.field}>
          <label htmlFor="paused" className={styles.label}>
            <input
              id="paused"
              type="checkbox"
              checked={paused}
              onChange={(e) => setPaused(e.target.checked)}
            />{' '}
            Paused
          </label>
        </div>

        <RetryPolicyFields
          maxAttempts={maxAttempts}
          onMaxAttemptsChange={setMaxAttempts}
          retryDelaySeconds={retryDelaySeconds}
          onRetryDelaySecondsChange={setRetryDelaySeconds}
          failureLimit={failureLimit}
          onFailureLimitChange={setFailureLimit}
          placeholder="Inherit farm default"
          fieldClassName={styles.field}
          labelClassName={styles.label}
          inputClassName={styles.input}
        />

        <div className={styles.footer}>
          <button type="submit" className={styles.submitBtn} disabled={!canSubmit}>
            {mode === 'create' ? 'Create Queue' : 'Save'}
          </button>
          <Link to="/queues" className={styles.cancelBtn}>
            Cancel
          </Link>
        </div>
      </form>
    </div>
  )
}

export default function QueueForm({ mode }: Props) {
  const params = useParams<{ id: string }>()
  const id = params.id ?? ''

  const farmsQuery = useListFarms()
  const queueQuery = useGetQueue(mode === 'edit' ? id : '')

  const farmsReady = !farmsQuery.isLoading && farmsQuery.data !== undefined
  const queueReady = mode === 'create' || (!queueQuery.isLoading && queueQuery.data !== undefined)

  if (!farmsReady || !queueReady) {
    const isError = farmsQuery.isError || queueQuery.isError
    return (
      <div className={styles.page}>{isError ? <p>Failed to load data.</p> : <p>Loading…</p>}</div>
    )
  }

  const farms = farmsQuery.data ?? []
  const defaultFarmId = mode === 'edit' ? (queueQuery.data?.farm_id ?? '') : (farms[0]?.id ?? '')

  return (
    <QueueFormInner
      key={mode === 'edit' ? id : 'new'}
      mode={mode}
      id={id}
      farms={farms}
      defaults={{
        farmId: defaultFarmId,
        name: queueQuery.data?.name ?? '',
        description: queueQuery.data?.description ?? '',
        priority: String(queueQuery.data?.priority ?? 50),
        maxConcurrent: String(queueQuery.data?.max_concurrent_tasks ?? 0),
        paused: queueQuery.data?.paused ?? false,
        maxAttempts:
          queueQuery.data?.max_attempts !== undefined ? String(queueQuery.data.max_attempts) : '',
        retryDelaySeconds:
          queueQuery.data?.retry_delay_seconds !== undefined
            ? String(queueQuery.data.retry_delay_seconds)
            : '',
        failureLimit:
          queueQuery.data?.failure_limit !== undefined ? String(queueQuery.data.failure_limit) : '',
      }}
    />
  )
}
