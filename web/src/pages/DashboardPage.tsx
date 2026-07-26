import { lazy, Suspense, useCallback, useMemo } from 'react'
import { Link } from 'react-router-dom'
import {
  Activity,
  CalendarClock,
  Database,
  HardDrive,
  Laptop,
  RefreshCw,
  Server,
  ShieldCheck,
  Sparkles,
} from 'lucide-react'
import type { LucideIcon } from 'lucide-react'
import { getDashboard } from '../api'
import type { DashboardStats, JobRun } from '../api'
import {
  Button,
  Card,
  CardHeader,
  EmptyState,
  ErrorBlock,
  PageHeader,
  ProgressBar,
  RunStatusPill,
  SkeletonCards,
  Spinner,
} from '../components/ui'
import { useAsync, usePolling } from '../lib/useAsync'
import {
  clampPct,
  formatBytes,
  formatCount,
  formatDuration,
  formatRatio,
  formatRelative,
} from '../lib/format'

function StatCard({
  icon: Icon,
  label,
  value,
  to,
}: {
  icon: LucideIcon
  label: string
  value: number
  to: string
}) {
  return (
    <Link
      to={to}
      className="group rounded-xl border border-slate-800 bg-slate-900/60 px-5 py-4 transition-colors hover:border-slate-700 hover:bg-slate-900"
    >
      <div className="flex items-center justify-between">
        <p className="text-[11px] font-medium tracking-wide text-slate-500 uppercase">{label}</p>
        <Icon className="size-4 text-slate-600 transition-colors group-hover:text-accent-400" aria-hidden />
      </div>
      <p className="mt-2.5 text-3xl font-semibold tracking-tight text-white">
        {formatCount(value)}
      </p>
    </Link>
  )
}

const OutcomeDonut = lazy(() => import('../components/OutcomeDonut'))

const OUTCOME_COLORS = {
  succeeded: '#10b981',
  failed: '#ef4444',
  running: '#0ea5e9',
} as const

function OutcomeChart({ stats }: { stats: DashboardStats }) {
  const slices = useMemo(
    () => [
      {
        key: 'succeeded',
        name: 'Succeeded',
        value: stats.last24h.succeeded,
        fill: OUTCOME_COLORS.succeeded,
      },
      { key: 'failed', name: 'Failed', value: stats.last24h.failed, fill: OUTCOME_COLORS.failed },
      { key: 'running', name: 'Running', value: stats.last24h.running, fill: OUTCOME_COLORS.running },
    ],
    [stats.last24h],
  )

  const total = slices.reduce((sum, slice) => sum + slice.value, 0)

  return (
    <div className="flex flex-col gap-5 sm:flex-row sm:items-center">
      <div className="relative h-40 w-40 shrink-0 self-center">
        {total === 0 ? (
          <div className="flex size-full items-center justify-center rounded-full border-[6px] border-slate-800 text-xs text-slate-600">
            No runs
          </div>
        ) : (
          <>
            <Suspense
              fallback={
                <div className="flex size-full items-center justify-center rounded-full border-[6px] border-slate-800">
                  <Spinner />
                </div>
              }
            >
              <OutcomeDonut slices={slices} />
            </Suspense>
            <div className="pointer-events-none absolute inset-0 flex flex-col items-center justify-center">
              <span className="text-2xl font-semibold text-white">{formatCount(total)}</span>
              <span className="text-[11px] tracking-wide text-slate-500 uppercase">runs</span>
            </div>
          </>
        )}
      </div>

      <dl className="flex-1 space-y-2.5">
        {slices.map((slice) => (
          <div key={slice.key} className="flex items-center gap-3">
            <span
              className="size-2.5 shrink-0 rounded-full"
              style={{ background: slice.fill }}
              aria-hidden
            />
            <dt className="flex-1 text-sm text-slate-400">{slice.name}</dt>
            <dd className="text-sm font-medium text-slate-100">{formatCount(slice.value)}</dd>
          </div>
        ))}
      </dl>
    </div>
  )
}

function StorageCard({ stats }: { stats: DashboardStats }) {
  const stored = Math.max(0, stats.storageBytes)
  const saved = Math.max(0, stats.dedupSavedBytes)
  const logical = stored + saved
  const savedPct = logical > 0 ? (saved / logical) * 100 : 0

  return (
    <Card>
      <CardHeader title="Storage" subtitle="Across every configured target" />
      <div className="space-y-5 px-5 py-5">
        <div className="grid gap-4 sm:grid-cols-2">
          <div>
            <p className="text-[11px] tracking-wide text-slate-500 uppercase">Stored on target</p>
            <p className="mt-1 text-2xl font-semibold text-white">{formatBytes(stored)}</p>
          </div>
          <div>
            <p className="text-[11px] tracking-wide text-slate-500 uppercase">
              Saved by deduplication
            </p>
            <p className="mt-1 text-2xl font-semibold text-emerald-400">{formatBytes(saved)}</p>
          </div>
        </div>

        <div>
          <div className="mb-2 flex items-center justify-between text-xs text-slate-500">
            <span>Logical protected data — {formatBytes(logical)}</span>
            <span className="text-emerald-400">{clampPct(savedPct).toFixed(0)}% saved</span>
          </div>
          <div className="flex h-2.5 w-full overflow-hidden rounded-full bg-slate-800">
            <div
              className="h-full bg-accent-500"
              style={{ width: `${100 - clampPct(savedPct)}%` }}
              aria-hidden
            />
            <div
              className="h-full bg-emerald-500/70"
              style={{ width: `${clampPct(savedPct)}%` }}
              aria-hidden
            />
          </div>
          <div className="mt-2 flex items-center gap-4 text-[11px] text-slate-500">
            <span className="flex items-center gap-1.5">
              <span className="size-2 rounded-full bg-accent-500" aria-hidden />
              Uploaded chunks
            </span>
            <span className="flex items-center gap-1.5">
              <span className="size-2 rounded-full bg-emerald-500/70" aria-hidden />
              Deduplicated
            </span>
          </div>
        </div>
      </div>
    </Card>
  )
}

function RecentRunsTable({ runs }: { runs: JobRun[] }) {
  if (runs.length === 0) {
    return (
      <div className="px-5 py-12 text-center">
        <p className="text-sm text-slate-400">No backup runs yet.</p>
        <p className="mt-1 text-xs text-slate-500">
          Create a backup job and run it to see activity here.
        </p>
        <Link
          to="/jobs"
          className="mt-4 inline-flex items-center gap-1.5 text-xs font-medium text-accent-400 hover:text-accent-300"
        >
          Go to backup jobs
        </Link>
      </div>
    )
  }

  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[46rem] text-sm">
        <thead>
          <tr className="border-b border-slate-800 text-left text-[11px] tracking-wide text-slate-500 uppercase">
            <th className="px-5 py-2.5 font-medium">Job</th>
            <th className="px-5 py-2.5 font-medium">Status</th>
            <th className="px-5 py-2.5 font-medium">Started</th>
            <th className="px-5 py-2.5 font-medium">Duration</th>
            <th className="px-5 py-2.5 text-right font-medium">Processed</th>
            <th className="px-5 py-2.5 text-right font-medium">Uploaded</th>
            <th className="px-5 py-2.5 text-right font-medium">Dedup</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-slate-800/70">
          {runs.map((run) => (
            <tr key={String(run.id)} className="transition-colors hover:bg-slate-800/30">
              <td className="max-w-[16rem] px-5 py-3">
                <p className="truncate font-medium text-slate-100">{run.jobName}</p>
                {run.status === 'running' ? (
                  <div className="mt-1.5 flex items-center gap-2">
                    <ProgressBar value={run.progressPct} active className="max-w-32" />
                    <span className="text-[11px] text-slate-500">
                      {clampPct(run.progressPct).toFixed(0)}%
                    </span>
                  </div>
                ) : run.error ? (
                  <p className="mt-0.5 truncate text-xs text-red-400/80">{run.error}</p>
                ) : null}
              </td>
              <td className="px-5 py-3">
                <RunStatusPill status={run.status} />
              </td>
              <td className="px-5 py-3 whitespace-nowrap text-slate-400">
                {formatRelative(run.startedAt)}
              </td>
              <td className="px-5 py-3 whitespace-nowrap text-slate-400">
                {formatDuration(run.startedAt, run.finishedAt)}
              </td>
              <td className="px-5 py-3 text-right whitespace-nowrap text-slate-300">
                {formatBytes(run.bytesProcessed)}
              </td>
              <td className="px-5 py-3 text-right whitespace-nowrap text-slate-300">
                {formatBytes(run.bytesUploaded)}
              </td>
              <td className="px-5 py-3 text-right whitespace-nowrap text-emerald-400">
                {formatRatio(run.dedupRatio)}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

export function DashboardPage() {
  const loader = useCallback(() => getDashboard(), [])
  const { data, loading, error, reload, refresh } = useAsync(loader)

  const hasRunning = Boolean(
    data && (data.last24h.running > 0 || data.recentRuns.some((run) => run.status === 'running')),
  )
  usePolling(() => void refresh(), 2000, hasRunning)

  if (loading && !data) {
    return (
      <>
        <PageHeader title="Dashboard" description="Protection status across your Proxmox estate." />
        <SkeletonCards count={5} height="h-24" />
      </>
    )
  }

  if (error && !data) {
    return (
      <>
        <PageHeader title="Dashboard" />
        <ErrorBlock message={error} onRetry={() => void reload()} />
      </>
    )
  }

  if (!data) return null

  return (
    <>
      <PageHeader
        title="Dashboard"
        description="Protection status across your Proxmox estate."
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

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-5">
        <StatCard icon={Laptop} label="Virtual machines" value={data.vmCount} to="/vms" />
        <StatCard icon={Server} label="Proxmox hosts" value={data.hostCount} to="/hosts" />
        <StatCard icon={ShieldCheck} label="Agents" value={data.agentCount} to="/agents" />
        <StatCard icon={Database} label="Storage targets" value={data.targetCount} to="/targets" />
        <StatCard icon={CalendarClock} label="Backup jobs" value={data.jobCount} to="/jobs" />
      </div>

      <div className="mt-4 grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader
            title="Last 24 hours"
            subtitle="Outcome of every job run in the past day"
            actions={
              <Link
                to="/monitor"
                className="inline-flex items-center gap-1.5 text-xs font-medium text-accent-400 hover:text-accent-300"
              >
                <Activity className="size-3.5" aria-hidden />
                Monitor
              </Link>
            }
          />
          <div className="px-5 py-5">
            <OutcomeChart stats={data} />
          </div>
        </Card>

        <StorageCard stats={data} />
      </div>

      <Card className="mt-4">
        <CardHeader
          title="Recent runs"
          subtitle="Newest first"
          actions={
            <Link
              to="/monitor"
              className="inline-flex items-center gap-1.5 text-xs font-medium text-accent-400 hover:text-accent-300"
            >
              View all runs
            </Link>
          }
        />
        <RecentRunsTable runs={data.recentRuns} />
      </Card>

      {data.hostCount === 0 || data.targetCount === 0 ? (
        <div className="mt-4">
          <EmptyState
            icon={data.hostCount === 0 ? <Server className="size-5" aria-hidden /> : <HardDrive className="size-5" aria-hidden />}
            title={data.hostCount === 0 ? 'Add your first Proxmox host' : 'Add a storage target'}
            description={
              data.hostCount === 0
                ? 'ProxBack needs an API token for at least one Proxmox VE host before it can inventory virtual machines.'
                : 'Backups are written to S3-compatible storage — Backblaze B2, MinIO, or AWS S3. Add a target to start protecting data.'
            }
            action={
              <Link to={data.hostCount === 0 ? '/hosts' : '/targets'}>
                <Button variant="primary" icon={<Sparkles className="size-4" aria-hidden />}>
                  {data.hostCount === 0 ? 'Add Proxmox host' : 'Add storage target'}
                </Button>
              </Link>
            }
          />
        </div>
      ) : null}
    </>
  )
}
