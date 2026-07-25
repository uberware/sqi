// SPDX-License-Identifier: AGPL-3.0-or-later

import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router'
import PageHeader from '@/components/PageHeader'
import { useToast } from '@/components/Toast'
import { useGetUsagePool } from '@/api/queries'
import { useCreateUsagePool, useUpdateUsagePool } from '@/api/mutations'
import type { UsagePoolInput } from '@/api/mutations'
import styles from './UsagePoolForm.module.css'

interface Props {
  mode: 'create' | 'edit'
}

interface Defaults {
  name: string
  serverHint: string
  maxConcurrent: string
}

interface InnerProps {
  mode: 'create' | 'edit'
  id: string
  defaults: Defaults
}

function UsagePoolFormInner({ mode, id, defaults }: InnerProps) {
  const navigate = useNavigate()
  const { showToast } = useToast()
  const createPool = useCreateUsagePool()
  const updatePool = useUpdateUsagePool()

  const [name, setName] = useState(defaults.name)
  const [serverHint, setServerHint] = useState(defaults.serverHint)
  const [maxConcurrent, setMaxConcurrent] = useState(defaults.maxConcurrent)

  const isPending = createPool.isPending || updatePool.isPending
  const maxConcurrentValue = Math.trunc(Number(maxConcurrent) || 0)
  const canSubmit = name.trim().length > 0 && maxConcurrentValue >= 1 && !isPending

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!canSubmit) return
    const trimmedHint = serverHint.trim()
    const input: UsagePoolInput = {
      name: name.trim(),
      max_concurrent: maxConcurrentValue,
      ...(trimmedHint ? { server_hint: trimmedHint } : {}),
    }
    try {
      if (mode === 'create') {
        await createPool.mutateAsync(input)
        showToast(`Usage pool "${input.name}" created`, 'success')
      } else {
        await updatePool.mutateAsync({ id, input })
        showToast(`Usage pool "${input.name}" saved`, 'success')
      }
      navigate('/usage-pools')
    } catch (err) {
      showToast(err instanceof Error ? err.message : 'Save failed', 'error')
    }
  }

  return (
    <div className={styles.page}>
      <PageHeader
        title={mode === 'create' ? 'New Usage Pool' : 'Edit Usage Pool'}
        subtitle="sqi caps concurrent tasks at the pool's seat count across the whole farm"
      />

      <form className={styles.form} onSubmit={(e) => void handleSubmit(e)} noValidate>
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
          <label htmlFor="serverHint" className={styles.label}>
            License server (optional)
          </label>
          <input
            id="serverHint"
            className={styles.input}
            value={serverHint}
            placeholder="e.g. 27000@licsrv.studio.internal"
            onChange={(e) => setServerHint(e.target.value)}
          />
        </div>

        <div className={styles.field}>
          <label htmlFor="maxConcurrent" className={styles.label}>
            Max concurrent (seats)
          </label>
          <input
            id="maxConcurrent"
            type="number"
            min={1}
            className={styles.input}
            value={maxConcurrent}
            onChange={(e) => setMaxConcurrent(e.target.value)}
            required
            aria-required="true"
          />
        </div>

        <div className={styles.footer}>
          <button type="submit" className={styles.submitBtn} disabled={!canSubmit}>
            {mode === 'create' ? 'Create Usage Pool' : 'Save'}
          </button>
          <Link to="/usage-pools" className={styles.cancelBtn}>
            Cancel
          </Link>
        </div>
      </form>
    </div>
  )
}

export default function UsagePoolForm({ mode }: Props) {
  const params = useParams<{ id: string }>()
  const id = params.id ?? ''
  const { data, isLoading, isError } = useGetUsagePool(mode === 'edit' ? id : '')

  if (mode === 'edit') {
    if (isLoading || !data) {
      return (
        <div className={styles.page}>
          {isError ? <p>Failed to load usage pool.</p> : <p>Loading…</p>}
        </div>
      )
    }
    return (
      <UsagePoolFormInner
        key={id}
        mode="edit"
        id={id}
        defaults={{
          name: data.name,
          serverHint: data.server_hint ?? '',
          maxConcurrent: String(data.max_concurrent),
        }}
      />
    )
  }

  return (
    <UsagePoolFormInner
      mode="create"
      id=""
      defaults={{ name: '', serverHint: '', maxConcurrent: '1' }}
    />
  )
}
