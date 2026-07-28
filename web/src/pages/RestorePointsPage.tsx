import { useCallback, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import {
  ChevronRight,
  Database,
  FolderOpen,
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
import { FileBrowser } from '../components/FileBrowser'
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
  onBrowse,
  onDeleted,
}: {
  backup: Backup
  /** The target holding this point, when it is still configured. */
  target?: Target
  depth: number
  onRestore: () => void
  onBrowse: () => void
  onDeleted: () => void
}) {
  const toast = useToast()
  const confirm = useConfirm()
  const navigate = useNavigate()
  const { can, role } = useSession()
  const [deleting, setDeleting] = useState(false)
  const [verifying, setVerifying] = useState(false)

  const denied = can.operateJobs ? undefined : roleDeniedReason(role, 'restore, verify or prune')
  // Only an agent file backup is an archive of files; a VM point is disk images.
  const browsable = backup.sourceKind === 'agent'

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
        {/* A VM point holds raw disk images rather than an archive of files, so
            there is nothing to list yet. The button says why instead of opening
            a dialog that can only apologise. */}
        <Button
          size="sm"
          icon={<FolderOpen className="size-3.5" aria-hidden />}
          onClick={onBrowse}
          disabled={!browsable || !can.operateJobs}
          title={
            browsable
              ? (denied ?? 'Browse the files in this restore point and recover one')
              : 'This is a whole-VM image. Install the agent in the guest for file-level backups you can browse.'
          }
          aria-label={`Browse the files in the restore point from ${formatDateTime(backup.createdAt)}`}
        >
          Browse
        </Button>
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

  /**
   * Which workloads are expanded. `null` means the operator has not touched the
   * list yet, in which case the newest workload is open — the page is useful on
   * arrival without pre-empting any later choice.
   */
  const [openKeys, setOpenKeys] = useState<ReadonlySet<string> | null>(null)
  const [restoring, setRestoring] = useState<Backup | null>(null)
  const [browsing, setBrowsing] = useState<Backup | null>(null)

  const groups = useMemo(() => groupSources(data?.backups ?? []), [data?.backups])

  const open = useMemo(
    () => openKeys ?? new Set(groups[0] ? [groups[0].key] : []),
    [openKeys, groups],
  )

  const toggle = (key: string) => {
    const next = new Set(open)
    if (next.has(key)) next.delete(key)
    else next.add(key)
    setOpenKeys(next)
  }

  /* The list already carries every point, so the chains are grouped from it
     rather than fetched again — one source of truth, and no window where the
     two disagree after a delete. */
  const chains = useMemo(() => {
    const byWorkload = new Map<string, Backup[]>()
    for (const backup of data?.backups ?? []) {
      const key = sourceKey(backup)
      const chain = byWorkload.get(key)
      if (chain) chain.push(backup)
      else byWorkload.set(key, [backup])
    }
    for (const chain of byWorkload.values()) {
      chain.sort((a, b) => new Date(b.createdAt).getTime() - new Date(a.createdAt).getTime())
    }
    return byWorkload
  }, [data?.backups])

  /* Depth resolves through parent ids, which never cross a workload, so one
     pass over every point gives the same answer as one pass per chain. */
  const depths = useMemo(() => chainDepths(data?.backups ?? []), [data?.backups])

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
        /* One full-width list rather than a master/detail split. A split puts a
           tall list of workloads beside a panel holding one workload's points,
           and grid rows are as tall as their tallest cell, so the short side is
           padded out with empty space that grows with the size of the estate.
           Expanding in place cannot produce that gap at any number of
           workloads or points. */
        <Card>
          <CardHeader title="Workloads" subtitle={`${groups.length} with restore points`} />
          <ul className="divide-y divide-slate-800">
            {groups.map((group) => {
              const expanded = open.has(group.key)
              const chain = chains.get(group.key) ?? []
              const panelId = `chain-${group.key.replace(/[^a-zA-Z0-9_-]/g, '-')}`
              const identity = {
                hostName: group.hostName,
                name: group.sourceName,
                vmid: group.sourceKind === 'vm' ? sourceVMID(group.sourceId) : null,
                node: group.node,
              }
              return (
                <li key={group.key}>
                  <button
                    type="button"
                    onClick={() => toggle(group.key)}
                    aria-expanded={expanded}
                    aria-controls={panelId}
                    aria-label={`${identityText(identity)} — ${
                      expanded ? 'hide' : 'show'
                    } ${formatCount(group.count)} restore ${
                      group.count === 1 ? 'point' : 'points'
                    }`}
                    className={cn(
                      'flex w-full items-start gap-3 px-4 py-3.5 text-left transition-colors duration-150',
                      expanded ? 'bg-accent-500/[0.07]' : 'hover:bg-slate-800/40',
                    )}
                  >
                    <ChevronRight
                      className={cn(
                        'mt-1.5 size-4 shrink-0 text-slate-500 transition-transform duration-150',
                        expanded && 'rotate-90 text-accent-300',
                      )}
                      aria-hidden
                    />
                    <span
                      className={cn(
                        'mt-0.5 flex size-7 shrink-0 items-center justify-center rounded-lg border',
                        expanded
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
                        emphasis={expanded ? 'strong' : 'normal'}
                        workload={identity}
                      />
                      <span className="mt-1 block truncate text-meta text-slate-500">
                        <Num>{formatCount(group.count)}</Num>{' '}
                        {group.count === 1 ? 'point' : 'points'} ·{' '}
                        <Num>{formatBytes(group.totalBytes)}</Num> · newest{' '}
                        <Num>{formatRelative(group.latest)}</Num>
                        {' · '}
                        <span className={group.lastVerifiedAt ? 'text-ok-400' : 'text-slate-600'}>
                          {group.lastVerifiedAt
                            ? `integrity verified ${formatRelative(group.lastVerifiedAt)}`
                            : 'no point verified yet'}
                        </span>
                      </span>
                    </span>

                    <span className="hidden shrink-0 items-center gap-1.5 pt-0.5 text-xs text-slate-500 sm:inline-flex">
                      <Database className="size-3.5" aria-hidden />
                      <Num>{formatBytes(group.totalBytes)}</Num> total
                    </span>
                  </button>

                  {expanded ? (
                    <div
                      id={panelId}
                      /* `min-w-0` keeps a wide chain row inside the card: without
                         it the row sets the width and the page scrolls sideways
                         instead of the row truncating. */
                      className="min-w-0 border-t border-slate-800/70 bg-slate-950/40"
                    >
                      <p className="px-5 pt-3 text-meta text-slate-500">
                        {group.sourceKind === 'vm'
                          ? 'Virtual machine — agentless image backup'
                          : 'Agent — file-level backup'}
                      </p>
                      <div className="divide-y divide-slate-800/70">
                        {chain.map((backup) => (
                          <BackupRow
                            key={String(backup.id)}
                            backup={backup}
                            target={targetOf(backup)}
                            depth={depths.get(String(backup.id)) ?? 0}
                            onRestore={() => setRestoring(backup)}
                            onBrowse={() => setBrowsing(backup)}
                            onDeleted={() => void refresh()}
                          />
                        ))}
                      </div>
                    </div>
                  ) : null}
                </li>
              )
            })}
          </ul>
        </Card>
      )}

      <FileBrowser backup={browsing} onClose={() => setBrowsing(null)} />

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
