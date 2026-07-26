import { useCallback, useState } from 'react'
import type { FormEvent } from 'react'
import { Database, Plug, Plus, RefreshCw, Trash2 } from 'lucide-react'
import { createTarget, deleteTarget, errorMessage, listTargets, testTarget } from '../api'
import type { ID, Target, TargetCreate } from '../api'
import { Modal } from '../components/Modal'
import { useConfirm } from '../components/Confirm'
import { useToast } from '../components/Toast'
import {
  Button,
  Card,
  Checkbox,
  EmptyState,
  ErrorBlock,
  Field,
  IconButton,
  Input,
  Metric,
  PageHeader,
  SectionNote,
  SkeletonCards,
  StatusPill,
  toneForStatus,
} from '../components/ui'
import { useAsync } from '../lib/useAsync'

const EMPTY_FORM: TargetCreate = {
  name: '',
  endpoint: '',
  region: '',
  bucket: '',
  accessKey: '',
  secretKey: '',
  pathStyle: false,
}

function AddTargetModal({
  open,
  onClose,
  onCreated,
}: {
  open: boolean
  onClose: () => void
  onCreated: () => void
}) {
  const toast = useToast()
  const [form, setForm] = useState<TargetCreate>(EMPTY_FORM)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const patch = (next: Partial<TargetCreate>) => setForm((current) => ({ ...current, ...next }))

  const close = () => {
    setForm(EMPTY_FORM)
    setError(null)
    onClose()
  }

  const onSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    setError(null)

    if (!form.name.trim()) return setError('Give the target a display name.')
    if (!form.endpoint.trim()) return setError('Enter the S3 endpoint URL.')
    if (!form.bucket.trim()) return setError('Enter the bucket name.')
    if (!form.accessKey.trim()) return setError('Enter the access key.')
    if (!form.secretKey.trim()) return setError('Enter the secret key.')

    setSubmitting(true)
    try {
      const target = await createTarget({
        name: form.name.trim(),
        endpoint: form.endpoint.trim().replace(/\/+$/, ''),
        region: form.region.trim(),
        bucket: form.bucket.trim(),
        accessKey: form.accessKey.trim(),
        secretKey: form.secretKey,
        pathStyle: form.pathStyle,
      })
      toast.success(`Target “${target.name}” added.`, 'Verifying bucket access…')
      close()

      try {
        const test = await testTarget(target.id)
        if (test.ok) {
          toast.success(`${target.name} is writable.`, 'Put, get, and delete probe succeeded.')
        } else {
          toast.error(`${target.name} probe failed`, test.error ?? 'The bucket rejected the keys.')
        }
      } catch (err) {
        toast.error('Test failed', errorMessage(err))
      } finally {
        onCreated()
      }
    } catch (err) {
      const message = errorMessage(err)
      setError(message)
      toast.error('Could not add target', message)
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Modal
      open={open}
      onClose={close}
      title="Add storage target"
      subtitle="Any S3-compatible bucket — Backblaze B2, MinIO, or AWS S3."
      width="lg"
      footer={
        <>
          <Button onClick={close}>Cancel</Button>
          <Button
            variant="primary"
            form="add-target-form"
            type="submit"
            loading={submitting}
            icon={<Plus className="size-4" aria-hidden />}
          >
            Add target
          </Button>
        </>
      }
    >
      <form
        id="add-target-form"
        className="space-y-4"
        onSubmit={(event) => void onSubmit(event)}
        noValidate
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Display name">
            {({ id }) => (
              <Input
                id={id}
                value={form.name}
                autoFocus
                placeholder="b2-offsite"
                onChange={(event) => patch({ name: event.target.value })}
              />
            )}
          </Field>

          <Field label="Endpoint" hint="Full URL including scheme.">
            {({ id }) => (
              <Input
                id={id}
                value={form.endpoint}
                placeholder="https://s3.eu-central-003.backblazeb2.com"
                onChange={(event) => patch({ endpoint: event.target.value })}
              />
            )}
          </Field>
        </div>

        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Region" hint="Leave blank if your provider ignores it.">
            {({ id }) => (
              <Input
                id={id}
                value={form.region}
                placeholder="eu-central-003"
                onChange={(event) => patch({ region: event.target.value })}
              />
            )}
          </Field>

          <Field label="Bucket">
            {({ id }) => (
              <Input
                id={id}
                value={form.bucket}
                placeholder="proxback-backups"
                onChange={(event) => patch({ bucket: event.target.value })}
              />
            )}
          </Field>
        </div>

        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Access key">
            {({ id }) => (
              <Input
                id={id}
                value={form.accessKey}
                autoComplete="off"
                onChange={(event) => patch({ accessKey: event.target.value })}
              />
            )}
          </Field>

          <Field label="Secret key" hint="Encrypted at rest; never returned by the API.">
            {({ id }) => (
              <Input
                id={id}
                type="password"
                value={form.secretKey}
                autoComplete="off"
                onChange={(event) => patch({ secretKey: event.target.value })}
              />
            )}
          </Field>
        </div>

        <Checkbox
          label="Use path-style addressing"
          hint="Required for Backblaze B2 / MinIO."
          checked={form.pathStyle}
          onChange={(checked) => patch({ pathStyle: checked })}
        />

        {error ? (
          <p className="rounded-lg border border-red-500/30 bg-red-500/10 px-3.5 py-2.5 text-xs text-red-300">
            {error}
          </p>
        ) : (
          <SectionNote>
            After the target is created, ProxBack writes, reads, and deletes a small probe object to
            prove the credentials work. Chunks are deduplicated per target, so keep one bucket per
            target.
          </SectionNote>
        )}
      </form>
    </Modal>
  )
}

function TargetCard({ target, onChanged }: { target: Target; onChanged: () => void }) {
  const toast = useToast()
  const confirm = useConfirm()
  const [testing, setTesting] = useState(false)
  const [deleting, setDeleting] = useState(false)

  const onTest = async () => {
    setTesting(true)
    try {
      const result = await testTarget(target.id)
      if (result.ok) {
        toast.success(`${target.name} is writable.`, 'Put, get, and delete probe succeeded.')
      } else {
        toast.error(`${target.name} probe failed`, result.error ?? 'The bucket rejected the keys.')
      }
      onChanged()
    } catch (err) {
      toast.error('Test failed', errorMessage(err))
    } finally {
      setTesting(false)
    }
  }

  const onDelete = async () => {
    const ok = await confirm({
      title: 'Remove storage target',
      message: (
        <>
          Remove <span className="font-medium text-slate-100">{target.name}</span>? Jobs pointing at
          this target stop working, and its restore points are no longer reachable from ProxBack.
          Objects already in the bucket are not deleted.
        </>
      ),
      confirmLabel: 'Remove target',
    })
    if (!ok) return

    setDeleting(true)
    try {
      await deleteTarget(target.id)
      toast.success(`Target “${target.name}” removed.`)
      onChanged()
    } catch (err) {
      toast.error('Could not remove target', errorMessage(err))
    } finally {
      setDeleting(false)
    }
  }

  return (
    <Card className="flex flex-col p-5">
      <div className="flex items-start justify-between gap-3">
        <div className="flex min-w-0 items-start gap-3">
          <div className="flex size-9 shrink-0 items-center justify-center rounded-lg border border-slate-800 bg-slate-950/60 text-accent-400">
            <Database className="size-4" aria-hidden />
          </div>
          <div className="min-w-0">
            <p className="truncate text-sm font-semibold text-slate-100">{target.name}</p>
            <p className="truncate text-xs text-slate-500">{target.endpoint}</p>
          </div>
        </div>
        <StatusPill tone={toneForStatus(target.status)} label={target.status || 'unknown'} />
      </div>

      <dl className="mt-5 grid grid-cols-2 gap-4">
        <Metric label="Bucket" value={target.bucket} />
        <Metric label="Region" value={target.region || '—'} />
        <Metric label="Addressing" value={target.pathStyle ? 'Path-style' : 'Virtual-hosted'} />
      </dl>

      <div className="mt-5 flex items-center gap-2 border-t border-slate-800 pt-4">
        <Button
          size="sm"
          onClick={() => void onTest()}
          loading={testing}
          icon={<Plug className="size-3.5" aria-hidden />}
        >
          Test
        </Button>
        <IconButton
          variant="danger"
          aria-label={`Remove ${target.name}`}
          title="Remove target"
          className="ml-auto"
          loading={deleting}
          onClick={() => void onDelete()}
        >
          <Trash2 className="size-4" aria-hidden />
        </IconButton>
      </div>
    </Card>
  )
}

export function TargetsPage() {
  const loader = useCallback(() => listTargets(), [])
  const { data, loading, error, reload, refresh } = useAsync(loader)
  const [addOpen, setAddOpen] = useState(false)

  const seen = new Set<ID>()
  const targets = (data ?? []).filter((target) => {
    if (seen.has(target.id)) return false
    seen.add(target.id)
    return true
  })

  return (
    <>
      <PageHeader
        title="Storage Targets"
        description="S3-compatible buckets that hold deduplicated chunks and manifests."
        actions={
          <>
            <Button
              icon={<RefreshCw className="size-4" aria-hidden />}
              onClick={() => void reload()}
              loading={loading}
            >
              Refresh
            </Button>
            <Button
              variant="primary"
              icon={<Plus className="size-4" aria-hidden />}
              onClick={() => setAddOpen(true)}
            >
              Add Target
            </Button>
          </>
        }
      />

      {loading && !data ? (
        <SkeletonCards count={3} height="h-56" />
      ) : error && !data ? (
        <ErrorBlock message={error} onRetry={() => void reload()} />
      ) : targets.length === 0 ? (
        <EmptyState
          icon={<Database className="size-5" aria-hidden />}
          title="No storage targets yet"
          description="Add a Backblaze B2, MinIO, or AWS S3 bucket. ProxBack stores 4 MiB deduplicated chunks plus one manifest per restore point, so a single bucket serves every job."
          action={
            <Button
              variant="primary"
              icon={<Plus className="size-4" aria-hidden />}
              onClick={() => setAddOpen(true)}
            >
              Add Target
            </Button>
          }
        />
      ) : (
        <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
          {targets.map((target) => (
            <TargetCard key={String(target.id)} target={target} onChanged={() => void refresh()} />
          ))}
        </div>
      )}

      <AddTargetModal
        open={addOpen}
        onClose={() => setAddOpen(false)}
        onCreated={() => void refresh()}
      />
    </>
  )
}
