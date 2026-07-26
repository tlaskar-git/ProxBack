import { useCallback, useEffect, useState } from 'react'
import type { FormEvent } from 'react'
import {
  ArrowUpCircle,
  CheckCircle2,
  ExternalLink,
  KeyRound,
  RefreshCw,
  Save,
  SlidersHorizontal,
} from 'lucide-react'
import {
  applyUpdate,
  changePassword,
  errorMessage,
  getSettings,
  getSetupStatus,
  getUpdateStatus,
  putSettings,
} from '../api'
import type { Settings, UpdateStatus } from '../api'
import { useToast } from '../components/Toast'
import { useConfirm } from '../components/Confirm'
import { useSession } from '../session'
import {
  Button,
  Card,
  CardHeader,
  ErrorBlock,
  Field,
  Input,
  LoadingBlock,
  PageHeader,
  SectionNote,
  Spinner,
} from '../components/ui'
import { useAsync } from '../lib/useAsync'

const MAX_CONCURRENCY = 32

function SoftwareUpdateCard() {
  const toast = useToast()
  const confirm = useConfirm()
  const loader = useCallback(() => getUpdateStatus(), [])
  const { data: status, loading, error, reload } = useAsync<UpdateStatus>(loader)
  const [applying, setApplying] = useState(false)
  const [restarting, setRestarting] = useState(false)

  const install = async () => {
    if (!status) return
    const ok = await confirm({
      title: `Install ProxBack ${status.latestVersion}?`,
      message:
        'The new server binary is downloaded, checksum-verified, and swapped in. The server then restarts itself — expect a few seconds of downtime. Let running backup jobs finish first.',
      confirmLabel: 'Install update',
      destructive: false,
    })
    if (!ok) return
    setApplying(true)
    try {
      const result = await applyUpdate()
      if (result.restarting) {
        setRestarting(true)
        // Poll an unauthenticated endpoint until the new build answers, then reload.
        const started = Date.now()
        const poll = window.setInterval(() => {
          getSetupStatus()
            .then(() => {
              window.clearInterval(poll)
              window.location.reload()
            })
            .catch(() => {
              if (Date.now() - started > 120_000) window.clearInterval(poll)
            })
        }, 2000)
      } else {
        toast.success(
          `Version ${result.version} installed.`,
          'Restart the ProxBack service to start running it.',
        )
        await reload()
      }
    } catch (err) {
      const message = errorMessage(err)
      toast.error('Update failed', message)
      setApplying(false)
    }
  }

  return (
    <Card className="mt-6 max-w-2xl">
      <CardHeader
        title="Software update"
        subtitle="Pulled from the ProxBack release repository on GitHub."
      />
      <div className="space-y-4 px-5 py-5">
        {restarting ? (
          <div className="flex items-center gap-3 rounded-lg border border-accent-500/30 bg-accent-500/10 px-4 py-3 text-sm text-accent-200">
            <Spinner />
            Installing and restarting the server — this page reloads automatically.
          </div>
        ) : loading && !status ? (
          <LoadingBlock label="Checking for updates…" />
        ) : error && !status ? (
          <ErrorBlock message={error} onRetry={() => void reload()} />
        ) : status ? (
          <>
            <div className="flex flex-wrap items-center gap-x-6 gap-y-2 text-sm">
              <div>
                <div className="text-[11px] tracking-wide text-slate-500 uppercase">Installed</div>
                <div className="font-mono text-slate-200">v{status.currentVersion}</div>
              </div>
              <div>
                <div className="text-[11px] tracking-wide text-slate-500 uppercase">Latest</div>
                <div className="font-mono text-slate-200">
                  {status.latestVersion ? `v${status.latestVersion}` : '— none published'}
                </div>
              </div>
            </div>

            {status.checkError ? (
              <p className="rounded-lg border border-amber-500/30 bg-amber-500/10 px-3.5 py-2.5 text-xs text-amber-300">
                Could not reach the release repository: {status.checkError}
              </p>
            ) : status.updateAvailable ? (
              <div className="space-y-3 rounded-lg border border-accent-500/30 bg-accent-500/10 px-4 py-3">
                <p className="text-sm font-medium text-accent-200">
                  Version {status.latestVersion} is available.
                </p>
                {status.releaseNotes ? (
                  <p className="line-clamp-4 text-xs whitespace-pre-line text-slate-400">
                    {status.releaseNotes}
                  </p>
                ) : null}
                {!status.assetAvailable ? (
                  <p className="text-xs text-amber-300">
                    The release has no prebuilt binary for this platform — update manually from
                    source.
                  </p>
                ) : null}
              </div>
            ) : status.latestVersion ? (
              <p className="flex items-center gap-2 text-sm text-slate-400">
                <CheckCircle2 className="size-4 text-accent-400" aria-hidden />
                You are running the latest version.
              </p>
            ) : (
              <p className="text-sm text-slate-400">
                No releases have been published yet. Once a release is tagged on GitHub, it appears
                here.
              </p>
            )}

            <div className="flex flex-wrap items-center gap-3 border-t border-slate-800 pt-4">
              {status.updateAvailable && status.assetAvailable ? (
                <Button
                  variant="primary"
                  loading={applying}
                  onClick={() => void install()}
                  icon={<ArrowUpCircle className="size-4" aria-hidden />}
                >
                  Install update
                </Button>
              ) : null}
              <Button
                icon={<RefreshCw className="size-4" aria-hidden />}
                onClick={() => void reload()}
                loading={loading}
              >
                Check again
              </Button>
              {status.releaseUrl ? (
                <a
                  href={status.releaseUrl}
                  target="_blank"
                  rel="noreferrer"
                  className="inline-flex items-center gap-1.5 text-xs text-slate-400 transition hover:text-slate-200"
                >
                  Release notes on GitHub
                  <ExternalLink className="size-3.5" aria-hidden />
                </a>
              ) : null}
            </div>
          </>
        ) : null}
      </div>
    </Card>
  )
}

function ChangePasswordCard() {
  const toast = useToast()
  const { mustChangePassword, setMustChangePassword } = useSession()
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const onSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    setError(null)
    if (!current || !next) {
      setError('Enter your current password and the new one.')
      return
    }
    if (next.length < 8) {
      setError('The new password must be at least 8 characters.')
      return
    }
    if (next !== confirm) {
      setError('The new passwords do not match.')
      return
    }

    setSubmitting(true)
    try {
      await changePassword(current, next)
      setCurrent('')
      setNext('')
      setConfirm('')
      setMustChangePassword(false)
      toast.success('Password changed.', 'Other signed-in sessions have been revoked.')
    } catch (err) {
      const message = errorMessage(err)
      setError(message)
      toast.error('Could not change the password', message)
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Card className="mt-6 max-w-2xl">
      <CardHeader
        title="Password"
        subtitle={
          mustChangePassword
            ? 'This server still uses the default admin / admin password — change it now.'
            : 'Change the password you sign in with.'
        }
      />
      <form className="space-y-5 px-5 py-5" onSubmit={(event) => void onSubmit(event)} noValidate>
        <Field label="Current password">
          {({ id }) => (
            <Input
              id={id}
              type="password"
              value={current}
              autoComplete="current-password"
              onChange={(event) => setCurrent(event.target.value)}
            />
          )}
        </Field>
        <Field label="New password" hint="At least 8 characters.">
          {({ id }) => (
            <Input
              id={id}
              type="password"
              value={next}
              autoComplete="new-password"
              onChange={(event) => setNext(event.target.value)}
            />
          )}
        </Field>
        <Field label="Confirm new password">
          {({ id }) => (
            <Input
              id={id}
              type="password"
              value={confirm}
              autoComplete="new-password"
              onChange={(event) => setConfirm(event.target.value)}
            />
          )}
        </Field>

        <SectionNote>
          Changing the password signs out every other session, so a stolen cookie dies with the old
          password.
        </SectionNote>

        {error ? (
          <p className="rounded-lg border border-red-500/30 bg-red-500/10 px-3.5 py-2.5 text-xs text-red-300">
            {error}
          </p>
        ) : null}

        <div className="flex items-center gap-3 border-t border-slate-800 pt-4">
          <Button
            type="submit"
            variant="primary"
            loading={submitting}
            icon={<KeyRound className="size-4" aria-hidden />}
          >
            Change password
          </Button>
        </div>
      </form>
    </Card>
  )
}

export function SettingsPage() {
  const toast = useToast()
  const { setServerName } = useSession()
  const loader = useCallback(() => getSettings(), [])
  const { data, loading, error, reload } = useAsync(loader)

  const [form, setForm] = useState<Settings>({ serverName: '', concurrency: 2 })
  const [submitting, setSubmitting] = useState(false)
  const [formError, setFormError] = useState<string | null>(null)

  useEffect(() => {
    if (data) setForm(data)
  }, [data])

  const dirty =
    data !== null && (data.serverName !== form.serverName || data.concurrency !== form.concurrency)

  const onSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    setFormError(null)

    const serverName = form.serverName.trim()
    if (!serverName) {
      setFormError('Give this server a display name.')
      return
    }
    if (
      !Number.isInteger(form.concurrency) ||
      form.concurrency < 1 ||
      form.concurrency > MAX_CONCURRENCY
    ) {
      setFormError(`Concurrency must be a whole number between 1 and ${MAX_CONCURRENCY}.`)
      return
    }

    setSubmitting(true)
    try {
      const saved = await putSettings({ serverName, concurrency: form.concurrency })
      setForm(saved)
      setServerName(saved.serverName)
      toast.success('Settings saved.')
      await reload()
    } catch (err) {
      const message = errorMessage(err)
      setFormError(message)
      toast.error('Could not save settings', message)
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <>
      <PageHeader
        title="Settings"
        description="Server identity and how many backup tasks may run at once."
        actions={
          <Button
            icon={<RefreshCw className="size-4" aria-hidden />}
            onClick={() => void reload()}
            loading={loading}
          >
            Reload
          </Button>
        }
      />

      {loading && !data ? (
        <Card>
          <LoadingBlock label="Loading settings…" />
        </Card>
      ) : error && !data ? (
        <ErrorBlock message={error} onRetry={() => void reload()} />
      ) : (
        <Card className="max-w-2xl">
          <CardHeader title="Server" subtitle="Applies to this ProxBack installation" />
          <form className="space-y-5 px-5 py-5" onSubmit={(event) => void onSubmit(event)} noValidate>
            <Field label="Server name" hint="Shown in the topbar and in notification subjects.">
              {({ id }) => (
                <Input
                  id={id}
                  value={form.serverName}
                  placeholder="ProxBack"
                  onChange={(event) =>
                    setForm((current) => ({ ...current, serverName: event.target.value }))
                  }
                />
              )}
            </Field>

            <Field
              label="Concurrency"
              hint={`How many disks or agent streams the engine processes in parallel. 1 – ${MAX_CONCURRENCY}.`}
            >
              {({ id }) => (
                <Input
                  id={id}
                  type="number"
                  min={1}
                  max={MAX_CONCURRENCY}
                  value={String(form.concurrency)}
                  className="w-32"
                  onChange={(event) =>
                    setForm((current) => ({
                      ...current,
                      concurrency: Number(event.target.value),
                    }))
                  }
                />
              )}
            </Field>

            <SectionNote>
              Higher concurrency finishes large jobs sooner but puts more read pressure on your
              Proxmox storage and more upload pressure on your link. Two to four is a good starting
              point.
            </SectionNote>

            {formError ? (
              <p className="rounded-lg border border-red-500/30 bg-red-500/10 px-3.5 py-2.5 text-xs text-red-300">
                {formError}
              </p>
            ) : null}

            <div className="flex items-center gap-3 border-t border-slate-800 pt-4">
              <Button
                type="submit"
                variant="primary"
                loading={submitting}
                disabled={!dirty}
                icon={<Save className="size-4" aria-hidden />}
              >
                Save changes
              </Button>
              {dirty ? (
                <Button onClick={() => data && setForm(data)} disabled={submitting}>
                  Discard
                </Button>
              ) : (
                <span className="flex items-center gap-1.5 text-xs text-slate-500">
                  <SlidersHorizontal className="size-3.5" aria-hidden />
                  No unsaved changes
                </span>
              )}
            </div>
          </form>
        </Card>
      )}

      <SoftwareUpdateCard />
      <ChangePasswordCard />
    </>
  )
}
