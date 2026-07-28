/**
 * Workload identity.
 *
 * Two clusters can each hold a guest called `db-01`, and both can hold a node
 * called `pve1`. A name alone is therefore never an identity in this console:
 * anywhere a workload is chosen or displayed it reads
 * `cluster / name (vmid) / node`, in that order — widest scope first, so a
 * column of them sorts and scans by cluster.
 *
 * The pieces render at different weights rather than as one grey string: the
 * name is what the operator is looking for, the cluster and node are what
 * disambiguate it.
 */

import { cn } from '../lib/cn'

export interface WorkloadRef {
  /** Cluster / Proxmox host display name. */
  hostName?: string | null
  name: string
  /** VMID for guests; agents have none. */
  vmid?: number | string | null
  node?: string | null
}

/** Plain-text identity for titles, toasts, confirmations and `aria-label`s. */
export function identityText(workload: WorkloadRef): string {
  const head = workload.hostName ? `${workload.hostName} / ` : ''
  const id =
    workload.vmid === undefined || workload.vmid === null || workload.vmid === ''
      ? ''
      : ` (${workload.vmid})`
  const tail = workload.node ? ` / ${workload.node}` : ''
  return `${head}${workload.name}${id}${tail}`
}

/**
 * Two-line identity for tables and pickers: the name and VMID on the first
 * line, the cluster and node underneath. `inline` collapses it to one line for
 * dense rows and chips.
 */
export function WorkloadIdentity({
  workload,
  inline = false,
  className,
  emphasis = 'normal',
}: {
  workload: WorkloadRef
  inline?: boolean
  className?: string
  emphasis?: 'normal' | 'strong'
}) {
  const vmid =
    workload.vmid === undefined || workload.vmid === null || workload.vmid === ''
      ? null
      : String(workload.vmid)

  const scope = [workload.hostName, workload.node].filter(Boolean) as string[]

  if (inline) {
    return (
      <span className={cn('inline-flex min-w-0 items-baseline gap-1.5', className)} title={identityText(workload)}>
        {workload.hostName ? (
          <span className="shrink-0 text-meta text-slate-500">{workload.hostName}</span>
        ) : null}
        {workload.hostName ? (
          <span className="shrink-0 text-slate-600" aria-hidden>
            /
          </span>
        ) : null}
        <span
          className={cn(
            'min-w-0 flex-1 truncate text-[13px]',
            emphasis === 'strong' ? 'font-medium text-slate-100' : 'text-slate-200',
          )}
        >
          {workload.name}
        </span>
        {vmid ? (
          <span className="max-w-[45%] shrink-0 truncate font-mono text-meta text-slate-500">
            ({vmid})
          </span>
        ) : null}
        {workload.node ? (
          <>
            <span className="shrink-0 text-slate-600" aria-hidden>
              /
            </span>
            <span className="shrink-0 font-mono text-meta text-slate-500">{workload.node}</span>
          </>
        ) : null}
      </span>
    )
  }

  return (
    <span className={cn('block min-w-0', className)} title={identityText(workload)}>
      {/* The name takes the leftover width (`flex-1`, basis 0) and the
          identifier keeps its natural width — but capped, because a
          content-addressed agent id is long enough to squeeze the name down to
          a single letter otherwise. Whichever one truncates, the full identity
          is still on the `title`. */}
      <span className="flex items-baseline gap-1.5">
        <span
          className={cn(
            'min-w-0 flex-1 truncate text-[13px]',
            emphasis === 'strong' ? 'font-medium text-slate-100' : 'text-slate-200',
          )}
        >
          {workload.name}
        </span>
        {vmid ? (
          <span className="max-w-[45%] shrink-0 truncate font-mono text-meta text-slate-500">
            ({vmid})
          </span>
        ) : null}
      </span>
      {scope.length > 0 ? (
        <span className="mt-0.5 block truncate font-mono text-meta text-slate-500">
          {scope.join(' / ')}
        </span>
      ) : null}
    </span>
  )
}
