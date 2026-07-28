/**
 * Users.
 *
 * One shared admin account destroys attribution and makes delegation
 * impossible, so v0.6.0 gives every person their own account and one of three
 * roles. Admin-only, and the server enforces that — this page hides nothing it
 * relies on.
 *
 * The contract's two safety rules are enforced here as well as on the server:
 * **the last administrator cannot be deleted or demoted**. The controls that
 * would do it are disabled with the reason attached, rather than left inviting
 * and answered with a 409 after the click.
 */

import { useCallback, useMemo, useState } from 'react'
import type { FormEvent } from 'react'
import {
  Eye,
  EyeOff,
  KeyRound,
  Plus,
  RefreshCw,
  ShieldCheck,
  Trash2,
  UserCog,
  UserPlus,
  Users as UsersIcon,
} from 'lucide-react'
import {
  createUser,
  deleteUser,
  errorMessage,
  listUsers,
  MIN_PASSWORD_LENGTH,
  patchUser,
  ROLE_LABEL,
  ROLE_SUMMARY,
  ROLES,
} from '../api'
import type { Role, UserAccount } from '../api'
import { Modal } from '../components/Modal'
import { useConfirm } from '../components/Confirm'
import { useToast } from '../components/Toast'
import {
  Button,
  Card,
  CardHeader,
  ChoiceTile,
  Chip,
  EmptyState,
  ErrorBlock,
  Field,
  Hint,
  IconButton,
  Input,
  Num,
  PageHeader,
  SectionNote,
  SkeletonRows,
} from '../components/ui'
import { useAsync } from '../lib/useAsync'
import { formatDateTime, formatRelative } from '../lib/format'
import { useSession } from '../session'

const ROLE_ICON: Record<Role, typeof ShieldCheck> = {
  admin: ShieldCheck,
  operator: UserCog,
  viewer: Eye,
}

/** Why the last administrator's destructive controls are refused. */
const LAST_ADMIN_REASON =
  'This is the only administrator. Give someone else the administrator role first, or you would lock everyone out of this server.'

/** Radio group of roles, each with what it can actually do. */
function RolePicker({
  value,
  onChange,
  label,
}: {
  value: Role
  onChange: (role: Role) => void
  label: string
}) {
  return (
    <div role="radiogroup" aria-label={label} className="grid gap-2">
      {ROLES.map((role) => {
        const Icon = ROLE_ICON[role]
        return (
          <ChoiceTile
            key={role}
            selected={value === role}
            onSelect={() => onChange(role)}
            icon={<Icon className="size-4" aria-hidden />}
            title={ROLE_LABEL[role]}
            description={ROLE_SUMMARY[role]}
          />
        )
      })}
    </div>
  )
}

/** Password field with a reveal toggle — an admin setting someone else's first
 *  password usually has to read it out. */
function PasswordField({
  label,
  hint,
  value,
  onChange,
  autoFocus,
}: {
  label: string
  hint: string
  value: string
  onChange: (value: string) => void
  autoFocus?: boolean
}) {
  const [visible, setVisible] = useState(false)
  return (
    <Field label={label} hint={hint}>
      {({ id, describedBy }) => (
        <div className="flex gap-2">
          <Input
            id={id}
            aria-describedby={describedBy}
            type={visible ? 'text' : 'password'}
            value={value}
            autoFocus={autoFocus}
            autoComplete="new-password"
            spellCheck={false}
            onChange={(event) => onChange(event.target.value)}
          />
          <IconButton
            aria-label={visible ? 'Hide the password' : 'Show the password'}
            title={visible ? 'Hide the password' : 'Show the password'}
            aria-pressed={visible}
            onClick={() => setVisible((current) => !current)}
          >
            {visible ? <EyeOff className="size-4" aria-hidden /> : <Eye className="size-4" aria-hidden />}
          </IconButton>
        </div>
      )}
    </Field>
  )
}

/* ---------------------------------------------------------------------------
 * Add user
 * ------------------------------------------------------------------------- */

function AddUserModal({
  open,
  onClose,
  onSaved,
}: {
  open: boolean
  onClose: () => void
  onSaved: () => void
}) {
  const toast = useToast()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [role, setRole] = useState<Role>('operator')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const close = () => {
    setUsername('')
    setPassword('')
    setRole('operator')
    setError(null)
    onClose()
  }

  const onSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    setError(null)

    const name = username.trim()
    if (!name) return setError('Enter a username.')
    if (password.length < MIN_PASSWORD_LENGTH) {
      return setError(`The password must be at least ${MIN_PASSWORD_LENGTH} characters.`)
    }

    setSubmitting(true)
    try {
      const user = await createUser({ username: name, password, role })
      toast.success(`User “${user.username}” added.`, `Signed in as ${ROLE_LABEL[role].toLowerCase()}.`)
      close()
      onSaved()
    } catch (err) {
      const message = errorMessage(err)
      setError(message)
      toast.error('Could not add user', message)
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Modal
      open={open}
      onClose={close}
      title="Add user"
      subtitle="Everyone gets their own account, so the audit trail names a person rather than “admin”."
      width="lg"
      footer={
        <>
          <Button onClick={close}>Cancel</Button>
          <Button
            variant="primary"
            form="add-user-form"
            type="submit"
            loading={submitting}
            icon={<UserPlus className="size-4" aria-hidden />}
          >
            Add user
          </Button>
        </>
      }
    >
      <form
        id="add-user-form"
        className="space-y-4"
        onSubmit={(event) => void onSubmit(event)}
        noValidate
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Username">
            {({ id }) => (
              <Input
                id={id}
                value={username}
                autoFocus
                autoComplete="off"
                spellCheck={false}
                placeholder="jrivera"
                onChange={(event) => setUsername(event.target.value)}
              />
            )}
          </Field>

          <PasswordField
            label="Password"
            hint={`At least ${MIN_PASSWORD_LENGTH} characters. They can change it themselves later.`}
            value={password}
            onChange={setPassword}
          />
        </div>

        <div className="space-y-2">
          <p className="text-xs font-medium tracking-wide text-slate-400">Role</p>
          <RolePicker value={role} onChange={setRole} label="Role for the new user" />
        </div>

        {error ? (
          <p
            className="rounded-lg border border-fail-500/30 bg-fail-500/10 px-3.5 py-2.5 text-xs text-fail-300"
            role="alert"
          >
            {error}
          </p>
        ) : (
          <Hint>A role can be changed at any time, and every change is recorded in the audit trail.</Hint>
        )}
      </form>
    </Modal>
  )
}

/* ---------------------------------------------------------------------------
 * Change role
 * ------------------------------------------------------------------------- */

function ChangeRoleModal({
  user,
  onClose,
  onSaved,
}: {
  user: UserAccount | null
  onClose: () => void
  onSaved: () => void
}) {
  const toast = useToast()
  const [role, setRole] = useState<Role>(user?.role ?? 'viewer')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [seeded, setSeeded] = useState<string | null>(null)

  // Seed from whichever user the dialog was opened for, without an effect.
  const key = user ? String(user.id) : null
  if (key !== seeded) {
    setSeeded(key)
    setRole(user?.role ?? 'viewer')
    setError(null)
  }

  const onSubmit = async () => {
    if (!user) return
    if (role === user.role) return onClose()
    setError(null)
    setSubmitting(true)
    try {
      await patchUser(user.id, { role })
      toast.success(`${user.username} is now ${ROLE_LABEL[role].toLowerCase()}.`)
      onClose()
      onSaved()
    } catch (err) {
      const message = errorMessage(err)
      setError(message)
      toast.error('Could not change the role', message)
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Modal
      open={user !== null}
      onClose={onClose}
      title={user ? `Change role — ${user.username}` : 'Change role'}
      subtitle="What this account may do from the next request onwards."
      width="lg"
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button
            variant="primary"
            loading={submitting}
            disabled={!user || role === user.role}
            onClick={() => void onSubmit()}
          >
            Save role
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        <RolePicker value={role} onChange={setRole} label="New role" />
        {error ? (
          <p
            className="rounded-lg border border-fail-500/30 bg-fail-500/10 px-3.5 py-2.5 text-xs text-fail-300"
            role="alert"
          >
            {error}
          </p>
        ) : (
          <Hint>Existing sessions pick up the new role on their next request.</Hint>
        )}
      </div>
    </Modal>
  )
}

/* ---------------------------------------------------------------------------
 * Reset password
 * ------------------------------------------------------------------------- */

function ResetPasswordModal({
  user,
  onClose,
}: {
  user: UserAccount | null
  onClose: () => void
}) {
  const toast = useToast()
  const [password, setPassword] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const close = () => {
    setPassword('')
    setError(null)
    onClose()
  }

  const onSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    if (!user) return
    setError(null)
    if (password.length < MIN_PASSWORD_LENGTH) {
      return setError(`The password must be at least ${MIN_PASSWORD_LENGTH} characters.`)
    }
    setSubmitting(true)
    try {
      await patchUser(user.id, { password })
      toast.success(`Password reset for ${user.username}.`, 'Tell them to change it after signing in.')
      close()
    } catch (err) {
      const message = errorMessage(err)
      setError(message)
      toast.error('Could not reset the password', message)
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Modal
      open={user !== null}
      onClose={close}
      title={user ? `Reset password — ${user.username}` : 'Reset password'}
      width="md"
      footer={
        <>
          <Button onClick={close}>Cancel</Button>
          <Button
            variant="primary"
            form="reset-password-form"
            type="submit"
            loading={submitting}
            icon={<KeyRound className="size-4" aria-hidden />}
          >
            Reset password
          </Button>
        </>
      }
    >
      <form
        id="reset-password-form"
        className="space-y-4"
        onSubmit={(event) => void onSubmit(event)}
        noValidate
      >
        <PasswordField
          label="New password"
          hint={`At least ${MIN_PASSWORD_LENGTH} characters.`}
          value={password}
          onChange={setPassword}
          autoFocus
        />
        {error ? (
          <p
            className="rounded-lg border border-fail-500/30 bg-fail-500/10 px-3.5 py-2.5 text-xs text-fail-300"
            role="alert"
          >
            {error}
          </p>
        ) : (
          <SectionNote>
            You are setting this password on someone else's behalf, so ProxBack cannot show it to
            them — pass it on yourself and ask them to change it from Settings.
          </SectionNote>
        )}
      </form>
    </Modal>
  )
}

/* ---------------------------------------------------------------------------
 * Table
 * ------------------------------------------------------------------------- */

function RoleChip({ role }: { role: Role }) {
  const Icon = ROLE_ICON[role]
  return (
    <Chip mono={false} icon={<Icon className="size-3" aria-hidden />} title={ROLE_SUMMARY[role]}>
      {ROLE_LABEL[role]}
    </Chip>
  )
}

function UsersTable({
  users,
  onChangeRole,
  onResetPassword,
  onDeleted,
}: {
  users: UserAccount[]
  onChangeRole: (user: UserAccount) => void
  onResetPassword: (user: UserAccount) => void
  onDeleted: () => void
}) {
  const toast = useToast()
  const confirm = useConfirm()
  const { user: me, signOut } = useSession()
  const [deleting, setDeleting] = useState<string | null>(null)

  const adminCount = users.filter((user) => user.role === 'admin').length

  const onDelete = async (user: UserAccount) => {
    const self = String(user.id) === String(me.id)
    const ok = await confirm({
      title: 'Delete user',
      message: (
        <>
          Delete <span className="font-medium text-slate-100">{user.username}</span>? Their sessions
          end immediately. Jobs they created keep running, and the audit trail keeps everything they
          did.
          {self ? ' This is the account you are signed in with — you will be signed out.' : ''}
        </>
      ),
      confirmLabel: 'Delete user',
    })
    if (!ok) return

    setDeleting(String(user.id))
    try {
      await deleteUser(user.id)
      toast.success(`User “${user.username}” deleted.`)
      if (self) signOut()
      else onDeleted()
    } catch (err) {
      toast.error('Could not delete the user', errorMessage(err))
    } finally {
      setDeleting(null)
    }
  }

  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[52rem] text-sm">
        <thead>
          <tr className="border-b border-slate-800 text-left text-micro font-semibold tracking-wide text-slate-500 uppercase">
            <th className="px-5 py-2.5 font-medium">Username</th>
            <th className="px-5 py-2.5 font-medium">Role</th>
            <th className="px-5 py-2.5 font-medium">Created</th>
            <th className="px-5 py-2.5 font-medium">Last sign-in</th>
            <th className="px-5 py-2.5" />
          </tr>
        </thead>
        <tbody className="divide-y divide-slate-800/70">
          {users.map((user) => {
            const self = String(user.id) === String(me.id)
            // The contract refuses both a demote and a delete here with a 409,
            // so say why before the click rather than after it.
            const lastAdmin = user.role === 'admin' && adminCount <= 1
            const blocked = lastAdmin ? LAST_ADMIN_REASON : undefined

            return (
              <tr
                key={String(user.id)}
                className="transition-colors duration-150 hover:bg-slate-800/30"
              >
                <td className="px-5 py-3">
                  <div className="flex items-center gap-2.5">
                    <span className="flex size-7 shrink-0 items-center justify-center rounded-lg border border-slate-800 bg-slate-950/60 text-accent-400">
                      <UsersIcon className="size-3.5" aria-hidden />
                    </span>
                    <span className="truncate font-medium text-slate-100">{user.username}</span>
                    {self ? (
                      <span className="shrink-0 text-meta text-slate-500">(you)</span>
                    ) : null}
                  </div>
                </td>
                <td className="px-5 py-3">
                  <RoleChip role={user.role} />
                </td>
                <td className="px-5 py-3 whitespace-nowrap">
                  <Num className="text-slate-400">{formatDateTime(user.createdAt)}</Num>
                </td>
                <td className="px-5 py-3 whitespace-nowrap">
                  {user.lastLoginAt ? (
                    <Num className="text-slate-400" title={formatDateTime(user.lastLoginAt)}>
                      {formatRelative(user.lastLoginAt)}
                    </Num>
                  ) : (
                    <span className="text-meta text-slate-500">Never signed in</span>
                  )}
                </td>
                <td className="px-5 py-3">
                  <div className="flex items-center justify-end gap-1">
                    <IconButton
                      aria-label={
                        blocked
                          ? `Change the role of ${user.username} — ${blocked}`
                          : `Change the role of ${user.username}`
                      }
                      title={blocked ?? 'Change role'}
                      disabled={lastAdmin}
                      onClick={() => onChangeRole(user)}
                    >
                      <UserCog className="size-4" aria-hidden />
                    </IconButton>
                    <IconButton
                      aria-label={`Reset the password of ${user.username}`}
                      title="Reset password"
                      onClick={() => onResetPassword(user)}
                    >
                      <KeyRound className="size-4" aria-hidden />
                    </IconButton>
                    <IconButton
                      variant="dangerQuiet"
                      aria-label={
                        blocked ? `Delete ${user.username} — ${blocked}` : `Delete ${user.username}`
                      }
                      title={blocked ?? 'Delete user'}
                      disabled={lastAdmin}
                      loading={deleting === String(user.id)}
                      onClick={() => void onDelete(user)}
                    >
                      <Trash2 className="size-4" aria-hidden />
                    </IconButton>
                  </div>
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}

/* ---------------------------------------------------------------------------
 * Page
 * ------------------------------------------------------------------------- */

export function UsersPage() {
  const loader = useCallback(() => listUsers(), [])
  const { data, loading, error, reload, refresh } = useAsync(loader)
  const [addOpen, setAddOpen] = useState(false)
  const [roleFor, setRoleFor] = useState<UserAccount | null>(null)
  const [passwordFor, setPasswordFor] = useState<UserAccount | null>(null)

  const users = useMemo(
    () =>
      [...(data ?? [])].sort(
        (a, b) => ROLES.indexOf(a.role) - ROLES.indexOf(b.role) || a.username.localeCompare(b.username),
      ),
    [data],
  )
  const admins = users.filter((user) => user.role === 'admin').length

  const addButton = (
    <Button
      variant="primary"
      icon={<Plus className="size-4" aria-hidden />}
      onClick={() => setAddOpen(true)}
    >
      Add User
    </Button>
  )

  return (
    <>
      <PageHeader
        title="Users"
        description="Who can sign in, and what each of them may do. Every action they take is attributed to them in the audit trail."
        actions={
          <>
            <Button
              icon={<RefreshCw className="size-4" aria-hidden />}
              onClick={() => void reload()}
              loading={loading}
            >
              Refresh
            </Button>
            {addButton}
          </>
        }
      />

      {loading && !data ? (
        <Card>
          <SkeletonRows count={3} />
        </Card>
      ) : error && !data ? (
        <ErrorBlock message={error} onRetry={() => void reload()} />
      ) : users.length === 0 ? (
        <EmptyState
          icon={<UsersIcon className="size-5" aria-hidden />}
          title="No user accounts listed"
          description="This server did not return any accounts. At least one administrator must exist, so this usually means the request was refused — try refreshing."
          action={
            <Button variant="primary" onClick={() => void reload()}>
              Try again
            </Button>
          }
        />
      ) : (
        <Card elevation="flat">
          <CardHeader
            title="Accounts"
            subtitle={
              admins <= 1
                ? 'One administrator. Add a second so a forgotten password cannot lock you out.'
                : `${users.length} accounts · ${admins} administrators`
            }
          />
          <UsersTable
            users={users}
            onChangeRole={setRoleFor}
            onResetPassword={setPasswordFor}
            onDeleted={() => void refresh()}
          />
        </Card>
      )}

      <AddUserModal
        open={addOpen}
        onClose={() => setAddOpen(false)}
        onSaved={() => void refresh()}
      />
      <ChangeRoleModal
        user={roleFor}
        onClose={() => setRoleFor(null)}
        onSaved={() => void refresh()}
      />
      <ResetPasswordModal user={passwordFor} onClose={() => setPasswordFor(null)} />
    </>
  )
}
