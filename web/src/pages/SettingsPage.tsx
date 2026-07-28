import { useCallback, useEffect, useState } from 'react'
import type { FormEvent } from 'react'
import {
  ArrowUpCircle,
  BellRing,
  CheckCircle2,
  ExternalLink,
  Gauge,
  KeyRound,
  RefreshCw,
  Save,
  Send,
  SlidersHorizontal,
} from 'lucide-react'
import {
  applyUpdate,
  changePassword,
  errorMessage,
  getSettings,
  getSetupStatus,
  getUpdateStatus,
  isApiError,
  putSettings,
  ROLE_LABEL,
  testWebhook,
} from '../api'
import type { Compression, NotifyOn, Settings, UpdateStatus } from '../api'
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
  Segmented,
  Spinner,
} from '../components/ui'
import { useAsync } from '../lib/useAsync'

const MAX_CONCURRENCY = 32
const MAX_UPLOAD_CONCURRENCY = 16
const MAX_UPLOAD_LIMIT = 10000

const DEFAULT_SETTINGS: Settings = {
  serverName: '',
  concurrency: 2,
  webhookUrl: '',
  notifyOn: 'off',
  uploadConcurrency: 4,
  compression: 'zstd',
  uploadLimitMbps: 0,
}

const NOTIFY_OPTIONS: { value: NotifyOn; label: string }[] = [
  { value: 'off', label: 'Off' },
  { value: 'failures', label: 'Failures only' },
  { value: 'all', label: 'All runs' },
]

function SoftwareUpdateCard() {
  const toast = useToast()
  const confirm = useConfirm()
  const loader = useCallback(() => getUpdateStatus(), [])
  const { data: status, loading, error, reload } = useAsync<UpdateStatus>(loader)
  const [applying, setApplying] = useState(false)
  const [restarting, setRestarting] = useState(false)
  const [blockedByRuns, setBlockedByRuns] = useState<string | null>(null)

  const install = async (force = false) => {
    if (!status) return
    const ok = await confirm({
      title: force
        ? `Install ${status.latestVersion} and cancel running work?`
        : `Install ProxBack ${status.latestVersion}?`,
      message: force
        ? 'The runs in progress will be canceled by the restart. Data they already transferred is kept, so a retry resumes cheaply — but the restore points from those runs are not created.'
        : 'The new server binary is downloaded, checksum-verified, and swapped in. The server then restarts itself — expect a few seconds of downtime.',
      confirmLabel: force ? 'Install anyway' : 'Install update',
      destructive: force,
    })
    if (!ok) return
    setApplying(true)
    try {
      const result = await applyUpdate(force)
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
      setApplying(false)
      // The server refuses to update while runs are in flight; offer the override.
      if (!force && isApiError(err) && err.isConflict && /in progress/i.test(err.message)) {
        setBlockedByRuns(err.message)
        return
      }
      toast.error('Update failed', errorMessage(err))
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
                <div className="eyebrow">Installed</div>
                <div className="font-mono text-slate-200">v{status.currentVersion}</div>
              </div>
              <div>
                <div className="eyebrow">Latest</div>
                <div className="font-mono text-slate-200">
                  {status.latestVersion ? `v${status.latestVersion}` : '— none published'}
                </div>
              </div>
            </div>

            {status.checkError ? (
              <p className="rounded-lg border border-warn-500/30 bg-warn-500/10 px-3.5 py-2.5 text-xs text-warn-300">
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
                  <p className="text-xs text-warn-300">
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

            {blockedByRuns ? (
              <div className="space-y-2 rounded-lg border border-warn-500/30 bg-warn-500/10 px-4 py-3">
                <p className="text-sm text-warn-200">{blockedByRuns}</p>
                <div className="flex flex-wrap items-center gap-2">
                  <Button size="sm" onClick={() => setBlockedByRuns(null)}>
                    Wait
                  </Button>
                  <Button
                    size="sm"
                    variant="danger"
                    loading={applying}
                    onClick={() => {
                      setBlockedByRuns(null)
                      void install(true)
                    }}
                  >
                    Install anyway
                  </Button>
                </div>
              </div>
            ) : null}

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
                  className="inline-flex items-center gap-1.5 text-xs text-slate-400 transition-colors duration-150 hover:text-slate-200"
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

function NotificationsCard({
  form,
  patch,
  dirty,
  savedWebhookUrl,
  saving,
  error,
  onSave,
  onDiscard,
}: {
  form: Settings
  patch: (next: Partial<Settings>) => void
  dirty: boolean
  savedWebhookUrl: string
  saving: boolean
  error: string | null
  onSave: () => void
  onDiscard: () => void
}) {
  const toast = useToast()
  const [testing, setTesting] = useState(false)

  const onTest = async () => {
    setTesting(true)
    try {
      const result = await testWebhook()
      if (result.ok) {
        toast.success(
          'Test notification sent.',
          'A sample run.finished payload went to the saved URL.',
        )
      } else {
        toast.error(
          'The endpoint did not accept the test',
          result.error ?? 'No further detail was reported.',
        )
      }
    } catch (err) {
      toast.error('Could not send the test', errorMessage(err))
    } finally {
      setTesting(false)
    }
  }

  return (
    <Card className="mt-6 max-w-2xl">
      <CardHeader
        title="Notifications"
        subtitle="POST a JSON summary to a webhook whenever a run finishes."
      />
      <form
        className="space-y-5 px-5 py-5"
        onSubmit={(event) => {
          event.preventDefault()
          onSave()
        }}
        noValidate
      >
        <Field
          label="Webhook URL"
          hint="Leave empty to disable notifications entirely."
        >
          {({ id }) => (
            <Input
              id={id}
              type="url"
              value={form.webhookUrl}
              placeholder="https://ntfy.sh/your-topic or any endpoint that accepts JSON"
              onChange={(event) => patch({ webhookUrl: event.target.value })}
            />
          )}
        </Field>

        <div className="space-y-1.5">
          <span className="block text-xs font-medium tracking-wide text-slate-400">Notify on</span>
          <Segmented
            label="Which runs trigger a notification"
            value={form.notifyOn}
            options={NOTIFY_OPTIONS}
            onChange={(notifyOn) => patch({ notifyOn })}
          />
          <p className="text-xs leading-relaxed text-slate-500">
            {form.notifyOn === 'off'
              ? 'Nothing is sent, even with a URL saved.'
              : form.notifyOn === 'failures'
                ? 'Only failed backup, restore, and verify runs are reported.'
                : 'Every finished backup, restore, and verify run is reported.'}
          </p>
        </div>

        <SectionNote>
          The payload is plain JSON — event, server, job, kind, status, bytes, data reduction, duration,
          and the error when there is one. It works as-is with ntfy, Gotify, a Discord webhook proxy,
          or any automation endpoint. Delivery has a 10-second timeout and never blocks a run.
        </SectionNote>

        {error ? (
          <p className="rounded-lg border border-fail-500/30 bg-fail-500/10 px-3.5 py-2.5 text-xs text-fail-300">
            {error}
          </p>
        ) : null}

        <div className="flex flex-wrap items-center gap-3 border-t border-slate-800 pt-4">
          <Button
            type="submit"
            variant="primary"
            loading={saving}
            disabled={!dirty}
            icon={<Save className="size-4" aria-hidden />}
          >
            Save changes
          </Button>
          <Button
            loading={testing}
            disabled={!savedWebhookUrl || dirty}
            onClick={() => void onTest()}
            icon={<Send className="size-4" aria-hidden />}
            title={
              !savedWebhookUrl
                ? 'Save a webhook URL first.'
                : dirty
                  ? 'Save your changes first.'
                  : undefined
            }
          >
            Send test
          </Button>
          {dirty ? (
            <Button onClick={onDiscard} disabled={saving}>
              Discard
            </Button>
          ) : (
            <span className="flex items-center gap-1.5 text-xs text-slate-500">
              <BellRing className="size-3.5" aria-hidden />
              {savedWebhookUrl
                ? form.notifyOn === 'off'
                  ? 'URL saved — notifications are off'
                  : 'Notifications are active'
                : 'No webhook configured'}
            </span>
          )}
        </div>
      </form>
    </Card>
  )
}

const COMPRESSION_OPTIONS: { value: Compression; label: string }[] = [
  { value: 'zstd', label: 'zstd' },
  { value: 'off', label: 'Off' },
]

/**
 * Throughput controls. Defaults are deliberate: four parallel transfers and
 * zstd compression roughly halve the time of a first full backup on a typical
 * home uplink, and neither weakens data reduction.
 */
function PerformanceCard({
  form,
  patch,
  dirty,
  saving,
  error,
  onSave,
  onDiscard,
}: {
  form: Settings
  patch: (next: Partial<Settings>) => void
  dirty: boolean
  saving: boolean
  error: string | null
  onSave: () => void
  onDiscard: () => void
}) {
  return (
    <Card className="mt-6 max-w-2xl">
      <CardHeader
        title="Performance"
        subtitle="How hard ProxBack pushes your uplink and storage target."
      />
      <form
        className="space-y-5 px-5 py-5"
        onSubmit={(event) => {
          event.preventDefault()
          onSave()
        }}
        noValidate
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field
            label="Parallel transfers"
            hint="1 – 16. Four suits most links."
          >
            {({ id }) => (
              <Input
                id={id}
                type="number"
                min={1}
                max={16}
                className="w-28"
                value={String(form.uploadConcurrency)}
                onChange={(event) => patch({ uploadConcurrency: Number(event.target.value) })}
              />
            )}
          </Field>
          <Field
            label="Transfer limit (Mbps)"
            hint="0 leaves transfers unthrottled."
          >
            {({ id }) => (
              <Input
                id={id}
                type="number"
                min={0}
                max={10000}
                className="w-28"
                value={String(form.uploadLimitMbps)}
                onChange={(event) => patch({ uploadLimitMbps: Number(event.target.value) })}
              />
            )}
          </Field>
        </div>

        <div className="space-y-1.5">
          <span className="block text-xs font-medium tracking-wide text-slate-400">Compression</span>
          <Segmented
            label="Compress before transfer"
            value={form.compression}
            options={COMPRESSION_OPTIONS}
            onChange={(compression) => patch({ compression })}
          />
          <p className="text-xs leading-relaxed text-slate-500">
            {form.compression === 'zstd'
              ? 'Data is zstd-compressed before it is sent — less bandwidth and less stored data.'
              : 'Data is sent as it is read. Choose this only if your sources are already compressed or encrypted.'}
          </p>
        </div>

        <SectionNote>
          Changing either setting never invalidates what is already on a target, and neither weakens
          data reduction. Both take effect on the next run — no restart needed.
        </SectionNote>

        {error ? (
          <p className="rounded-lg border border-fail-500/30 bg-fail-500/10 px-3.5 py-2.5 text-xs text-fail-300">
            {error}
          </p>
        ) : null}

        <div className="flex flex-wrap items-center gap-3 border-t border-slate-800 pt-4">
          <Button
            type="submit"
            variant="primary"
            loading={saving}
            disabled={!dirty}
            icon={<Save className="size-4" aria-hidden />}
          >
            Save changes
          </Button>
          {dirty ? (
            <Button onClick={onDiscard} disabled={saving}>
              Discard
            </Button>
          ) : (
            <span className="flex items-center gap-1.5 text-xs text-slate-500">
              <Gauge className="size-3.5" aria-hidden />
              {form.uploadLimitMbps > 0
                ? `${form.uploadConcurrency} parallel uploads, capped at ${form.uploadLimitMbps} Mbps`
                : `${form.uploadConcurrency} parallel uploads, unthrottled`}
            </span>
          )}
        </div>
      </form>
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
          <p className="rounded-lg border border-fail-500/30 bg-fail-500/10 px-3.5 py-2.5 text-xs text-fail-300">
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
  const { setServerName, can, role } = useSession()
  const loader = useCallback(() => getSettings(), [])
  const { data, loading, error, reload } = useAsync(loader)

  const [form, setForm] = useState<Settings>(DEFAULT_SETTINGS)
  const [submitting, setSubmitting] = useState<
    'server' | 'notifications' | 'performance' | null
  >(null)
  const [formError, setFormError] = useState<string | null>(null)
  const [notifyError, setNotifyError] = useState<string | null>(null)
  const [perfError, setPerfError] = useState<string | null>(null)

  useEffect(() => {
    if (data) setForm(data)
  }, [data])

  const patch = (next: Partial<Settings>) => setForm((current) => ({ ...current, ...next }))

  const dirty =
    data !== null && (data.serverName !== form.serverName || data.concurrency !== form.concurrency)
  const notifyDirty =
    data !== null && (data.webhookUrl !== form.webhookUrl || data.notifyOn !== form.notifyOn)
  const perfDirty =
    data !== null &&
    (data.uploadConcurrency !== form.uploadConcurrency ||
      data.compression !== form.compression ||
      data.uploadLimitMbps !== form.uploadLimitMbps)

  /**
   * Both cards write through the single PUT the contract exposes, so every
   * save carries the complete settings object — never a partial that would
   * blank out the other card's values.
   */
  const persist = async (which: 'server' | 'notifications' | 'performance') => {
    setFormError(null)
    setNotifyError(null)
    setPerfError(null)
    const fail =
      which === 'server' ? setFormError : which === 'notifications' ? setNotifyError : setPerfError

    const serverName = form.serverName.trim()
    const webhookUrl = form.webhookUrl.trim()

    if (!serverName) return fail('Give this server a display name.')
    if (
      !Number.isInteger(form.concurrency) ||
      form.concurrency < 1 ||
      form.concurrency > MAX_CONCURRENCY
    ) {
      return fail(`Concurrency must be a whole number between 1 and ${MAX_CONCURRENCY}.`)
    }
    if (webhookUrl && !/^https?:\/\//i.test(webhookUrl)) {
      return fail('The webhook URL must start with http:// or https://.')
    }
    if (!webhookUrl && form.notifyOn !== 'off') {
      return fail('Add a webhook URL, or set notifications to Off.')
    }
    if (
      !Number.isInteger(form.uploadConcurrency) ||
      form.uploadConcurrency < 1 ||
      form.uploadConcurrency > MAX_UPLOAD_CONCURRENCY
    ) {
      return fail(`Parallel transfers must be a whole number between 1 and ${MAX_UPLOAD_CONCURRENCY}.`)
    }
    if (
      !Number.isInteger(form.uploadLimitMbps) ||
      form.uploadLimitMbps < 0 ||
      form.uploadLimitMbps > MAX_UPLOAD_LIMIT
    ) {
      return fail(`The upload limit must be a whole number between 0 and ${MAX_UPLOAD_LIMIT} Mbps.`)
    }

    setSubmitting(which)
    try {
      const saved = await putSettings({
        serverName,
        concurrency: form.concurrency,
        webhookUrl,
        notifyOn: form.notifyOn,
        uploadConcurrency: form.uploadConcurrency,
        compression: form.compression,
        uploadLimitMbps: form.uploadLimitMbps,
      })
      setForm(saved)
      setServerName(saved.serverName)
      toast.success(
        which === 'server'
          ? 'Settings saved.'
          : which === 'notifications'
            ? 'Notification settings saved.'
            : 'Performance settings saved — they apply to the next run.',
      )
      await reload()
    } catch (err) {
      const message = errorMessage(err)
      fail(message)
      toast.error('Could not save settings', message)
    } finally {
      setSubmitting(null)
    }
  }

  const onSubmit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    void persist('server')
  }

  /**
   * Server settings are administrator-only and the server enforces that. Rather
   * than render four cards whose every control is refused, the page says out
   * loud that they exist and are not yours — and keeps the one card that is:
   * your own password, which every role may change.
   */
  if (!can.manageInfrastructure) {
    return (
      <>
        <PageHeader
          title="Settings"
          description="Your password. Server settings belong to administrators."
        />
        <SectionNote>
          Server identity, throughput, notifications and software updates are administrator-only on
          this server. You are signed in as {ROLE_LABEL[role].toLowerCase()}, so they are not shown
          — ask an administrator if something there needs changing.
        </SectionNote>
        <ChangePasswordCard />
      </>
    )
  }

  return (
    <>
      <PageHeader
        title="Settings"
        description="Server identity, throughput tuning, notifications, and your password."
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
          <form className="space-y-5 px-5 py-5" onSubmit={onSubmit} noValidate>
            <Field label="Server name" hint="Shown in the topbar and in notification subjects.">
              {({ id }) => (
                <Input
                  id={id}
                  value={form.serverName}
                  placeholder="ProxBack"
                  onChange={(event) => patch({ serverName: event.target.value })}
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
                  className="w-32 font-mono tabular-nums"
                  onChange={(event) => patch({ concurrency: Number(event.target.value) })}
                />
              )}
            </Field>

            <div className="flex items-baseline justify-between gap-4 rounded-lg border border-slate-800 bg-slate-950/40 px-4 py-2.5">
              <div className="min-w-0">
                <p className="eyebrow">Time zone</p>
                <p className="mt-0.5 text-meta text-slate-500">
                  Every job schedule fires in this zone. Set on the server, read-only here.
                </p>
              </div>
              <span className="shrink-0 font-mono text-[13px] text-slate-300">
                {data?.timezone || 'server local'}
              </span>
            </div>

            <SectionNote>
              Higher concurrency finishes large jobs sooner but puts more read pressure on your
              Proxmox storage and more upload pressure on your link. Two to four is a good starting
              point.
            </SectionNote>

            {formError ? (
              <p className="rounded-lg border border-fail-500/30 bg-fail-500/10 px-3.5 py-2.5 text-xs text-fail-300">
                {formError}
              </p>
            ) : null}

            <div className="flex items-center gap-3 border-t border-slate-800 pt-4">
              <Button
                type="submit"
                variant="primary"
                loading={submitting === 'server'}
                disabled={!dirty}
                icon={<Save className="size-4" aria-hidden />}
              >
                Save changes
              </Button>
              {dirty ? (
                <Button
                  onClick={() => data && patch({ serverName: data.serverName, concurrency: data.concurrency })}
                  disabled={submitting !== null}
                >
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

      {data ? (
        <PerformanceCard
          form={form}
          patch={patch}
          dirty={perfDirty}
          saving={submitting === 'performance'}
          error={perfError}
          onSave={() => void persist('performance')}
          onDiscard={() =>
            patch({
              uploadConcurrency: data.uploadConcurrency,
              compression: data.compression,
              uploadLimitMbps: data.uploadLimitMbps,
            })
          }
        />
      ) : null}

      {data ? (
        <NotificationsCard
          form={form}
          patch={patch}
          dirty={notifyDirty}
          savedWebhookUrl={data.webhookUrl}
          saving={submitting === 'notifications'}
          error={notifyError}
          onSave={() => void persist('notifications')}
          onDiscard={() => patch({ webhookUrl: data.webhookUrl, notifyOn: data.notifyOn })}
        />
      ) : null}

      <ChangePasswordCard />
    </>
  )
}
