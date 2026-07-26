import { useState } from 'react'
import type { FormEvent } from 'react'
import { LogIn } from 'lucide-react'
import { errorMessage, login } from '../api'
import type { User } from '../api'
import { useToast } from '../components/Toast'
import { Button, Field, Input } from '../components/ui'
import { AuthShell } from './AuthShell'

export function LoginPage({
  onAuthenticated,
  defaultLogin = false,
}: {
  onAuthenticated: (user: User) => void
  /** True while the seeded admin/admin credentials are unchanged. */
  defaultLogin?: boolean
}) {
  const toast = useToast()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const onSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    setError(null)
    if (!username.trim() || !password) {
      setError('Enter your username and password.')
      return
    }

    setSubmitting(true)
    try {
      const result = await login(username.trim(), password)
      toast.success(`Signed in as ${result.user.username}.`)
      onAuthenticated(result.user)
    } catch (err) {
      const message = errorMessage(err)
      setError(message)
      setPassword('')
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <AuthShell
      title="Sign in"
      subtitle="Use your ProxBack console credentials."
      footer="Sessions are kept in an HTTP-only cookie and expire on the server."
    >
      {defaultLogin ? (
        <p className="mb-4 rounded-lg border border-amber-500/30 bg-amber-500/10 px-3.5 py-2.5 text-xs text-amber-300">
          Fresh installation — sign in with <span className="font-semibold">admin</span> /{' '}
          <span className="font-semibold">admin</span>, then change the password right away.
        </p>
      ) : null}
      <form className="space-y-4" onSubmit={(event) => void onSubmit(event)} noValidate>
        <Field label="Username">
          {({ id }) => (
            <Input
              id={id}
              value={username}
              autoComplete="username"
              autoFocus
              onChange={(event) => setUsername(event.target.value)}
            />
          )}
        </Field>

        <Field label="Password">
          {({ id }) => (
            <Input
              id={id}
              type="password"
              value={password}
              autoComplete="current-password"
              onChange={(event) => setPassword(event.target.value)}
            />
          )}
        </Field>

        {error ? (
          <p className="rounded-lg border border-red-500/30 bg-red-500/10 px-3.5 py-2.5 text-xs text-red-300">
            {error}
          </p>
        ) : null}

        <Button
          type="submit"
          variant="primary"
          size="lg"
          loading={submitting}
          className="w-full"
          icon={submitting ? undefined : <LogIn className="size-4" aria-hidden />}
        >
          {submitting ? 'Signing in…' : 'Sign in'}
        </Button>
      </form>
    </AuthShell>
  )
}
