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
  roleEditable: boolean
  passwordEditable: boolean
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

  // Straight from the server's `role_editable`, which it computes from the
  // same predicate that guards PATCH /users/{id}. Deliberately not inferred
  // from auth_source: the guard is two conditions (auth_source=ldap AND
  // role_source=directory) and role_source is not on the wire, so inferring
  // would disable the control for LDAP accounts under role_source=local,
  // where the server does accept a role change.
  const roleManagedExternally = !defaults.roleEditable

  // Same contract, for the set-password control: straight from the server's
  // `password_editable`, computed from the same predicate that guards PUT
  // /users/{id}/password. Without it the form happily accepted a new password
  // for an LDAP or OIDC account and turned every submit into a 409 toast.
  const passwordManagedExternally = !defaults.passwordEditable

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
        // The role is omitted when the server says it is not editable. A
        // no-op role (equal to the stored one) would in fact be accepted —
        // the server compares against the current value before rejecting —
        // but the control is disabled here, so there is nothing to send and
        // omitting it keeps the request honest about what the form changed.
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
    if (passwordManagedExternally || newPassword === '' || setUserPassword.isPending) return
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
              {passwordManagedExternally && (
                <span className={styles.labelQualifier}> (managed externally)</span>
              )}
            </label>
            <input
              id="user-new-password"
              type="password"
              className={styles.input}
              value={newPassword}
              disabled={passwordManagedExternally}
              aria-describedby={passwordManagedExternally ? 'user-new-password-hint' : undefined}
              onChange={(e) => setNewPassword(e.target.value)}
            />
            {passwordManagedExternally && (
              <p id="user-new-password-hint" className={styles.hint}>
                This account signs in against an external provider and has no local password —
                change it there instead.
              </p>
            )}
          </div>
          <div className={styles.footer}>
            <button
              type="submit"
              className={styles.submitBtn}
              disabled={
                passwordManagedExternally || newPassword === '' || setUserPassword.isPending
              }
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
          roleEditable: data.role_editable,
          passwordEditable: data.password_editable,
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
        // POST /users always creates a local account, so its role is always
        // editable — there is no server response to read this from yet. The
        // same is true of its password, though the set-password section only
        // renders in edit mode, so this value is never read here.
        roleEditable: true,
        passwordEditable: true,
      }}
    />
  )
}
