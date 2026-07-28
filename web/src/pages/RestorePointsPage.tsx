import { useCallback, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import {
  Database,
  History,
  Laptop,
  RefreshCw,
  RotateCcw,
  ShieldCheck,
  Trash2,
} from 'lucide-react'
import {
  deleteBackup,
  errorMessage,
  listAgents,
  listBackups,
  listHosts,
  listTargets,
  listVMs,
  sourceVMID,
  targetLocation,
  verifyBackup,
} from '../api'
import type { Agent, Backup, CachedVM, Host, ID, SourceKind, Target } from '../api'
import { useConfirm } from '../components/Confirm'
import { WorkloadIdentity, identityText } from '../components/Identity'
import { RestoreWizard } from '../components/RestoreWizard'
import { TargetKindChip } from '../components/TargetIdentity'
import { useToast } from '../components/Toast'
import {
  Button,
  Card,
  CardHeader,
  EmptyState,
  ErrorBlock,
  IconButton,
  LoadingBlock,
  Num,
  PageHeader,
  SkeletonRows,
  StatusPill,
} from '../components/ui'
import { cn } from '../lib/cn'
import { useAsync } from '../lib/useAsync'
import { formatBytes, formatCount, formatDateTime, formatRelative } from '../lib/format'
import { roleDeniedReason, useSession } from '../session'

interface RestoreData {
  backups: Backup[]
  hosts: Host[]
  agents: Agent[]
  vms: CachedVM[]
  targets: Target[]
}

/**
 * Sources group by (kind, cluster, id) rather than by name: two clusters can
 * each hold a `db-01`, and merging them would attribute one estate's restore
 * points to the other.
 */
interface SourceGroup {
  key: string
  sourceKind: SourceKind
  sourceId: ID
  sourceName: string
  hostId?: ID
  hostName?: string
  node?: string
  count: number
  latest: string
  totalBytes: number
  /** Newest passing integrity verification anywhere in this chain. */
  lastVerifiedAt: string | null
}

function sourceKey(backup: Backup): string {
  return `${backup.sourceKind}:${String(backup.hostId ?? '')}:${String(backup.sourceId)}`
}

function newer(a: string | null | undefined, b: string | null | undefined): string | null {
  if (!a) return b ?? null
  if (!b) return a
  return new Date(a).getTime() >= new Date(b).getTime() ? a : b
}

function groupSources(backups: Backup[]): SourceGroup[] {
  const groups = new Map<string, SourceGroup>()
  for (const backup of backups) {
    const key = sourceKey(backup)
    const verified = backup.lastVerifyResult === 'failed' ? null : (backup.lastVerifiedAt ?? null)
    const existing = groups.get(key)
    if (existing) {
      existing.count += 1
      existing.totalBytes += backup.sizeBytes
      existing.lastVerifiedAt = newer(existing.lastVerifiedAt, verified)
      if (new Date(backup.createdAt).getTime() > new Date(existing.latest).getTime()) {
        existing.latest = backup.createdAt
      }
    } else {
      groups.set(key, {
        key,
        sourceKind: backup.sourceKind,
        sourceId: backup.sourceId,
        sourceName: backup.sourceName,
        hostId: backup.hostId,
        hostName: backup.hostName,
        node: backup.node,
        count: 1,
        totalBytes: backup.sizeBytes,
        latest: backup.createdAt,
        lastVerifiedAt: verified,
      })
    }
  }
  return [...groups.values()].sort(
    (a, b) => new Date(b.latest).getTime() - new Date(a.latest).getTime(),
  )
}

/** Depth of each backup in its full → incremental chain, for indentation. */
function chainDepths(backups: Backup[]): Map<string, number> {
  const byId = new Map<string, Backup>(backups.map((backup) => [String(backup.id), backup]))
  const depths = new Map<string, number>()

  const depthOf = (backup: Backup, guard: number): number => {
    const id = String(backup.id)
    const cached = depths.get(id)
    if (cached !== undefined) return cached
    const parentId = backup.parentId === null ? undefined : backup.parentId
    const parent = parentId === undefined ? undefined : byId.get(String(parentId))
    const depth = !parent || guard <= 0 ? 0 : Math.min(6, depthOf(parent, guard - 1) + 1)
    depths.set(id, depth)
    return depth
  }

  for (const backup of backups) depthOf(backup, backups.length)
  return depths
}

/**
 * Verification evidence for one point. "Integrity verified" means every block
 * was read back from the target and re-hashed. It is never presented as proof
 * that a restore was rehearsed — that is a different claim, and ProxBack does
 * not make it.
 */
function VerificationBadge({ backup }: { backup: Backup }) {
  if (backup.lastVerifyResult === 'failed') {
    return (
      <span className="inline-flex items-center gap-1.5">
        <StatusPill tone="fail" label="Integrity check failed" />
        {backup.lastVerifiedAt ? (
          <span className="text-micro text-slate-500">{formatRelative(backup.lastVerifiedAt)}</span>
        ) : null}
      </span>
    )
  }
  if (backup.lastVerifiedAt) {
    return (
      <span className="inline-flex items-center gap-1.5">
        <StatusPill tone="ok" label="Integrity verified" />
        <span className="text-micro text-slate-500">{formatRelative(backup.lastVerifiedAt)}</span>
      </span>
    )
  }
  return <StatusPill tone="neutral" label="Not verified" />
}

function BackupRow({
  backup,
  target,
  depth,
  onRestore,
  onDeleted,
}: {
  backup: Backup
  /** The target holding this point, when it is still configured. */
  target?: Target
  depth: number
  onRestore: () => void
  onDeleted: () => void
}) {
  const toast = useToast()
  const confirm = useConfirm()
  const navigate = useNavigate()
  const { can, role } = useSession()
  const [deleting, setDeleting] = useState(false)
  const [verifying, setVerifying] = useState(false)

  const denied = can.operateJobs ? undefined : roleDeniedReason(role, 'restore, verify or prune')

  const onVerify = async () => {
    setVerifying(true)
    try {
      await verifyBackup(backup.id)
      toast.success(
        'Verification started.',
        `The ${backup.kind} point from ${formatDateTime(backup.createdAt)} is being read back from the target and re-hashed.`,
        { label: 'View in Monitor', onClick: () => navigate('/monitor') },
      )
    } catch (err) {
      toast.error('Could not start verification', errorMessage(err))
    } finally {
      setVerifying(false)
    }
  }

  const onDelete = async () => {
    const ok = await confirm({
      title: 'Delete restore point',
      message: (
        <>
          Delete the {backup.kind} restore point of{' '}
          <span className="font-medium text-slate-100">{backup.sourceName}</span> from{' '}
          {formatDateTime(backup.createdAt)}? Storage it does not share with another point is
          reclaimed, and later incrementals in this chain may become unusable.
        </>
      ),
      confirmLabel: 'Delete restore point',
    })
    if (!ok) return

    setDeleting(true)
    try {
      await deleteBackup(backup.id)
      toast.success('Restore point deleted.')
      onDeleted()
    } catch (err) {
      toast.error('Could not delete restore point', errorMessage(err))
    } finally {
      setDeleting(false)
    }
  }

  return (
    <div className="flex flex-wrap items-center gap-4 px-5 py-3.5">
      <div
        className="flex min-w-0 flex-1 items-center gap-3"
        style={{ paddingLeft: `${Math.min(depth, 6) * 18}px` }}
      >
        {depth > 0 ? (
          <span
            className="-ml-4 h-6 w-3 shrink-0 rounded-bl-md border-b border-l border-slate-700"
            aria-hidden
          />
        ) : null}
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <p className="text-[13px] font-medium text-slate-100">
              {formatDateTime(backup.createdAt)}
            </p>
            <StatusPill tone="neutral" label={backup.kind === 'full' ? 'full' : 'incremental'} />
            <VerificationBadge backup={backup} />
            {/* Where this point physically lives: local storage restores fast,
                an offsite bucket has to come back down the wire first. */}
            {target ? <TargetKindChip kind={target.kind} /> : null}
          </div>
          <p className="mt-0.5 truncate text-xs text-slate-500">
            {target ? (
              <>
                On <span className="text-slate-400">{target.name}</span>{' '}
                <span className="font-mono">{targetLocation(target)}</span>
                {' · '}
              </>
            ) : null}
            <Num>{formatBytes(backup.sizeBytes)}</Num> source data ·{' '}
            <Num>{formatBytes(backup.uploadedBytes)}</Num> transferred to target
            {backup.disks.length > 0 ? (
              <>
                {' · '}
                <Num>{backup.disks.length}</Num> {backup.disks.length === 1 ? 'disk' : 'disks'}:{' '}
                {backup.disks.map((disk, index) => (
                  <span key={disk.name}>
                    {index > 0 ? ', ' : ''}
                    {disk.name} <Num>{formatBytes(disk.sizeBytes)}</Num>
                  </span>
                ))}
              </>
            ) : null}
          </p>
        </div>
      </div>

      <div className="flex shrink-0 items-center gap-2">
        <Button
          size="sm"
          variant="primary"
          icon={<RotateCcw className="size-3.5" aria-hidden />}
          disabled={!can.operateJobs}
          title={denied}
          aria-label={denied ? `Restore — ${denied}` : undefined}
          onClick={onRestore}
        >
          Restore
        </Button>
        <IconButton
          aria-label={
            denied
              ? `Verify the restore point from ${formatDateTime(backup.createdAt)} — ${denied}`
              : `Verify the restore point from ${formatDateTime(backup.createdAt)}`
          }
          title={
            denied ??
            'Verify integrity — read every block back from the target and re-check its hash'
          }
          disabled={!can.operateJobs}
          loading={verifying}
          onClick={() => void onVerify()}
        >
          <ShieldCheck className="size-4" aria-hidden />
        </IconButton>
        <IconButton
          variant="dangerQuiet"
          aria-label={
            denied
              ? `Delete the restore point from ${formatDateTime(backup.createdAt)} — ${denied}`
              : `Delete the restore point from ${formatDateTime(backup.createdAt)}`
          }
          title={denied ?? 'Delete restore point'}
          disabled={!can.operateJobs}
          loading={deleting}
          onClick={() => void onDelete()}
        >
          <Trash2 className="size-4" aria-hidden />
        </IconButton>
      </div>
    </div>
  )
}

export function RestorePointsPage() {
  const loader = useCallback(async (): Promise<RestoreData> => {
    const [backups, hosts, agents, vms, targets] = await Promise.all([
      listBackups(),
      listHosts(),
      listAgents(),
      listVMs(),
      listTargets(),
    ])
    return { backups, hosts, agents, vms, targets }
  }, [])
  const { data, loading, error, reload, refresh } = useAsync(loader)

  const [selectedKey, setSelectedKey] = useState<string | null>(null)
  const [restoring, setRestoring] = useState<Backup | null>(null)

  const groups = useMemo(() => groupSources(data?.backups ?? []), [data?.backups])
  /* Falling back to the first group means the page opens on the most recently
     protected workload instead of a placeholder, and a workload that is pruned
     away while selected hands over to a real chain rather than a blank panel. */
  const selected = useMemo(
    () => groups.find((group) => group.key === selectedKey) ?? groups[0] ?? null,
    [groups, selectedKey],
  )

  /* The list already carries every point, so the chain view filters it rather
     than issuing a second request — one source of truth, and no window where
     the two disagree after a delete. */
  const detail = useMemo(() => {
    if (!selected) return null
    return (data?.backups ?? [])
      .filter((backup) => sourceKey(backup) === selected.key)
      .sort((a, b) => new Date(b.createdAt).getTime() - new Date(a.createdAt).getTime())
  }, [data?.backups, selected])

  const depths = useMemo(() => chainDepths(detail ?? []), [detail])

  /**
   * The target a point lives on. A point can outlive the target record, so this
   * is allowed to come back undefined and the row simply says less.
   */
  const targetOf = (backup: Backup): Target | undefined =>
    (data?.targets ?? []).find((target) => String(target.id) === String(backup.targetId))

  return (
    <>
      <PageHeader
        title="Restore Points"
        description="Every recovery point on your targets, grouped by workload and shown as full → incremental chains."
        actions={
          <Button
            icon={<RefreshCw className="size-4" aria-hidden />}
            onClick={() => void reload()}
            loading={loading}
          >
            Refresh
          </Button>
        }
      />

      {loading && !data ? (
        <Card>
          <SkeletonRows count={5} />
        </Card>
      ) : error && !data ? (
        <ErrorBlock message={error} onRetry={() => void reload()} />
      ) : groups.length === 0 ? (
        <EmptyState
          icon={<History className="size-5" aria-hidden />}
          title="No restore points yet"
          description="A restore point appears here the first time a backup job succeeds. Until one does, nothing on this estate can be recovered."
          action={
            <Button variant="primary" onClick={() => void reload()}>
              Check again
            </Button>
          }
        />
      ) : (
        <div className="grid gap-4 lg:grid-cols-[22rem_1fr]">
          <Card className="h-fit lg:sticky lg:top-4">
            <CardHeader title="Workloads" subtitle={`${groups.length} with restore points`} />
            {/* A long estate scrolls inside the card rather than setting the
                height of the whole row, which would leave the chain panel
                beside it padded out with empty space. */}
            <ul className="divide-y divide-slate-800 lg:max-h-[calc(100vh-14rem)] lg:overflow-y-auto">
              {groups.map((group) => {
                const active = selected?.key === group.key
                return (
                  <li key={group.key}>
                    <button
                      type="button"
                      onClick={() => setSelectedKey(group.key)}
                      aria-pressed={active}
                      className={cn(
                        'flex w-full items-start gap-3 px-4 py-3 text-left transition-colors duration-150',
                        active ? 'bg-accent-500/10' : 'hover:bg-slate-800/40',
                      )}
                    >
                      <span
                        className={cn(
                          'mt-0.5 flex size-7 shrink-0 items-center justify-center rounded-lg border',
                          active
                            ? 'border-accent-500/40 bg-accent-500/15 text-accent-300'
                            : 'border-slate-800 bg-slate-950/60 text-slate-500',
                        )}
                      >
                        {group.sourceKind === 'vm' ? (
                          <Laptop className="size-3.5" aria-hidden />
                        ) : (
                          <ShieldCheck className="size-3.5" aria-hidden />
                        )}
                      </span>
                      <span className="min-w-0 flex-1">
                        <WorkloadIdentity
                          emphasis={active ? 'strong' : 'normal'}
                          workload={{
                            hostName: group.hostName,
                            name: group.sourceName,
                            vmid: group.sourceKind === 'vm' ? sourceVMID(group.sourceId) : null,
                            node: group.node,
                          }}
                        />
                        <span className="mt-1 block truncate text-meta text-slate-500">
                          <Num>{formatCount(group.count)}</Num>{' '}
                          {group.count === 1 ? 'point' : 'points'} ·{' '}
                          <Num>{formatBytes(group.totalBytes)}</Num> · newest{' '}
                          <Num>{formatRelative(group.latest)}</Num>
                        </span>
                        <span
                          className={cn(
                            'mt-0.5 block truncate text-meta',
                            group.lastVerifiedAt ? 'text-ok-400' : 'text-slate-600',
                          )}
                        >
                          {group.lastVerifiedAt
                            ? `Integrity verified ${formatRelative(group.lastVerifiedAt)}`
                            : 'No point verified yet'}
                        </span>
                      </span>
                    </button>
                  </li>
                )
              })}
            </ul>
          </Card>

          {/* `min-w-0` is load-bearing: a `1fr` track defaults to min-width
              auto, so without it the widest chain row sets the column width and
              the page scrolls sideways instead of the row truncating. */}
          <Card className="h-fit min-w-0">
            {!selected ? null : (
              <>
                <CardHeader
                  title={identityText({
                    hostName: selected.hostName,
                    name: selected.sourceName,
                    vmid: selected.sourceKind === 'vm' ? sourceVMID(selected.sourceId) : null,
                    node: selected.node,
                  })}
                  subtitle={
                    selected.sourceKind === 'vm'
                      ? 'Virtual machine — agentless image backup'
                      : 'Agent — file-level backup'
                  }
                  actions={
                    <span className="inline-flex items-center gap-1.5 text-xs text-slate-500">
                      <Database className="size-3.5" aria-hidden />
                      <Num>{formatBytes(selected.totalBytes)}</Num> total
                    </span>
                  }
                />
                {loading && !detail ? (
                  <LoadingBlock label="Loading restore points…" />
                ) : !detail || detail.length === 0 ? (
                  <EmptyState
                    className="border-0 bg-transparent"
                    icon={<History className="size-5" aria-hidden />}
                    title="Every point for this workload has gone"
                    description="Retention pruned them, or they were deleted by hand. The next successful run of its job writes a fresh full backup."
                    action={<Button onClick={() => void refresh()}>Check again</Button>}
                  />
                ) : (
                  <div className="divide-y divide-slate-800">
                    {detail.map((backup) => (
                      <BackupRow
                        key={String(backup.id)}
                        backup={backup}
                        target={targetOf(backup)}
                        depth={depths.get(String(backup.id)) ?? 0}
                        onRestore={() => setRestoring(backup)}
                        onDeleted={() => void refresh()}
                      />
                    ))}
                  </div>
                )}
              </>
            )}
          </Card>
        </div>
      )}

      <RestoreWizard
        backup={restoring}
        target={restoring ? targetOf(restoring) : undefined}
        hosts={data?.hosts ?? []}
        agents={data?.agents ?? []}
        vms={data?.vms ?? []}
        onClose={() => setRestoring(null)}
        onStarted={() => void refresh()}
      />
    </>
  )
}
