import { useCallback, useEffect, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import {
  ArrowLeft,
  Database,
  History,
  Laptop,
  RefreshCw,
  RotateCcw,
  ShieldCheck,
  Trash2,
} from 'lucide-react'
import {
  createRestore,
  deleteBackup,
  errorMessage,
  listAgents,
  listBackups,
  listHosts,
  listVMs,
} from '../api'
import type { Agent, Backup, CachedVM, Host, ID, SourceKind } from '../api'
import { Modal } from '../components/Modal'
import { useConfirm } from '../components/Confirm'
import { useToast } from '../components/Toast'
import {
  Button,
  Card,
  CardHeader,
  EmptyState,
  ErrorBlock,
  Field,
  IconButton,
  Input,
  LoadingBlock,
  PageHeader,
  SectionNote,
  Select,
  SkeletonRows,
  StatusPill,
} from '../components/ui'
import { cn } from '../lib/cn'
import { useAsync } from '../lib/useAsync'
import { formatBytes, formatDateTime, formatRelative } from '../lib/format'

interface RestoreData {
  backups: Backup[]
  hosts: Host[]
  agents: Agent[]
  vms: CachedVM[]
}

interface SourceGroup {
  key: string
  sourceKind: SourceKind
  sourceId: ID
  sourceName: string
  count: number
  latest: string
  totalBytes: number
}

function sourceKey(kind: SourceKind, id: ID): string {
  return `${kind}:${String(id)}`
}

function groupSources(backups: Backup[]): SourceGroup[] {
  const groups = new Map<string, SourceGroup>()
  for (const backup of backups) {
    const key = sourceKey(backup.sourceKind, backup.sourceId)
    const existing = groups.get(key)
    if (existing) {
      existing.count += 1
      existing.totalBytes += backup.sizeBytes
      if (new Date(backup.createdAt).getTime() > new Date(existing.latest).getTime()) {
        existing.latest = backup.createdAt
      }
    } else {
      groups.set(key, {
        key,
        sourceKind: backup.sourceKind,
        sourceId: backup.sourceId,
        sourceName: backup.sourceName,
        count: 1,
        totalBytes: backup.sizeBytes,
        latest: backup.createdAt,
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

function RestoreWizard({
  backup,
  hosts,
  agents,
  vms,
  onClose,
  onStarted,
}: {
  backup: Backup | null
  hosts: Host[]
  agents: Agent[]
  vms: CachedVM[]
  onClose: () => void
  onStarted: () => void
}) {
  const toast = useToast()
  const navigate = useNavigate()
  const [hostId, setHostId] = useState<string>('')
  const [node, setNode] = useState('')
  const [vmid, setVmid] = useState('')
  const [agentId, setAgentId] = useState<string>('')
  const [destPath, setDestPath] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const isVM = backup?.sourceKind === 'vm'

  // Prefill from the cached inventory: the VM this restore point came from.
  useEffect(() => {
    if (!backup) return
    setError(null)
    setSubmitting(false)
    if (backup.sourceKind === 'vm') {
      const match =
        vms.find((vm) => String(vm.vmid) === String(backup.sourceId)) ??
        vms.find((vm) => vm.name === backup.sourceName)
      setHostId(String(match?.hostId ?? hosts[0]?.id ?? ''))
      setNode(match?.node ?? '')
      setVmid(String(match?.vmid ?? backup.sourceId ?? ''))
    } else {
      const match = agents.find((agent) => String(agent.id) === String(backup.sourceId))
      setAgentId(String(match?.id ?? agents[0]?.id ?? ''))
      setDestPath('')
    }
  }, [backup, hosts, agents, vms])

  const onSubmit = async () => {
    if (!backup) return
    setError(null)

    if (isVM) {
      if (!hostId) return setError('Choose the Proxmox host to restore into.')
      if (!node.trim()) return setError('Enter the target node name.')
      const parsedVmid = Number(vmid)
      if (!Number.isInteger(parsedVmid) || parsedVmid <= 0) {
        return setError('Enter a numeric VMID for the restored machine.')
      }
    } else {
      if (!agentId) return setError('Choose the agent to restore to.')
      if (!destPath.trim()) return setError('Enter the destination path on the agent.')
    }

    setSubmitting(true)
    try {
      const result = await createRestore(
        isVM
          ? { backupId: backup.id, vm: { hostId, node: node.trim(), vmid: Number(vmid) } }
          : { backupId: backup.id, agent: { agentId, destPath: destPath.trim() } },
      )
      toast.success(
        `Restore of ${backup.sourceName} started.`,
        `Run ${String(result.runId)} — follow it on the Monitor page.`,
      )
      onStarted()
      onClose()
      navigate('/monitor')
    } catch (err) {
      const message = errorMessage(err)
      setError(message)
      toast.error('Could not start restore', message)
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Modal
      open={backup !== null}
      onClose={onClose}
      width="lg"
      title="Restore"
      subtitle={
        backup
          ? `${backup.sourceName} — ${backup.kind} restore point from ${formatDateTime(backup.createdAt)}`
          : undefined
      }
      footer={
        <>
          <Button onClick={onClose} disabled={submitting}>
            Cancel
          </Button>
          <Button
            variant="primary"
            loading={submitting}
            onClick={() => void onSubmit()}
            icon={<RotateCcw className="size-4" aria-hidden />}
          >
            Start restore
          </Button>
        </>
      }
    >
      {!backup ? null : (
        <div className="space-y-4">
          <dl className="grid grid-cols-2 gap-4 rounded-lg border border-slate-800 bg-slate-950/40 px-4 py-3">
            <div>
              <dt className="text-[11px] tracking-wide text-slate-500 uppercase">Size</dt>
              <dd className="mt-0.5 text-sm text-slate-200">{formatBytes(backup.sizeBytes)}</dd>
            </div>
            <div>
              <dt className="text-[11px] tracking-wide text-slate-500 uppercase">Created</dt>
              <dd className="mt-0.5 text-sm text-slate-200">{formatRelative(backup.createdAt)}</dd>
            </div>
          </dl>

          {isVM ? (
            <>
              <Field label="Restore into host">
                {({ id }) => (
                  <Select
                    id={id}
                    value={hostId}
                    onChange={(event) => setHostId(event.target.value)}
                  >
                    <option value="">Select a host…</option>
                    {hosts.map((host) => (
                      <option key={String(host.id)} value={String(host.id)}>
                        {host.name}
                      </option>
                    ))}
                  </Select>
                )}
              </Field>

              <div className="grid gap-4 sm:grid-cols-2">
                <Field label="Node" hint="Cluster node that will own the restored disks.">
                  {({ id }) => (
                    <Input
                      id={id}
                      value={node}
                      placeholder="pve-node-1"
                      onChange={(event) => setNode(event.target.value)}
                    />
                  )}
                </Field>
                <Field label="VMID" hint="Use a free VMID to restore side by side.">
                  {({ id }) => (
                    <Input
                      id={id}
                      type="number"
                      min={100}
                      value={vmid}
                      onChange={(event) => setVmid(event.target.value)}
                    />
                  )}
                </Field>
              </div>

              <SectionNote>
                Every chunk is verified against its SHA-256 hash while the disk image is streamed
                back to the host. Restoring onto an existing VMID overwrites that machine’s disks.
              </SectionNote>
            </>
          ) : (
            <>
              <Field label="Restore to agent">
                {({ id }) => (
                  <Select
                    id={id}
                    value={agentId}
                    onChange={(event) => setAgentId(event.target.value)}
                  >
                    <option value="">Select an agent…</option>
                    {agents.map((agent) => (
                      <option key={String(agent.id)} value={String(agent.id)}>
                        {agent.hostname} — {agent.os}/{agent.arch} ({agent.status})
                      </option>
                    ))}
                  </Select>
                )}
              </Field>

              <Field
                label="Destination path"
                hint="Absolute path on the guest. Files are unpacked into this directory."
              >
                {({ id }) => (
                  <Input
                    id={id}
                    value={destPath}
                    placeholder="/restore/2026-07-26  or  C:\\restore"
                    onChange={(event) => setDestPath(event.target.value)}
                  />
                )}
              </Field>

              <SectionNote>
                The agent pulls a verified tar stream from the server and unpacks it. Existing files
                with the same names inside the destination are overwritten.
              </SectionNote>
            </>
          )}

          {error ? (
            <p className="rounded-lg border border-red-500/30 bg-red-500/10 px-3.5 py-2.5 text-xs text-red-300">
              {error}
            </p>
          ) : null}
        </div>
      )}
    </Modal>
  )
}

function BackupRow({
  backup,
  depth,
  onRestore,
  onDeleted,
}: {
  backup: Backup
  depth: number
  onRestore: () => void
  onDeleted: () => void
}) {
  const toast = useToast()
  const confirm = useConfirm()
  const [deleting, setDeleting] = useState(false)

  const onDelete = async () => {
    const ok = await confirm({
      title: 'Delete restore point',
      message: (
        <>
          Delete the {backup.kind} restore point of{' '}
          <span className="font-medium text-slate-100">{backup.sourceName}</span> from{' '}
          {formatDateTime(backup.createdAt)}? Its manifest is removed and chunks referenced by no
          other restore point are garbage-collected. Later incrementals in this chain may become
          unusable.
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
          <span className="-ml-4 h-6 w-3 shrink-0 rounded-bl-md border-b border-l border-slate-700" aria-hidden />
        ) : null}
        <span
          className={cn(
            'flex size-8 shrink-0 items-center justify-center rounded-lg border',
            backup.kind === 'full'
              ? 'border-accent-500/40 bg-accent-500/10 text-accent-300'
              : 'border-slate-800 bg-slate-950/60 text-slate-400',
          )}
        >
          <History className="size-4" aria-hidden />
        </span>
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <p className="text-sm text-slate-100">{formatDateTime(backup.createdAt)}</p>
            <StatusPill
              tone={backup.kind === 'full' ? 'blue' : 'slate'}
              label={backup.kind === 'full' ? 'full' : 'incremental'}
            />
          </div>
          <p className="mt-0.5 truncate text-xs text-slate-500">
            {formatBytes(backup.sizeBytes)} logical · {formatBytes(backup.uploadedBytes)} uploaded
            {backup.disks.length > 0
              ? ` · ${backup.disks.length} ${backup.disks.length === 1 ? 'disk' : 'disks'}: ${backup.disks
                  .map((disk) => `${disk.name} ${formatBytes(disk.sizeBytes)}`)
                  .join(', ')}`
              : ''}
          </p>
        </div>
      </div>

      <div className="flex shrink-0 items-center gap-2">
        <Button
          size="sm"
          variant="primary"
          icon={<RotateCcw className="size-3.5" aria-hidden />}
          onClick={onRestore}
        >
          Restore
        </Button>
        <IconButton
          variant="danger"
          aria-label="Delete restore point"
          title="Delete restore point"
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
    const [backups, hosts, agents, vms] = await Promise.all([
      listBackups(),
      listHosts(),
      listAgents(),
      listVMs(),
    ])
    return { backups, hosts, agents, vms }
  }, [])
  const { data, loading, error, reload, refresh } = useAsync(loader)

  const [selected, setSelected] = useState<SourceGroup | null>(null)
  const [detail, setDetail] = useState<Backup[] | null>(null)
  const [detailLoading, setDetailLoading] = useState(false)
  const [detailError, setDetailError] = useState<string | null>(null)
  const [restoring, setRestoring] = useState<Backup | null>(null)

  const groups = useMemo(() => groupSources(data?.backups ?? []), [data?.backups])

  const loadDetail = useCallback(async (group: SourceGroup) => {
    setDetailLoading(true)
    setDetailError(null)
    try {
      const rows = await listBackups({ sourceKind: group.sourceKind, sourceId: group.sourceId })
      setDetail(
        [...rows].sort(
          (a, b) => new Date(b.createdAt).getTime() - new Date(a.createdAt).getTime(),
        ),
      )
    } catch (err) {
      setDetailError(errorMessage(err))
      setDetail(null)
    } finally {
      setDetailLoading(false)
    }
  }, [])

  const onSelect = (group: SourceGroup) => {
    setSelected(group)
    void loadDetail(group)
  }

  const afterMutation = async () => {
    await refresh()
    if (selected) await loadDetail(selected)
  }

  const depths = useMemo(() => chainDepths(detail ?? []), [detail])

  return (
    <>
      <PageHeader
        title="Restore Points"
        description="Every backup written to your targets, grouped by source and shown as full → incremental chains."
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
          description="Restore points appear here after a backup job succeeds. Each one is a manifest on your storage target that references deduplicated chunks."
          action={
            <Button variant="primary" onClick={() => void reload()}>
              Check again
            </Button>
          }
        />
      ) : (
        <div className="grid gap-4 lg:grid-cols-[20rem_1fr]">
          <Card className="h-fit">
            <CardHeader title="Sources" subtitle={`${groups.length} protected`} />
            <ul className="divide-y divide-slate-800">
              {groups.map((group) => {
                const active = selected?.key === group.key
                return (
                  <li key={group.key}>
                    <button
                      type="button"
                      onClick={() => onSelect(group)}
                      className={cn(
                        'flex w-full items-start gap-3 px-4 py-3 text-left transition',
                        active ? 'bg-accent-500/10' : 'hover:bg-slate-800/40',
                      )}
                    >
                      <span
                        className={cn(
                          'flex size-8 shrink-0 items-center justify-center rounded-lg border',
                          active
                            ? 'border-accent-500/40 bg-accent-500/15 text-accent-300'
                            : 'border-slate-800 bg-slate-950/60 text-slate-500',
                        )}
                      >
                        {group.sourceKind === 'vm' ? (
                          <Laptop className="size-4" aria-hidden />
                        ) : (
                          <ShieldCheck className="size-4" aria-hidden />
                        )}
                      </span>
                      <span className="min-w-0 flex-1">
                        <span
                          className={cn(
                            'block truncate text-sm',
                            active ? 'font-medium text-white' : 'text-slate-200',
                          )}
                        >
                          {group.sourceName}
                        </span>
                        <span className="mt-0.5 block truncate text-xs text-slate-500">
                          {group.count} {group.count === 1 ? 'point' : 'points'} ·{' '}
                          {formatBytes(group.totalBytes)} · {formatRelative(group.latest)}
                        </span>
                      </span>
                    </button>
                  </li>
                )
              })}
            </ul>
          </Card>

          <Card>
            {!selected ? (
              <div className="flex flex-col items-center justify-center px-6 py-20 text-center">
                <div className="mb-4 flex size-12 items-center justify-center rounded-xl border border-slate-800 bg-slate-900 text-accent-400">
                  <ArrowLeft className="size-5" aria-hidden />
                </div>
                <h3 className="text-base font-semibold text-slate-100">Pick a source</h3>
                <p className="mt-1.5 max-w-sm text-sm text-slate-400">
                  Choose a virtual machine or agent on the left to see its restore-point chain,
                  restore it, or prune old points.
                </p>
              </div>
            ) : (
              <>
                <CardHeader
                  title={selected.sourceName}
                  subtitle={
                    selected.sourceKind === 'vm'
                      ? 'Virtual machine — agentless image backup'
                      : 'Agent — file-level backup'
                  }
                  actions={
                    <span className="inline-flex items-center gap-1.5 text-xs text-slate-500">
                      <Database className="size-3.5" aria-hidden />
                      {formatBytes(selected.totalBytes)} total
                    </span>
                  }
                />
                {detailLoading && !detail ? (
                  <LoadingBlock label="Loading restore points…" />
                ) : detailError ? (
                  <div className="p-5">
                    <ErrorBlock
                      message={detailError}
                      onRetry={() => void loadDetail(selected)}
                    />
                  </div>
                ) : !detail || detail.length === 0 ? (
                  <div className="px-5 py-16 text-center">
                    <p className="text-sm text-slate-400">
                      No restore points remain for this source.
                    </p>
                  </div>
                ) : (
                  <div className="divide-y divide-slate-800">
                    {detail.map((backup) => (
                      <BackupRow
                        key={String(backup.id)}
                        backup={backup}
                        depth={depths.get(String(backup.id)) ?? 0}
                        onRestore={() => setRestoring(backup)}
                        onDeleted={() => void afterMutation()}
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
        hosts={data?.hosts ?? []}
        agents={data?.agents ?? []}
        vms={data?.vms ?? []}
        onClose={() => setRestoring(null)}
        onStarted={() => void afterMutation()}
      />
    </>
  )
}
