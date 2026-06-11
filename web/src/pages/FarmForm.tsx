// SPDX-License-Identifier: AGPL-3.0-or-later

import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import PageHeader from '@/components/PageHeader'
import { useToast } from '@/components/Toast'
import { useGetFarm } from '@/api/queries'
import { useCreateFarm, useUpdateFarm } from '@/api/mutations'
import type { FarmInput } from '@/api/mutations'
import styles from './FarmForm.module.css'

interface Props {
  mode: 'create' | 'edit'
}

interface Defaults {
  name: string
  description: string
  maxConcurrent: string
}

interface InnerProps {
  mode: 'create' | 'edit'
  id: string
  defaults: Defaults
}

function FarmFormInner({ mode, id, defaults }: InnerProps) {
  const navigate = useNavigate()
  const { showToast } = useToast()
  const createFarm = useCreateFarm()
  const updateFarm = useUpdateFarm()

  const [name, setName] = useState(defaults.name)
  const [description, setDescription] = useState(defaults.description)
  const [maxConcurrent, setMaxConcurrent] = useState(defaults.maxConcurrent)

  const isPending = createFarm.isPending || updateFarm.isPending
  const canSubmit = name.trim().length > 0 && !isPending

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!canSubmit) return
    const input: FarmInput = {
      name: name.trim(),
      description: description.trim(),
      max_concurrent_tasks: Math.trunc(Number(maxConcurrent) || 0),
    }
    try {
      if (mode === 'create') {
        await createFarm.mutateAsync(input)
        showToast(`Farm "${input.name}" created`, 'success')
      } else {
        await updateFarm.mutateAsync({ id, input })
        showToast(`Farm "${input.name}" saved`, 'success')
      }
      navigate('/farms')
    } catch (err) {
      showToast(err instanceof Error ? err.message : 'Save failed', 'error')
    }
  }

  return (
    <div className={styles.page}>
      <PageHeader
        title={mode === 'create' ? 'New Farm' : 'Edit Farm'}
        subtitle="Farm-level defaults are inherited by its queues and workers"
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

        <div className={styles.footer}>
          <button type="submit" className={styles.submitBtn} disabled={!canSubmit}>
            {mode === 'create' ? 'Create Farm' : 'Save'}
          </button>
          <Link to="/farms" className={styles.cancelBtn}>
            Cancel
          </Link>
        </div>
      </form>
    </div>
  )
}

export default function FarmForm({ mode }: Props) {
  const params = useParams<{ id: string }>()
  const id = params.id ?? ''
  const { data, isLoading, isError } = useGetFarm(mode === 'edit' ? id : '')

  if (mode === 'edit') {
    if (isLoading || !data) {
      return (
        <div className={styles.page}>{isError ? <p>Failed to load farm.</p> : <p>Loading…</p>}</div>
      )
    }
    return (
      <FarmFormInner
        key={id}
        mode="edit"
        id={id}
        defaults={{
          name: data.name,
          description: data.description ?? '',
          maxConcurrent: String(data.max_concurrent_tasks),
        }}
      />
    )
  }

  return (
    <FarmFormInner
      mode="create"
      id=""
      defaults={{ name: '', description: '', maxConcurrent: '0' }}
    />
  )
}
