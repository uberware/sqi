// SPDX-License-Identifier: AGPL-3.0-or-later

import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import PageHeader from '@/components/PageHeader'
import { useToast } from '@/components/Toast'
import { useGetUser } from '@/api/queries'
import {
  useCreateUser,
  useUpdateUser,
  useSetUserPassword,
  type UserCreateInput,
  type UserUpdateInput,
} from '@/api/mutations'
import styles from './UserForm.module.css'

interface Props {
  mode: 'create' | 'edit'
}

const ROLES = ['admin', 'operator', 'user', 'read-only'] as const

interface Defaults {
  username: string
  displayName: string
  role: string
  disabled: boolean
  authSource: string
}

interface InnerProps {
  mode: 'create' | 'edit'
  id: string
  defaults: Defaults
}

function UserFormInner({ mode, id, defaults }: InnerProps) {
  const navigate = useNavigate()
  const { showToast } = useToast()
  const createUser = useCreateUser()
  const updateUser = useUpdateUser()
  const setUserPassword = useSetUserPassword()

  const [username, setUsername] = useState(defaults.username)
  const [password, setPassword] = useState('')
  const [displayName, setDisplayName] = useState(defaults.displayName)
  const [role, setRole] = useState(defaults.role)
  const [disabled, setDisabled] = useState(defaults.disabled)
  const [newPassword, setNewPassword] = useState('')

  // Mirrors the server's PATCH /users/{id} guard: in role_source=directory
  // mode the group mapping owns an LDAP account's role. Showing an editable
  // control here would offer a change that is rejected on save.
  const roleManagedExternally = defaults.authSource === 'ldap'

  const isPending = createUser.isPending || updateUser.isPending
  const canSubmit =
    mode === 'create' ? username.trim() !== '' && password !== '' && !isPending : !isPending

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!canSubmit) return
    const trimmedUsername = username.trim()
    const trimmedDisplayName = displayName.trim()
    try {
      if (mode === 'create') {
        const input: UserCreateInput = {
          username: trimmedUsername,
          password,
          role,
          ...(trimmedDisplayName ? { display_name: trimmedDisplayName } : {}),
        }
        await createUser.mutateAsync(input)
        showToast(`User "${trimmedUsername}" created`, 'success')
      } else {
        // A directory-managed account's role is omitted rather than sent
        // unchanged: the server rejects any role field in the PATCH body for
        // an ldap account in role_source=directory mode, even one matching
        // its current value.
        const input: UserUpdateInput = roleManagedExternally
          ? { display_name: trimmedDisplayName, disabled }
          : { display_name: trimmedDisplayName, disabled, role }
        await updateUser.mutateAsync({ id, input })
        showToast(`User "${username}" saved`, 'success')
      }
      navigate('/users')
    } catch (err) {
      showToast(err instanceof Error ? err.message : 'Save failed', 'error')
    }
  }

  async function handleSetPassword(e: React.FormEvent) {
    e.preventDefault()
    if (newPassword === '' || setUserPassword.isPending) return
    try {
      await setUserPassword.mutateAsync({ id, password: newPassword })
      setNewPassword('')
      showToast('Password updated', 'success')
    } catch (err) {
      showToast(err instanceof Error ? err.message : 'Failed to set password', 'error')
    }
  }

  return (
    <div className={styles.page}>
      <PageHeader
        title={mode === 'create' ? 'New User' : 'Edit User'}
        subtitle="A local account that can sign in to this server"
      />

      <form className={styles.form} onSubmit={(e) => void handleSubmit(e)} noValidate>
        <div className={styles.field}>
          <label htmlFor="user-username" className={styles.label}>
            Username
          </label>
          <input
            id="user-username"
            className={styles.input}
            value={username}
            onChange={mode === 'create' ? (e) => setUsername(e.target.value) : undefined}
            readOnly={mode === 'edit'}
            required={mode === 'create'}
            aria-required={mode === 'create' || undefined}
          />
        </div>

        {mode === 'create' && (
          <div className={styles.field}>
            <label htmlFor="user-password" className={styles.label}>
              Password
            </label>
            <input
              id="user-password"
              type="password"
              className={styles.input}
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              required
              aria-required="true"
            />
          </div>
        )}

        <div className={styles.field}>
          <label htmlFor="user-display-name" className={styles.label}>
            Display Name (optional)
          </label>
          <input
            id="user-display-name"
            className={styles.input}
            value={displayName}
            onChange={(e) => setDisplayName(e.target.value)}
          />
        </div>

        <div className={styles.field}>
          <label htmlFor="user-role" className={styles.label}>
            Role
            {roleManagedExternally && (
              <span className={styles.labelQualifier}> (managed by the directory)</span>
            )}
          </label>
          <select
            id="user-role"
            className={styles.select}
            value={role}
            disabled={roleManagedExternally}
            aria-describedby={roleManagedExternally ? 'user-role-hint' : undefined}
            onChange={(e) => setRole(e.target.value)}
          >
            {ROLES.map((r) => (
              <option key={r} value={r}>
                {r}
              </option>
            ))}
          </select>
          {roleManagedExternally && (
            <p id="user-role-hint" className={styles.hint}>
              Managed by the directory — change this user&apos;s group membership instead.
            </p>
          )}
        </div>

        {mode === 'edit' && (
          <div className={styles.field}>
            <label htmlFor="user-disabled" className={styles.checkboxLabel}>
              <input
                id="user-disabled"
                type="checkbox"
                checked={disabled}
                onChange={(e) => setDisabled(e.target.checked)}
              />
              Disabled
            </label>
          </div>
        )}

        <div className={styles.footer}>
          <button type="submit" className={styles.submitBtn} disabled={!canSubmit}>
            {mode === 'create' ? 'Create User' : 'Save'}
          </button>
          <Link to="/users" className={styles.cancelBtn}>
            Cancel
          </Link>
        </div>
      </form>

      {mode === 'edit' && (
        <form
          className={styles.passwordSection}
          onSubmit={(e) => void handleSetPassword(e)}
          noValidate
        >
          <h2 className={styles.sectionHeading}>Change Password</h2>
          <div className={styles.field}>
            <label htmlFor="user-new-password" className={styles.label}>
              New Password
            </label>
            <input
              id="user-new-password"
              type="password"
              className={styles.input}
              value={newPassword}
              onChange={(e) => setNewPassword(e.target.value)}
            />
          </div>
          <div className={styles.footer}>
            <button
              type="submit"
              className={styles.submitBtn}
              disabled={newPassword === '' || setUserPassword.isPending}
            >
              Set Password
            </button>
          </div>
        </form>
      )}
    </div>
  )
}

export default function UserForm({ mode }: Props) {
  const params = useParams<{ id: string }>()
  const id = params.id ?? ''
  const { data, isLoading, isError } = useGetUser(mode === 'edit' ? id : '')

  if (mode === 'edit') {
    if (isLoading || !data) {
      return (
        <div className={styles.page}>{isError ? <p>Failed to load user.</p> : <p>Loading…</p>}</div>
      )
    }
    return (
      <UserFormInner
        key={id}
        mode="edit"
        id={id}
        defaults={{
          username: data.username,
          displayName: data.display_name ?? '',
          role: data.role,
          disabled: data.disabled,
          authSource: data.auth_source,
        }}
      />
    )
  }

  return (
    <UserFormInner
      mode="create"
      id=""
      defaults={{
        username: '',
        displayName: '',
        role: 'user',
        disabled: false,
        authSource: 'local',
      }}
    />
  )
}
