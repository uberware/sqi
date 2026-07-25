// SPDX-License-Identifier: AGPL-3.0-or-later

import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router'
import PageHeader from '@/components/PageHeader'
import { useToast } from '@/components/Toast'
import { useGetComputeLocation } from '@/api/queries'
import { useCreateComputeLocation, useUpdateComputeLocation } from '@/api/mutations'
import type { ComputeLocationInput } from '@/api/mutations'
import styles from './ComputeLocationForm.module.css'

interface Props {
  mode: 'create' | 'edit'
}

interface Defaults {
  name: string
  description: string
}

interface InnerProps {
  mode: 'create' | 'edit'
  id: string
  defaults: Defaults
}

// NAME_PATTERN mirrors the server's openjd.ValidLocationName: a name must be
// referenceable via a loc:// URI, so it may not contain whitespace, "/", or
// quote characters.
const NAME_PATTERN = /^[^/\s"']+$/

function ComputeLocationFormInner({ mode, id, defaults }: InnerProps) {
  const navigate = useNavigate()
  const { showToast } = useToast()
  const createLocation = useCreateComputeLocation()
  const updateLocation = useUpdateComputeLocation()

  const [name, setName] = useState(defaults.name)
  const [description, setDescription] = useState(defaults.description)
  const [nameFocused, setNameFocused] = useState(false)

  const isPending = createLocation.isPending || updateLocation.isPending

  const trimmedName = name.trim()
  const nameValid = NAME_PATTERN.test(trimmedName)
  const nameInvalid = trimmedName !== '' && !nameValid
  const nameDescribedBy =
    [nameFocused ? 'cl-name-help' : null, nameInvalid ? 'cl-name-error' : null]
      .filter(Boolean)
      .join(' ') || undefined

  const canSubmit = nameValid && !isPending

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!canSubmit) return
    const trimmedDesc = description.trim()
    const input: ComputeLocationInput = {
      name: trimmedName,
      ...(trimmedDesc ? { description: trimmedDesc } : {}),
    }
    try {
      if (mode === 'create') {
        await createLocation.mutateAsync(input)
        showToast(`Compute location "${input.name}" created`, 'success')
      } else {
        await updateLocation.mutateAsync({ id, input })
        showToast(`Compute location "${input.name}" saved`, 'success')
      }
      navigate('/compute-locations')
    } catch (err) {
      showToast(err instanceof Error ? err.message : 'Save failed', 'error')
    }
  }

  return (
    <div className={styles.page}>
      <PageHeader
        title={mode === 'create' ? 'New Compute Location' : 'Edit Compute Location'}
        subtitle="A named group of workers sharing the same physical or logical site"
      />

      <form className={styles.form} onSubmit={(e) => void handleSubmit(e)} noValidate>
        <div className={styles.field}>
          <label htmlFor="cl-name" className={styles.label}>
            Name
          </label>
          <div className={styles.nameControl}>
            <input
              id="cl-name"
              className={styles.input}
              value={name}
              onChange={(e) => setName(e.target.value)}
              onFocus={() => setNameFocused(true)}
              onBlur={() => setNameFocused(false)}
              aria-describedby={nameDescribedBy}
              aria-invalid={nameInvalid || undefined}
              required
              aria-required="true"
            />
            {(nameFocused || nameInvalid) && (
              <div className={styles.nameHelp}>
                {nameFocused && (
                  <p id="cl-name-help" className={styles.hint}>
                    Referenced by workers as their compute location. Use letters, numbers, dashes,
                    dots, or underscores. No spaces, slashes, or quotes.
                  </p>
                )}
                {nameInvalid && (
                  <p id="cl-name-error" className={styles.hint} role="alert">
                    Name must not contain spaces, slashes, or quotes.
                  </p>
                )}
              </div>
            )}
          </div>
        </div>

        <div className={styles.field}>
          <label htmlFor="cl-description" className={styles.label}>
            Description (optional)
          </label>
          <input
            id="cl-description"
            className={styles.input}
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </div>

        <div className={styles.footer}>
          <button type="submit" className={styles.submitBtn} disabled={!canSubmit}>
            {mode === 'create' ? 'Create Compute Location' : 'Save'}
          </button>
          <Link to="/compute-locations" className={styles.cancelBtn}>
            Cancel
          </Link>
        </div>
      </form>
    </div>
  )
}

export default function ComputeLocationForm({ mode }: Props) {
  const params = useParams<{ id: string }>()
  const id = params.id ?? ''
  const { data, isLoading, isError } = useGetComputeLocation(mode === 'edit' ? id : '')

  if (mode === 'edit') {
    if (isLoading || !data) {
      return (
        <div className={styles.page}>
          {isError ? <p>Failed to load compute location.</p> : <p>Loading…</p>}
        </div>
      )
    }
    return (
      <ComputeLocationFormInner
        key={id}
        mode="edit"
        id={id}
        defaults={{
          name: data.name,
          description: data.description ?? '',
        }}
      />
    )
  }

  return <ComputeLocationFormInner mode="create" id="" defaults={{ name: '', description: '' }} />
}
