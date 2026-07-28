/**
 * Storage Targets.
 *
 * v0.6.0 splits a target into two kinds, and the choice comes *first* — the
 * fields for a NAS path and the fields for an S3 bucket have nothing in common,
 * so showing both at once only invites filling in the wrong half.
 *
 * The filesystem form exists to set expectations, because this is exactly where
 * NAS setups go wrong: ProxBack writes to a path, not to a protocol, and the
 * share has to be mounted by the operating system before a path means anything.
 * The connection test then reports what it found, and a warning ("this is not a
 * mount point") is rendered as amber information rather than as a red failure —
 * the target works; something about it will bite later.
 */

import { useCallback, useState } from 'react'
import type { FormEvent } from 'react'
import {
  Cloud,
  Database,
  FolderTree,
  HardDrive,
  Info,
  Plug,
  Plus,
  RefreshCw,
  Trash2,
  TriangleAlert,
} from 'lucide-react'
import {
  capacityOf,
  createTarget,
  deleteTarget,
  describeTargetWarning,
  errorMessage,
  listTargets,
  LOW_SPACE_PCT,
  TARGET_KIND_LABEL,
  TARGET_KIND_SUMMARY,
  targetLocation,
  testTarget,
} from '../api'
import type {
  FilesystemTargetCreate,
  ID,
  S3TargetCreate,
  Target,
  TargetKind,
  TargetTestResult,
} from '../api'
import { Modal } from '../components/Modal'
import { useConfirm } from '../components/Confirm'
import { useToast } from '../components/Toast'
import { TARGET_KIND_ICON, TargetKindChip } from '../components/TargetIdentity'
import {
  Button,
  Card,
  Checkbox,
  ChoiceTile,
  Disclosure,
  EmptyState,
  ErrorBlock,
  Field,
  Hint,
  IconButton,
  Input,
  Metric,
  Mono,
  Num,
  PageHeader,
  ProgressBar,
  SectionNote,
  SkeletonCards,
  StatusPill,
  toneForStatus,
} from '../components/ui'
import { useAsync } from '../lib/useAsync'
import { formatBytes, formatPct } from '../lib/format'
import { roleDeniedReason, useSession } from '../session'

/**
 * Used and free space on a filesystem target, as a bar and as figures.
 *
 * This is what makes a local target feel real: an S3 bucket is elastic and a
 * NAS is not, so a target that is nearly full has to say so before a run
 * discovers it. Amber under 10% free — at risk, not failed.
 */
function Capacity({ target }: { target: Target }) {
  const capacity = capacityOf(target)
  if (!capacity) return null

  return (
    <div className="mt-4">
      <div className="flex items-baseline justify-between gap-3">
        <span className="eyebrow">Capacity</span>
        <span className={capacity.low ? 'text-meta text-warn-300' : 'text-meta text-slate-500'}>
          <Num>{formatBytes(capacity.freeBytes)}</Num> free
        </span>
      </div>
      <ProgressBar
        className="mt-1.5"
        value={capacity.usedPct}
        tone={capacity.low ? 'warn' : 'neutral'}
        ariaLabel={`${target.name}: ${formatPct(capacity.usedPct)} of ${formatBytes(
          capacity.totalBytes,
        )} used, ${formatBytes(capacity.freeBytes)} free`}
      />
      <p className="mt-1.5 text-meta text-slate-500">
        <Num>{formatBytes(capacity.usedBytes)}</Num> used of{' '}
        <Num>{formatBytes(capacity.totalBytes)}</Num> (<Num>{formatPct(capacity.usedPct)}</Num>)
      </p>
      {capacity.low ? (
        <p className="mt-2 flex items-start gap-2 rounded-lg border border-warn-500/30 bg-warn-500/10 px-2.5 py-2 text-meta leading-relaxed text-warn-200">
          <TriangleAlert className="mt-px size-3.5 shrink-0 text-warn-400" aria-hidden />
          <span>
            Under <Num>{LOW_SPACE_PCT}%</Num> free. Trim retention or add space before the next run
            needs it.
          </span>
        </p>
      ) : null}
    </div>
  )
}

/**
 * The outcome of a connection test.
 *
 * Failures and warnings are deliberately different objects on screen: red is
 * blocking and means the target cannot be used, amber is informational and
 * means it can. Collapsing the two is how "the NAS never mounted" gets read as
 * success.
 */
function TestOutcome({ result }: { result: TargetTestResult }) {
  const hasWarnings = result.warnings.length > 0

  return (
    <div className="mt-4 space-y-2">
      {result.ok ? (
        <div className="flex items-start gap-2 rounded-lg border border-slate-800 bg-slate-950/40 px-3 py-2 text-xs leading-relaxed text-slate-300">
          <Info className="mt-px size-3.5 shrink-0 text-slate-500" aria-hidden />
          <div className="min-w-0">
            <p className="font-medium text-slate-200">Writable.</p>
            <p className="mt-0.5 text-slate-400">
              {result.filesystemType ? (
                <>
                  Detected <Mono>{result.filesystemType}</Mono>
                  {result.totalBytes ? (
                    <>
                      {' · '}
                      <Num>{formatBytes(result.freeBytes)}</Num> free of{' '}
                      <Num>{formatBytes(result.totalBytes)}</Num>
                    </>
                  ) : null}
                </>
              ) : (
                'Wrote, read back, and removed a probe.'
              )}
            </p>
          </div>
        </div>
      ) : (
        <div
          className="flex items-start gap-2 rounded-lg border border-fail-500/30 bg-fail-500/10 px-3 py-2 text-xs leading-relaxed text-fail-200"
          role="alert"
        >
          <TriangleAlert className="mt-px size-3.5 shrink-0 text-fail-400" aria-hidden />
          <div className="min-w-0">
            <p className="font-medium">Cannot be used</p>
            <p className="mt-0.5 break-words text-slate-400">
              {result.error || 'The target refused the probe.'}
            </p>
          </div>
        </div>
      )}

      {hasWarnings ? (
        <ul className="space-y-2">
          {result.warnings.map((warning) => (
            <li
              key={warning.code}
              className="flex items-start gap-2 rounded-lg border border-warn-500/30 bg-warn-500/10 px-3 py-2 text-xs leading-relaxed text-warn-200"
            >
              <TriangleAlert className="mt-px size-3.5 shrink-0 text-warn-400" aria-hidden />
              <div className="min-w-0">
                <p className="font-medium">Worth knowing</p>
                <p className="mt-0.5 break-words text-slate-400">
                  {describeTargetWarning(warning)}
                </p>
              </div>
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  )
}

/* ---------------------------------------------------------------------------
 * Add target — kind first, then only that kind's fields
 * ------------------------------------------------------------------------- */

interface S3Form {
  name: string
  endpoint: string
  region: string
  bucket: string
  accessKey: string
  secretKey: string
  pathStyle: boolean
}

interface FilesystemForm {
  name: string
  path: string
  allowSameFilesystem: boolean
}

const EMPTY_S3: S3Form = {
  name: '',
  endpoint: '',
  region: '',
  bucket: '',
  accessKey: '',
  secretKey: '',
  pathStyle: false,
}

const EMPTY_FS: FilesystemForm = { name: '', path: '', allowSameFilesystem: false }

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
  /** `null` until the kind is chosen — the choice is the first thing asked. */
  const [kind, setKind] = useState<TargetKind | null>(null)
  const [s3, setS3] = useState<S3Form>(EMPTY_S3)
  const [fs, setFs] = useState<FilesystemForm>(EMPTY_FS)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const close = () => {
    setKind(null)
    setS3(EMPTY_S3)
    setFs(EMPTY_FS)
    setError(null)
    onClose()
  }

  /** Runs the probe after creation and reports failures and warnings apart. */
  const probe = async (target: Target) => {
    try {
      const test = await testTarget(target.id)
      const firstWarning = test.warnings.length > 0 ? test.warnings[0] : undefined
      if (!test.ok) {
        toast.error(`${target.name} is not usable yet`, test.error ?? 'The probe was refused.')
      } else if (firstWarning) {
        toast.info(`${target.name} is writable, with warnings`, describeTargetWarning(firstWarning))
      } else {
        toast.success(`${target.name} is writable.`)
      }
    } catch (err) {
      toast.error('Test failed', errorMessage(err))
    } finally {
      onCreated()
    }
  }

  const submitS3 = async () => {
    if (!s3.name.trim()) return setError('Give the target a display name.')
    if (!s3.endpoint.trim()) return setError('Enter the S3 endpoint URL.')
    if (!s3.bucket.trim()) return setError('Enter the bucket name.')
    if (!s3.accessKey.trim()) return setError('Enter the access key.')
    if (!s3.secretKey.trim()) return setError('Enter the secret key.')

    const payload: S3TargetCreate = {
      kind: 's3',
      name: s3.name.trim(),
      endpoint: s3.endpoint.trim().replace(/\/+$/, ''),
      region: s3.region.trim(),
      bucket: s3.bucket.trim(),
      accessKey: s3.accessKey.trim(),
      secretKey: s3.secretKey,
      pathStyle: s3.pathStyle,
    }
    const target = await createTarget(payload)
    toast.success(`Target “${target.name}” added.`, 'Checking bucket access…')
    close()
    await probe(target)
  }

  const submitFilesystem = async () => {
    if (!fs.name.trim()) return setError('Give the target a display name.')
    const path = fs.path.trim().replace(/(?!^)[/\\]+$/, '')
    if (!path) return setError('Enter the directory ProxBack should write into.')
    if (!/^([/~]|[A-Za-z]:[/\\]|\\\\)/.test(path)) {
      return setError('Enter an absolute path, for example /mnt/nas/proxback.')
    }

    const payload: FilesystemTargetCreate = {
      kind: 'filesystem',
      name: fs.name.trim(),
      path,
      allowSameFilesystem: fs.allowSameFilesystem,
    }
    const target = await createTarget(payload)
    toast.success(`Target “${target.name}” added.`, 'Checking the path…')
    close()
    await probe(target)
  }

  const onSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    setError(null)
    setSubmitting(true)
    try {
      if (kind === 'filesystem') await submitFilesystem()
      else await submitS3()
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
      subtitle={
        kind === null
          ? 'Back up locally, offsite, or both — pick where this target writes.'
          : TARGET_KIND_LABEL[kind]
      }
      width="lg"
      footer={
        kind === null ? (
          <Button onClick={close}>Cancel</Button>
        ) : (
          <>
            <Button onClick={() => setKind(null)}>Back</Button>
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
        )
      }
    >
      {kind === null ? (
        <div className="space-y-3">
          <p className="text-xs font-medium text-slate-400">What kind of storage is this?</p>
          <div
            role="radiogroup"
            aria-label="Kind of storage target"
            className="grid gap-3 sm:grid-cols-2"
          >
            <ChoiceTile
              selected={false}
              onSelect={() => setKind('filesystem')}
              icon={<HardDrive className="size-4" aria-hidden />}
              title={TARGET_KIND_LABEL.filesystem}
              description={TARGET_KIND_SUMMARY.filesystem}
            />
            <ChoiceTile
              selected={false}
              onSelect={() => setKind('s3')}
              icon={<Cloud className="size-4" aria-hidden />}
              title={TARGET_KIND_LABEL.s3}
              description={TARGET_KIND_SUMMARY.s3}
            />
          </div>
          <SectionNote>
            Most estates want one of each: a local target for fast recovery, and an offsite bucket
            for the copy that survives the building.
          </SectionNote>
        </div>
      ) : (
        <form
          id="add-target-form"
          className="space-y-4"
          onSubmit={(event) => void onSubmit(event)}
          noValidate
        >
          {kind === 'filesystem' ? (
            <>
              <SectionNote>
                ProxBack writes to a <span className="font-medium text-slate-200">path</span>, not to
                a protocol. Mount the NAS share with the operating system first — an{' '}
                <span className="font-medium text-slate-200">/etc/fstab</span> entry or{' '}
                <span className="font-medium text-slate-200">autofs</span> — then point this target
                at the mount path. NFS, SMB/CIFS, iSCSI, a USB disk and a ZFS dataset all work this
                way, and the operating system keeps the credentials, retries and caching.
              </SectionNote>

              <div className="grid gap-4 sm:grid-cols-2">
                <Field label="Display name">
                  {({ id }) => (
                    <Input
                      id={id}
                      value={fs.name}
                      autoFocus
                      placeholder="nas-primary"
                      onChange={(event) => setFs({ ...fs, name: event.target.value })}
                    />
                  )}
                </Field>

                <Field
                  label="Directory"
                  hint="An absolute path that already exists on this server."
                >
                  {({ id, describedBy }) => (
                    <Input
                      id={id}
                      aria-describedby={describedBy}
                      value={fs.path}
                      placeholder="/mnt/nas/proxback"
                      spellCheck={false}
                      autoComplete="off"
                      onChange={(event) => setFs({ ...fs, path: event.target.value })}
                    />
                  )}
                </Field>
              </div>

              <Hint>
                The connection test checks that the path exists, is a directory and is writable, and
                tells you whether it is really a mount point — the usual reason backups end up on
                the wrong disk.
              </Hint>

              <Disclosure summary="Advanced" hint="Rarely needed">
                <Checkbox
                  label="Allow this path on ProxBack’s own filesystem"
                  hint="Normally refused: if the disk holding ProxBack fails, it takes both the server and the only copy of your backups with it. Tick this only for a deliberate test or a lab."
                  checked={fs.allowSameFilesystem}
                  onChange={(checked) => setFs({ ...fs, allowSameFilesystem: checked })}
                />
              </Disclosure>
            </>
          ) : (
            <>
              <div className="grid gap-4 sm:grid-cols-2">
                <Field label="Display name">
                  {({ id }) => (
                    <Input
                      id={id}
                      value={s3.name}
                      autoFocus
                      placeholder="b2-offsite"
                      onChange={(event) => setS3({ ...s3, name: event.target.value })}
                    />
                  )}
                </Field>

                <Field label="Endpoint" hint="Full URL including scheme.">
                  {({ id, describedBy }) => (
                    <Input
                      id={id}
                      aria-describedby={describedBy}
                      value={s3.endpoint}
                      placeholder="https://s3.eu-central-003.backblazeb2.com"
                      onChange={(event) => setS3({ ...s3, endpoint: event.target.value })}
                    />
                  )}
                </Field>
              </div>

              <div className="grid gap-4 sm:grid-cols-2">
                <Field label="Region" hint="Leave blank if your provider ignores it.">
                  {({ id, describedBy }) => (
                    <Input
                      id={id}
                      aria-describedby={describedBy}
                      value={s3.region}
                      placeholder="eu-central-003"
                      onChange={(event) => setS3({ ...s3, region: event.target.value })}
                    />
                  )}
                </Field>

                <Field label="Bucket">
                  {({ id }) => (
                    <Input
                      id={id}
                      value={s3.bucket}
                      placeholder="proxback-backups"
                      onChange={(event) => setS3({ ...s3, bucket: event.target.value })}
                    />
                  )}
                </Field>
              </div>

              <div className="grid gap-4 sm:grid-cols-2">
                <Field label="Access key">
                  {({ id }) => (
                    <Input
                      id={id}
                      value={s3.accessKey}
                      autoComplete="off"
                      onChange={(event) => setS3({ ...s3, accessKey: event.target.value })}
                    />
                  )}
                </Field>

                <Field label="Secret key" hint="Encrypted at rest; never returned by the API.">
                  {({ id, describedBy }) => (
                    <Input
                      id={id}
                      aria-describedby={describedBy}
                      type="password"
                      value={s3.secretKey}
                      autoComplete="off"
                      onChange={(event) => setS3({ ...s3, secretKey: event.target.value })}
                    />
                  )}
                </Field>
              </div>

              <Checkbox
                label="Use path-style addressing"
                hint="Required for Backblaze B2 / MinIO."
                checked={s3.pathStyle}
                onChange={(checked) => setS3({ ...s3, pathStyle: checked })}
              />
            </>
          )}

          {error ? (
            <p
              className="rounded-lg border border-fail-500/30 bg-fail-500/10 px-3.5 py-2.5 text-xs text-fail-300"
              role="alert"
            >
              {error}
            </p>
          ) : (
            <Hint>
              Data reduction happens per target, so keep one target per bucket or directory and point
              as many jobs at it as you like.
            </Hint>
          )}
        </form>
      )}
    </Modal>
  )
}

/* ---------------------------------------------------------------------------
 * Target card
 * ------------------------------------------------------------------------- */

function TargetCard({ target, onChanged }: { target: Target; onChanged: () => void }) {
  const toast = useToast()
  const confirm = useConfirm()
  const { can, role } = useSession()
  const [testing, setTesting] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const [result, setResult] = useState<TargetTestResult | null>(null)

  const filesystem = target.kind === 'filesystem'
  const Icon = TARGET_KIND_ICON[target.kind]
  const denied = can.manageInfrastructure ? undefined : roleDeniedReason(role, 'change storage targets')

  const onTest = async () => {
    setTesting(true)
    try {
      const test = await testTarget(target.id)
      setResult(test)
      if (!test.ok) {
        toast.error(`${target.name} is not usable`, test.error ?? 'The probe was refused.')
      } else if (test.warnings.length > 0) {
        toast.info(`${target.name} is writable, with warnings`, 'See the target card for details.')
      } else {
        toast.success(`${target.name} is writable.`)
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
          this target stop working, and its restore points are no longer reachable from ProxBack.{' '}
          {filesystem
            ? 'Files already written to the directory are left alone.'
            : 'Objects already in the bucket are not deleted.'}
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
            <Icon className="size-4" aria-hidden />
          </div>
          <div className="min-w-0">
            <p className="truncate text-sm font-semibold text-slate-100">{target.name}</p>
            <div className="mt-1 flex flex-wrap items-center gap-1.5">
              <TargetKindChip kind={target.kind} />
              <span className="truncate font-mono text-meta text-slate-500" title={targetLocation(target)}>
                {targetLocation(target)}
              </span>
            </div>
          </div>
        </div>
        <StatusPill tone={toneForStatus(target.status)} label={target.status || 'unknown'} />
      </div>

      {filesystem ? (
        <>
          {/* Written out rather than truncated: a wrong path is the whole bug
              class this feature has, and it is unreadable cut off. */}
          <dl className="mt-5">
            <dt className="eyebrow">Directory</dt>
            <dd className="mt-0.5 font-mono text-xs break-all text-slate-300">
              {target.path || '—'}
            </dd>
          </dl>
          <Capacity target={target} />
          {target.allowSameFilesystem ? (
            <p className="mt-3 flex items-start gap-2 rounded-lg border border-warn-500/30 bg-warn-500/10 px-2.5 py-2 text-meta leading-relaxed text-warn-200">
              <TriangleAlert className="mt-px size-3.5 shrink-0 text-warn-400" aria-hidden />
              <span>
                Allowed on ProxBack’s own filesystem. A disk failure takes the server and these
                backups together.
              </span>
            </p>
          ) : null}
        </>
      ) : (
        <dl className="mt-5 grid grid-cols-2 gap-4">
          <Metric label="Bucket" value={target.bucket || '—'} />
          <Metric label="Region" value={target.region || '—'} />
          <Metric label="Addressing" value={target.pathStyle ? 'Path-style' : 'Virtual-hosted'} />
        </dl>
      )}

      {result ? <TestOutcome result={result} /> : null}

      <div className="mt-5 flex items-center gap-2 border-t border-slate-800 pt-4">
        <Button
          size="sm"
          onClick={() => void onTest()}
          loading={testing}
          disabled={!can.manageInfrastructure}
          title={denied}
          aria-label={denied ? `Test ${target.name} — ${denied}` : `Test ${target.name}`}
          icon={<Plug className="size-3.5" aria-hidden />}
        >
          Test
        </Button>
        <IconButton
          variant="dangerQuiet"
          aria-label={
            denied ? `Remove ${target.name} — ${denied}` : `Remove ${target.name}`
          }
          title={denied ?? 'Remove target'}
          className="ml-auto"
          loading={deleting}
          disabled={!can.manageInfrastructure}
          onClick={() => void onDelete()}
        >
          <Trash2 className="size-4" aria-hidden />
        </IconButton>
      </div>
    </Card>
  )
}

/* ---------------------------------------------------------------------------
 * Page
 * ------------------------------------------------------------------------- */

export function TargetsPage() {
  const loader = useCallback(() => listTargets(), [])
  const { data, loading, error, reload, refresh } = useAsync(loader)
  const [addOpen, setAddOpen] = useState(false)
  const { can, role } = useSession()

  const seen = new Set<ID>()
  const targets = (data ?? []).filter((target) => {
    if (seen.has(target.id)) return false
    seen.add(target.id)
    return true
  })

  const local = targets.filter((target) => target.kind === 'filesystem').length
  const offsite = targets.length - local
  const denied = can.manageInfrastructure
    ? undefined
    : roleDeniedReason(role, 'add storage targets')

  const addButton = (
    <Button
      variant="primary"
      icon={<Plus className="size-4" aria-hidden />}
      onClick={() => setAddOpen(true)}
      disabled={!can.manageInfrastructure}
      title={denied}
      aria-label={denied ? `Add Target — ${denied}` : undefined}
    >
      Add Target
    </Button>
  )

  return (
    <>
      <PageHeader
        title="Storage Targets"
        description="Where backups are written: a local or network path such as a NAS, an S3-compatible bucket offsite, or both."
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
        <SkeletonCards count={3} height="h-56" />
      ) : error && !data ? (
        <ErrorBlock message={error} onRetry={() => void reload()} />
      ) : targets.length === 0 ? (
        <EmptyState
          icon={<Database className="size-5" aria-hidden />}
          title="No storage targets yet"
          description="Nothing can be backed up until there is somewhere to write. Point a target at a mounted NAS or local disk, at an S3-compatible bucket, or at one of each."
          action={addButton}
          hint="Backups are reduced before they are written, so one target comfortably serves every job."
        />
      ) : (
        <>
          {offsite === 0 ? (
            <div className="mb-4 flex items-start gap-2.5 rounded-xl border border-slate-800 bg-slate-900/40 px-4 py-3 text-[13px] leading-relaxed text-slate-400">
              <Info className="mt-0.5 size-4 shrink-0 text-slate-500" aria-hidden />
              <span>
                Every target here is on local or network storage. A fire, a theft or a
                ransomware event reaches all of them at once — add an offsite bucket for the copy
                that leaves the building.
              </span>
            </div>
          ) : null}
          {local === 0 ? (
            <div className="mb-4 flex items-start gap-2.5 rounded-xl border border-slate-800 bg-slate-900/40 px-4 py-3 text-[13px] leading-relaxed text-slate-400">
              <FolderTree className="mt-0.5 size-4 shrink-0 text-slate-500" aria-hidden />
              <span>
                Every target here is offsite. A local target on a NAS or a spare disk restores far
                faster when the problem is one broken guest rather than a lost site.
              </span>
            </div>
          ) : null}
          <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
            {targets.map((target) => (
              <TargetCard key={String(target.id)} target={target} onChanged={() => void refresh()} />
            ))}
          </div>
        </>
      )}

      <AddTargetModal
        open={addOpen}
        onClose={() => setAddOpen(false)}
        onCreated={() => void refresh()}
      />
    </>
  )
}
