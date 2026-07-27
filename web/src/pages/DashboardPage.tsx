import { lazy, Suspense, useCallback, useMemo } from 'react'
import type { ReactNode } from 'react'
import { Link } from 'react-router-dom'
import {
  Activity,
  CalendarClock,
  Database,
  HardDrive,
  Laptop,
  RefreshCw,
  Server,
  ShieldAlert,
  ShieldCheck,
  Sparkles,
} from 'lucide-react'
import type { LucideIcon } from 'lucide-react'
import { getDashboard, listJobs, listVMs, tagsOf, vmSourcesOf } from '../api'
import type { CachedVM, DashboardStats, Job, JobRun } from '../api'
import {
  Button,
  Card,
  CardHeader,
  EmptyState,
  ErrorBlock,
  LiveDot,
  Num,
  PageHeader,
  ProgressBar,
  RunStatusPill,
  SectionHeading,
  SkeletonCards,
  Spinner,
} from '../components/ui'
import type { PillTone } from '../components/ui'
import { useAsync, usePolling } from '../lib/useAsync'
import { cn } from '../lib/cn'
import {
  clampPct,
  formatBytes,
  formatCount,
  formatDateTime,
  formatDuration,
  formatRatio,
  formatRelative,
} from '../lib/format'

interface DashboardData {
  stats: DashboardStats
  jobs: Job[]
  vms: CachedVM[]
}

/* ---------------------------------------------------------------------------
 * Estate status strip
 *
 * The one thing an operator wants on opening a backup console is "is my estate
 * healthy?". The strip answers it above the fold in five cells, and colour is
 * spent only where it means something: the verdict, and the count of things
 * that need a human.
 * ------------------------------------------------------------------------- */

interface Verdict {
  tone: PillTone
  headline: string
  detail: string
}

/** Newest successful backup run, if any. */
function lastSuccess(runs: JobRun[]): JobRun | null {
  return (
    runs
      .filter((run) => run.status === 'success')
      .sort(
        (a, b) =>
          new Date(b.finishedAt ?? b.startedAt).getTime() -
          new Date(a.finishedAt ?? a.startedAt).getTime(),
      )[0] ?? null
  )
}

/** Soonest upcoming fire time across enabled jobs. */
function nextScheduled(jobs: Job[]): Job | null {
  return (
    jobs
      .filter((job) => job.enabled && job.nextRun)
      .sort(
        (a, b) => new Date(a.nextRun as string).getTime() - new Date(b.nextRun as string).getTime(),
      )[0] ?? null
  )
}

/**
 * How many VMs a job actually covers. Fixed lists are read straight off the
 * job; tag-filtered jobs resolve against the cached inventory, which is the
 * same set the server would resolve at run start.
 */
function protectedVMKeys(jobs: Job[], vms: CachedVM[]): Set<string> {
  const keys = new Set<string>()
  for (const job of jobs) {
    if (job.kind !== 'vm' || !job.enabled) continue
    if (job.tagFilter) {
      for (const vm of vms) {
        if (tagsOf(vm).includes(job.tagFilter)) keys.add(`${String(vm.hostId)}:${vm.vmid}`)
      }
      continue
    }
    for (const source of vmSourcesOf(job)) keys.add(`${String(source.hostId)}:${source.vmid}`)
  }
  return keys
}

function verdictOf(data: DashboardData, unprotected: number, latest: JobRun | null): Verdict {
  const { stats, jobs } = data

  if (stats.last24h.failed > 0) {
    return {
      tone: 'red',
      headline: 'Attention needed',
      detail:
        stats.last24h.failed === 1
          ? '1 run failed in the last 24 hours'
          : `${stats.last24h.failed} runs failed in the last 24 hours`,
    }
  }
  if (stats.hostCount === 0 || stats.targetCount === 0 || jobs.length === 0) {
    return { tone: 'amber', headline: 'Setup incomplete', detail: 'Nothing is being protected yet' }
  }
  if (!latest) {
    return { tone: 'amber', headline: 'No backups yet', detail: 'No job has completed a run' }
  }
  const ageHours =
    (Date.now() - new Date(latest.finishedAt ?? latest.startedAt).getTime()) / 3_600_000
  if (ageHours > 36) {
    return {
      tone: 'amber',
      headline: 'Backups are stale',
      detail: `Last success ${formatRelative(latest.finishedAt ?? latest.startedAt)}`,
    }
  }
  if (unprotected > 0) {
    return {
      tone: 'amber',
      headline: 'Partially protected',
      detail:
        unprotected === 1
          ? '1 virtual machine is in no backup job'
          : `${unprotected} virtual machines are in no backup job`,
    }
  }
  return { tone: 'green', headline: 'Protected', detail: 'Every guest is covered and current' }
}

const VERDICT_SKIN: Record<PillTone, string> = {
  green: 'bg-emerald-500/[0.07] text-emerald-300',
  amber: 'bg-amber-500/[0.07] text-amber-300',
  red: 'bg-red-500/[0.07] text-red-300',
  blue: 'bg-accent-500/[0.07] text-accent-300',
  slate: 'bg-slate-800/40 text-slate-300',
}

const VERDICT_RULE: Record<PillTone, string> = {
  green: 'bg-emerald-400',
  amber: 'bg-amber-400',
  red: 'bg-red-400',
  blue: 'bg-accent-400',
  slate: 'bg-slate-600',
}

function StripCell({
  label,
  value,
  sub,
  to,
  tone,
}: {
  label: string
  value: ReactNode
  sub?: ReactNode
  to?: string
  tone?: 'default' | 'alert'
}) {
  const body = (
    <>
      <p className="eyebrow">{label}</p>
      <p
        className={cn(
          'figure-lg mt-1 truncate text-[19px] leading-6',
          tone === 'alert' ? 'text-red-300' : 'text-slate-50',
        )}
      >
        {value}
      </p>
      <p className="mt-0.5 truncate text-meta text-slate-500">{sub ?? ' '}</p>
    </>
  )

  // Horizontal rules while the cells stack, vertical ones once they sit in a
  // single row — so a rule never dangles at the start of a wrapped row.
  const classes =
    'block min-w-0 border-t border-slate-800/80 px-5 py-4 xl:border-t-0 xl:border-l'

  return to ? (
    <Link
      to={to}
      className={cn(classes, 'transition-colors duration-150 hover:bg-slate-800/30')}
    >
      {body}
    </Link>
  ) : (
    <div className={classes}>{body}</div>
  )
}

function EstateStrip({ data }: { data: DashboardData }) {
  const { stats, jobs, vms } = data
  const latest = lastSuccess(stats.recentRuns)
  const upcoming = nextScheduled(jobs)
  const covered = protectedVMKeys(jobs, vms)
  const totalVMs = vms.length || stats.vmCount
  const unprotected = Math.max(0, totalVMs - covered.size)
  const verdict = verdictOf(data, unprotected, latest)
  const failures = stats.last24h.failed

  return (
    <section
      className="overflow-hidden rounded-xl border border-slate-700/70 bg-slate-900/70 elev-2"
      aria-label="Estate status"
    >
      <div className="grid grid-cols-1 sm:grid-cols-2 xl:grid-cols-[minmax(15rem,1.1fr)_repeat(4,minmax(0,1fr))]">
        {/* Verdict. The only cell that carries a full colour wash. */}
        <div className={cn('relative flex items-center gap-3.5 px-5 py-4', VERDICT_SKIN[verdict.tone])}>
          <span
            className={cn('absolute inset-y-0 left-0 w-0.5', VERDICT_RULE[verdict.tone])}
            aria-hidden
          />
          <span className="flex size-9 shrink-0 items-center justify-center rounded-lg border border-current/25 bg-slate-950/40">
            {verdict.tone === 'green' ? (
              <ShieldCheck className="size-4.5" aria-hidden />
            ) : (
              <ShieldAlert className="size-4.5" aria-hidden />
            )}
          </span>
          <div className="min-w-0">
            <p className="truncate text-[15px] leading-5 font-semibold tracking-tight">
              {verdict.headline}
            </p>
            <p className="mt-0.5 truncate text-meta text-slate-400">{verdict.detail}</p>
          </div>
          {stats.last24h.running > 0 ? (
            <span className="ml-auto flex shrink-0 items-center gap-1.5 text-meta text-slate-400">
              <LiveDot tone="blue" />
              <Num>{stats.last24h.running}</Num> running
            </span>
          ) : null}
        </div>

        <StripCell
          label="Protected VMs"
          value={
            <>
              <Num>{formatCount(covered.size)}</Num>
              <span className="text-slate-600"> / </span>
              <Num className="text-slate-400">{formatCount(totalVMs)}</Num>
            </>
          }
          sub={unprotected === 0 ? 'All guests in a job' : `${unprotected} not in any job`}
          to="/vms"
        />

        <StripCell
          label="Last successful backup"
          value={latest ? formatRelative(latest.finishedAt ?? latest.startedAt) : 'never'}
          sub={latest ? latest.jobName : 'No completed run yet'}
          to="/monitor"
        />

        <StripCell
          label="Next scheduled run"
          value={upcoming?.nextRun ? formatRelative(upcoming.nextRun) : 'none'}
          sub={
            upcoming
              ? `${upcoming.name} · ${upcoming.scheduleLabel ?? ''}`.replace(/ · $/, '')
              : 'No enabled job has a schedule'
          }
          to="/jobs"
        />

        <StripCell
          label={failures > 0 ? 'Needs attention' : 'Storage used'}
          value={
            failures > 0 ? (
              <>
                <Num>{formatCount(failures)}</Num>{' '}
                <span className="text-[13px] font-normal">failed</span>
              </>
            ) : (
              formatBytes(stats.storageBytes)
            )
          }
          sub={
            failures > 0
              ? 'In the last 24 hours'
              : `${formatBytes(stats.dedupSavedBytes)} saved by dedup`
          }
          to={failures > 0 ? '/monitor' : '/targets'}
          tone={failures > 0 ? 'alert' : 'default'}
        />
      </div>
    </section>
  )
}

/* ---------------------------------------------------------------------------
 * Inventory tiles — navigation, not status, so they stay quiet.
 * ------------------------------------------------------------------------- */

function CountTile({
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
      className="group flex items-center gap-3 rounded-lg border border-slate-800/80 bg-slate-900/35 px-3.5 py-2.5 transition-colors duration-150 hover:border-slate-700 hover:bg-slate-900/70"
    >
      <Icon
        className="size-4 shrink-0 text-slate-600 transition-colors duration-150 group-hover:text-accent-400"
        aria-hidden
      />
      <span className="min-w-0 flex-1 truncate text-meta text-slate-400">{label}</span>
      <Num className="figure-lg shrink-0 text-[17px] leading-5 text-slate-100">
        {formatCount(value)}
      </Num>
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
      {
        key: 'running',
        name: 'Running',
        value: stats.last24h.running,
        fill: OUTCOME_COLORS.running,
      },
    ],
    [stats.last24h],
  )

  const total = slices.reduce((sum, slice) => sum + slice.value, 0)

  return (
    <div className="flex flex-col gap-5 sm:flex-row sm:items-center">
      <div className="relative h-36 w-36 shrink-0 self-center">
        {total === 0 ? (
          <div className="flex size-full items-center justify-center rounded-full border-[6px] border-slate-800/70 text-meta text-slate-600">
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
              <Num className="figure-lg text-2xl text-white">{formatCount(total)}</Num>
              <span className="eyebrow">runs</span>
            </div>
          </>
        )}
      </div>

      <dl className="flex-1 space-y-2.5">
        {slices.map((slice) => (
          <div key={slice.key} className="flex items-center gap-3">
            <span
              className="size-2 shrink-0 rounded-full"
              style={{ background: slice.fill }}
              aria-hidden
            />
            <dt className="flex-1 text-[13px] text-slate-400">{slice.name}</dt>
            <dd className="text-[13px]">
              <Num className="text-slate-100">{formatCount(slice.value)}</Num>
            </dd>
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
      <div className="space-y-5 px-5 py-4">
        <div className="grid gap-4 sm:grid-cols-2">
          <div>
            <p className="eyebrow">Stored on target</p>
            <Num className="figure-lg mt-1 block text-[26px] leading-8 text-white">
              {formatBytes(stored)}
            </Num>
          </div>
          <div>
            <p className="eyebrow">Saved by deduplication</p>
            <Num className="figure-lg mt-1 block text-[26px] leading-8 text-emerald-400">
              {formatBytes(saved)}
            </Num>
          </div>
        </div>

        <div>
          <div className="mb-2 flex items-center justify-between text-meta text-slate-500">
            <span>
              Logical protected data <Num className="text-slate-400">{formatBytes(logical)}</Num>
            </span>
            <span className="text-emerald-400">
              <Num>{clampPct(savedPct).toFixed(0)}%</Num> saved
            </span>
          </div>
          <div className="flex h-2 w-full overflow-hidden rounded-full bg-slate-800">
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
          <div className="mt-2 flex items-center gap-4 text-meta text-slate-500">
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
      <EmptyState
        className="rounded-none border-0 bg-transparent"
        icon={<Activity className="size-5" aria-hidden />}
        title="No backup runs yet"
        description="Create a backup job and run it — every run, restore, and verification lands here."
        action={
          <Link to="/jobs">
            <Button variant="primary" icon={<CalendarClock className="size-4" aria-hidden />}>
              Go to backup jobs
            </Button>
          </Link>
        }
      />
    )
  }

  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[46rem]">
        <thead>
          <tr className="border-b border-slate-800 text-left text-micro tracking-wide text-slate-500 uppercase">
            <th className="py-2 pr-4 pl-5 font-semibold">Job</th>
            <th className="px-4 py-2 font-semibold">Status</th>
            <th className="px-4 py-2 font-semibold">Started</th>
            <th className="px-4 py-2 font-semibold">Duration</th>
            <th className="px-4 py-2 text-right font-semibold">Read</th>
            <th className="px-4 py-2 text-right font-semibold">Uploaded</th>
            <th className="px-4 py-2 text-right font-semibold">Dedup</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-slate-800/70">
          {runs.map((run) => (
            <tr
              key={String(run.id)}
              className="align-middle transition-colors duration-150 hover:bg-slate-800/30"
            >
              <td className="max-w-[16rem] py-2.5 pr-4 pl-5">
                <p className="truncate text-[13px] font-medium text-slate-100">{run.jobName}</p>
                <div className="mt-0.5 flex h-4 items-center gap-2">
                  {run.status === 'running' ? (
                    <>
                      <ProgressBar value={run.progressPct} active className="h-1 max-w-32" />
                      <Num className="shrink-0 text-micro text-accent-300">
                        {clampPct(run.progressPct).toFixed(0)}%
                      </Num>
                    </>
                  ) : run.error ? (
                    <span className="truncate text-micro text-red-400/90" title={run.error}>
                      {run.error}
                    </span>
                  ) : null}
                </div>
              </td>
              <td className="px-4 py-2.5 whitespace-nowrap">
                <RunStatusPill status={run.status} />
              </td>
              <td className="px-4 py-2.5 whitespace-nowrap" title={formatDateTime(run.startedAt)}>
                <Num className="text-meta text-slate-400">{formatRelative(run.startedAt)}</Num>
              </td>
              <td className="px-4 py-2.5 whitespace-nowrap">
                <Num className="text-meta text-slate-400">
                  {formatDuration(run.startedAt, run.finishedAt)}
                </Num>
              </td>
              <td className="px-4 py-2.5 text-right whitespace-nowrap">
                <Num className="text-meta text-slate-300">{formatBytes(run.bytesProcessed)}</Num>
              </td>
              <td className="px-4 py-2.5 text-right whitespace-nowrap">
                <Num className="text-meta text-slate-300">{formatBytes(run.bytesUploaded)}</Num>
              </td>
              <td className="px-4 py-2.5 text-right whitespace-nowrap">
                <Num className="text-meta text-emerald-400">{formatRatio(run.dedupRatio)}</Num>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

export function DashboardPage() {
  const loader = useCallback(async (): Promise<DashboardData> => {
    const [stats, jobs, vms] = await Promise.all([getDashboard(), listJobs(), listVMs()])
    return { stats, jobs, vms }
  }, [])
  const { data, loading, error, reload, refresh } = useAsync(loader)

  const stats = data?.stats
  const hasRunning = Boolean(
    stats && (stats.last24h.running > 0 || stats.recentRuns.some((run) => run.status === 'running')),
  )
  usePolling(() => void refresh(), 2000, hasRunning)

  if (loading && !data) {
    return (
      <>
        <PageHeader title="Dashboard" description="Protection status across your Proxmox estate." />
        <div className="pb-skeleton mb-6 h-[5.5rem] rounded-xl border border-slate-800/70" />
        <SkeletonCards count={5} height="h-16" />
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

  if (!data || !stats) return null

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

      <EstateStrip data={data} />

      <div className="mt-6">
        <SectionHeading title="Inventory" />
        <div className="grid gap-2 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-5">
          <CountTile icon={Laptop} label="Virtual machines" value={stats.vmCount} to="/vms" />
          <CountTile icon={Server} label="Proxmox hosts" value={stats.hostCount} to="/hosts" />
          <CountTile icon={ShieldCheck} label="Agents" value={stats.agentCount} to="/agents" />
          <CountTile
            icon={Database}
            label="Storage targets"
            value={stats.targetCount}
            to="/targets"
          />
          <CountTile icon={CalendarClock} label="Backup jobs" value={stats.jobCount} to="/jobs" />
        </div>
      </div>

      <div className="mt-6">
        <SectionHeading title="Activity" hint="Last 24 hours" />
        <div className="grid gap-4 lg:grid-cols-2">
          <Card>
            <CardHeader
              title="Run outcomes"
              subtitle="Every job run in the past day"
              actions={
                <Link
                  to="/monitor"
                  className="inline-flex items-center gap-1.5 text-meta font-medium text-accent-400 transition-colors duration-150 hover:text-accent-300"
                >
                  <Activity className="size-3.5" aria-hidden />
                  Monitor
                </Link>
              }
            />
            <div className="px-5 py-4">
              <OutcomeChart stats={stats} />
            </div>
          </Card>

          <StorageCard stats={stats} />
        </div>
      </div>

      <div className="mt-6">
        <SectionHeading
          title="Recent runs"
          hint={
            <Link
              to="/monitor"
              className="font-medium text-accent-400 transition-colors duration-150 hover:text-accent-300"
            >
              View all runs
            </Link>
          }
        />
        <Card elevation="flat">
          <RecentRunsTable runs={stats.recentRuns} />
        </Card>
      </div>

      {stats.hostCount === 0 || stats.targetCount === 0 ? (
        <div className="mt-6">
          <EmptyState
            icon={
              stats.hostCount === 0 ? (
                <Server className="size-5" aria-hidden />
              ) : (
                <HardDrive className="size-5" aria-hidden />
              )
            }
            title={stats.hostCount === 0 ? 'Add your first Proxmox host' : 'Add a storage target'}
            description={
              stats.hostCount === 0
                ? 'ProxBack needs an API token for at least one Proxmox VE host before it can inventory virtual machines.'
                : 'Backups are written to S3-compatible storage — Backblaze B2, MinIO, or AWS S3. Add a target to start protecting data.'
            }
            action={
              <Link to={stats.hostCount === 0 ? '/hosts' : '/targets'}>
                <Button variant="primary" icon={<Sparkles className="size-4" aria-hidden />}>
                  {stats.hostCount === 0 ? 'Add Proxmox host' : 'Add storage target'}
                </Button>
              </Link>
            }
          />
        </div>
      ) : null}
    </>
  )
}
