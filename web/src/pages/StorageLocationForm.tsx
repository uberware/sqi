// SPDX-License-Identifier: AGPL-3.0-or-later

import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router'
import PageHeader from '@/components/PageHeader'
import { useToast } from '@/components/Toast'
import { useGetStorageLocation } from '@/api/queries'
import { useCreateStorageLocation, useUpdateStorageLocation } from '@/api/mutations'
import type { StorageLocationInput } from '@/api/mutations'
import styles from './StorageLocationForm.module.css'

interface Props {
  mode: 'create' | 'edit'
}

interface RootRow {
  rowId: string
  key: string
  value: string
}

interface Defaults {
  name: string
  description: string
  roots: RootRow[]
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

// rowId only needs to be unique within a single component's rows array; an
// ever-incrementing module-level counter is sufficient (no reset needed).
let rowSeq = 0
function newRow(key = '', value = ''): RootRow {
  rowSeq += 1
  return { rowId: `r${rowSeq}`, key, value }
}

function rootsToRows(roots: Record<string, string> | undefined): RootRow[] {
  const entries = Object.entries(roots ?? {})
  if (entries.length === 0) return [newRow('default', '')]
  return entries.map(([k, v]) => newRow(k, v))
}

function StorageLocationFormInner({ mode, id, defaults }: InnerProps) {
  const navigate = useNavigate()
  const { showToast } = useToast()
  const createLocation = useCreateStorageLocation()
  const updateLocation = useUpdateStorageLocation()

  const [name, setName] = useState(defaults.name)
  const [description, setDescription] = useState(defaults.description)
  const [rows, setRows] = useState<RootRow[]>(defaults.roots)
  const [nameFocused, setNameFocused] = useState(false)

  const isPending = createLocation.isPending || updateLocation.isPending

  const trimmedName = name.trim()
  const nameValid = NAME_PATTERN.test(trimmedName)
  const nameInvalid = trimmedName !== '' && !nameValid
  const nameDescribedBy =
    [nameFocused ? 'sl-name-help' : null, nameInvalid ? 'sl-name-error' : null]
      .filter(Boolean)
      .join(' ') || undefined

  const trimmedRows = rows.map((r) => ({ ...r, key: r.key.trim(), value: r.value.trim() }))
  const nonEmptyKeys = trimmedRows.filter((r) => r.key !== '').map((r) => r.key)
  const hasDuplicateKey = new Set(nonEmptyKeys).size !== nonEmptyKeys.length
  const hasEmptyKeyWithValue = trimmedRows.some((r) => r.key === '' && r.value !== '')
  const hasAtLeastOnePath = trimmedRows.some((r) => r.value !== '')
  const hasDefault = trimmedRows.some((r) => r.key === 'default' && r.value !== '')

  const canSubmit =
    nameValid && hasAtLeastOnePath && !hasDuplicateKey && !hasEmptyKeyWithValue && !isPending

  function updateRow(rowId: string, patch: Partial<RootRow>) {
    setRows((rs) => rs.map((r) => (r.rowId === rowId ? { ...r, ...patch } : r)))
  }

  function removeRow(rowId: string) {
    setRows((rs) => rs.filter((r) => r.rowId !== rowId))
  }

  function addRow() {
    setRows((rs) => [...rs, newRow()])
  }

  function serializeRoots(): Record<string, string> {
    const out: Record<string, string> = {}
    for (const r of trimmedRows) {
      if (r.value === '') continue // skip rows with no path (includes fully-blank rows)
      out[r.key] = r.value
    }
    return out
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!canSubmit) return
    const trimmedDesc = description.trim()
    const input: StorageLocationInput = {
      name: name.trim(),
      roots: serializeRoots(),
      ...(trimmedDesc ? { description: trimmedDesc } : {}),
    }
    try {
      if (mode === 'create') {
        await createLocation.mutateAsync(input)
        showToast(`Storage location "${input.name}" created`, 'success')
      } else {
        await updateLocation.mutateAsync({ id, input })
        showToast(`Storage location "${input.name}" saved`, 'success')
      }
      navigate('/storage-locations')
    } catch (err) {
      showToast(err instanceof Error ? err.message : 'Save failed', 'error')
    }
  }

  return (
    <div className={styles.page}>
      <PageHeader
        title={mode === 'create' ? 'New Storage Location' : 'Edit Storage Location'}
        subtitle="A named storage root that jobs reference with loc:// so each worker resolves its own real path"
      />

      <form className={styles.form} onSubmit={(e) => void handleSubmit(e)} noValidate>
        <div className={styles.field}>
          <label htmlFor="sl-name" className={styles.label}>
            Name
          </label>
          <div className={styles.nameControl}>
            <input
              id="sl-name"
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
                  <p id="sl-name-help" className={styles.hint}>
                    Referenced in jobs as loc://&lt;name&gt;/path. Use letters, numbers, dashes,
                    dots, or underscores. No spaces, slashes, or quotes.
                  </p>
                )}
                {nameInvalid && (
                  <p id="sl-name-error" className={styles.hint} role="alert">
                    Name must not contain spaces, slashes, or quotes.
                  </p>
                )}
              </div>
            )}
          </div>
        </div>

        <div className={styles.field}>
          <label htmlFor="sl-description" className={styles.label}>
            Description (optional)
          </label>
          <input
            id="sl-description"
            className={styles.input}
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </div>

        <fieldset className={styles.field}>
          <legend className={styles.label}>Roots</legend>
          <div className={styles.rootsTable}>
            {rows.map((row, i) => (
              <div key={row.rowId} className={styles.rootRow}>
                <input
                  className={styles.input}
                  value={row.key}
                  aria-label={`Location key ${i + 1}`}
                  placeholder="default"
                  onChange={(e) => updateRow(row.rowId, { key: e.target.value })}
                />
                <input
                  className={styles.input}
                  value={row.value}
                  aria-label={`Root path ${i + 1}`}
                  placeholder="/mnt/nas/shows"
                  onChange={(e) => updateRow(row.rowId, { value: e.target.value })}
                />
                <button
                  type="button"
                  className={styles.cancelBtn}
                  aria-label={`Remove root ${i + 1}`}
                  onClick={() => removeRow(row.rowId)}
                >
                  ✕
                </button>
              </div>
            ))}
          </div>
          <button type="button" className={styles.cancelBtn} onClick={addRow}>
            + Add root
          </button>
          {hasDuplicateKey && (
            <p className={styles.hint} role="alert">
              Duplicate location keys — each key must be unique.
            </p>
          )}
          {hasEmptyKeyWithValue && (
            <p className={styles.hint} role="alert">
              A root path has no location key.
            </p>
          )}
          {!hasDefault && (
            <p className={styles.hint}>
              No &#34;default&#34; root set. Jobs without compute-location affinity require a
              default root.
            </p>
          )}
        </fieldset>

        <div className={styles.footer}>
          <button type="submit" className={styles.submitBtn} disabled={!canSubmit}>
            {mode === 'create' ? 'Create Storage Location' : 'Save'}
          </button>
          <Link to="/storage-locations" className={styles.cancelBtn}>
            Cancel
          </Link>
        </div>
      </form>
    </div>
  )
}

export default function StorageLocationForm({ mode }: Props) {
  const params = useParams<{ id: string }>()
  const id = params.id ?? ''
  const { data, isLoading, isError } = useGetStorageLocation(mode === 'edit' ? id : '')

  if (mode === 'edit') {
    if (isLoading || !data) {
      return (
        <div className={styles.page}>
          {isError ? <p>Failed to load storage location.</p> : <p>Loading…</p>}
        </div>
      )
    }
    return (
      <StorageLocationFormInner
        key={id}
        mode="edit"
        id={id}
        defaults={{
          name: data.name,
          description: data.description ?? '',
          roots: rootsToRows(data.roots),
        }}
      />
    )
  }

  return (
    <StorageLocationFormInner
      mode="create"
      id=""
      defaults={{ name: '', description: '', roots: rootsToRows(undefined) }}
    />
  )
}
