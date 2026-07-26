import { useState } from 'react'
import type { FormEvent } from 'react'
import { ArrowRight } from 'lucide-react'
import { errorMessage, setup } from '../api'
import type { User } from '../api'
import { useToast } from '../components/Toast'
import { Button, Field, Input, SectionNote } from '../components/ui'
import { AuthShell } from './AuthShell'

const MIN_PASSWORD_LENGTH = 8

export function SetupPage({ onAuthenticated }: { onAuthenticated: (user: User) => void }) {
  const toast = useToast()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const onSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    setError(null)

    if (username.trim().length < 3) {
      setError('Choose a username of at least 3 characters.')
      return
    }
    if (password.length < MIN_PASSWORD_LENGTH) {
      setError(`Use at least ${MIN_PASSWORD_LENGTH} characters for the password.`)
      return
    }
    if (password !== confirmPassword) {
      setError('The two passwords do not match.')
      return
    }

    setSubmitting(true)
    try {
      const result = await setup(username.trim(), password)
      toast.success('Administrator account created.', 'Welcome to ProxBack.')
      onAuthenticated(result.user)
    } catch (err) {
      const message = errorMessage(err)
      setError(message)
      toast.error('Setup failed', message)
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <AuthShell
      title="Create the administrator account"
      subtitle="This is the first run of this server. The account you create here owns the console."
      footer="Only one administrator can be created during setup. Additional users can be added later."
    >
      <form className="space-y-4" onSubmit={(event) => void onSubmit(event)} noValidate>
        <Field label="Username">
          {({ id }) => (
            <Input
              id={id}
              value={username}
              autoComplete="username"
              autoFocus
              placeholder="admin"
              onChange={(event) => setUsername(event.target.value)}
            />
          )}
        </Field>

        <Field label="Password" hint={`At least ${MIN_PASSWORD_LENGTH} characters.`}>
          {({ id }) => (
            <Input
              id={id}
              type="password"
              value={password}
              autoComplete="new-password"
              onChange={(event) => setPassword(event.target.value)}
            />
          )}
        </Field>

        <Field label="Confirm password">
          {({ id }) => (
            <Input
              id={id}
              type="password"
              value={confirmPassword}
              autoComplete="new-password"
              onChange={(event) => setConfirmPassword(event.target.value)}
            />
          )}
        </Field>

        {error ? (
          <p className="rounded-lg border border-red-500/30 bg-red-500/10 px-3.5 py-2.5 text-xs text-red-300">
            {error}
          </p>
        ) : (
          <SectionNote>
            Passwords are stored as bcrypt hashes. Sessions use an HTTP-only cookie.
          </SectionNote>
        )}

        <Button
          type="submit"
          variant="primary"
          size="lg"
          loading={submitting}
          className="w-full"
          icon={submitting ? undefined : <ArrowRight className="size-4" aria-hidden />}
        >
          {submitting ? 'Creating account…' : 'Create account and continue'}
        </Button>
      </form>
    </AuthShell>
  )
}
