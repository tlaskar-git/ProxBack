import { lazy, Suspense, useCallback, useMemo, useState } from 'react'
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
} from 'lucide-react'
import type { LucideIcon } from 'lucide-react'
import { errorMessage, getDashboard, getPosture, listJobs, reductionOf } from '../api'
import type {
  DashboardStats,
  Job,
  JobRun,
  Posture,
  PostureVerdict,
  PostureWorkload,
  WorkloadStatus,
} from '../api'
import { WorkloadIdentity } from '../components/Identity'
import {
  Button,
  Card,
  CardHeader,
  ChipButton,
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
  StatusPill,
} from '../components/ui'
import type { PillTone } from '../components/ui'
import { useAsync, usePolling } from '../lib/useAsync'
import { cn } from '../lib/cn'
import {
  clampPct,
  describeRpo,
  formatBytes,
  formatCount,
  formatDateTime,
  formatDuration,
  formatRatio,
  formatReduction,
  formatRelative,
} from '../lib/format'

interface DashboardData {
  stats: DashboardStats
  jobs: Job[]
  /** null when the server could not evaluate posture — never faked locally. */
  posture: Posture | null
  postureError: string | null
}

/* ---------------------------------------------------------------------------
 * Verdict
 *
 * The verdict is the server's, computed per workload against each workload's
 * own RPO. The console's whole job here is to *show its working*: the
 * headline, the counts behind it, and every reason that produced it. Nothing
 * on this page asserts health the posture payload did not state.
 * ------------------------------------------------------------------------- */

const VERDICT_LABEL: Record<PostureVerdict, string> = {
  protected: 'Protected',
  at_risk: 'At risk',
  unprotected: 'Unprotected',
  unknown: 'Unknown',
}

const VERDICT_TONE: Record<PostureVerdict, PillTone> = {
  protected: 'ok',
  at_risk: 'warn',
  unprotected: 'fail',
  unknown: 'neutral',
}

const VERDICT_WASH: Record<PillTone, string> = {
  ok: 'border-ok-500/30 bg-ok-500/[0.07]',
  warn: 'border-warn-500/30 bg-warn-500/[0.07]',
  fail: 'border-fail-500/30 bg-fail-500/[0.07]',
  brand: 'border-accent-500/30 bg-accent-500/[0.07]',
  neutral: 'border-slate-800 bg-slate-900/60',
}

const VERDICT_INK: Record<PillTone, string> = {
  ok: 'text-ok-300',
  warn: 'text-warn-300',
  fail: 'text-fail-300',
  brand: 'text-accent-300',
  neutral: 'text-slate-300',
}

const STATUS_TONE: Record<WorkloadStatus, PillTone> = {
  protected: 'ok',
  at_risk: 'warn',
  unprotected: 'fail',
}

const STATUS_LABEL: Record<WorkloadStatus, string> = {
  protected: 'Protected',
  at_risk: 'At risk',
  unprotected: 'Unprotected',
}

/** What an operator should do next when there is nothing to evaluate. */
function setupRoute(stats: DashboardStats): { label: string; to: string; hint: string } {
  if (stats.hostCount === 0) {
    return {
      label: 'Add a Proxmox host',
      to: '/hosts',
      hint: 'No cluster is connected yet, so there is nothing to evaluate.',
    }
  }
  if (stats.targetCount === 0) {
    return {
      label: 'Add a storage target',
      to: '/targets',
      hint: 'Backups need somewhere to go before any workload can be protected.',
    }
  }
  if (stats.jobCount === 0) {
    return {
      label: 'Create a backup job',
      to: '/jobs',
      hint: 'No workload is covered by a job, so no recovery point exists to evaluate.',
    }
  }
  return {
    label: 'Review backup jobs',
    to: '/jobs',
    hint: 'No job has completed a run yet, so there is no evidence to report.',
  }
}

function VerdictPanel({
  posture,
  stats,
  running,
}: {
  posture: Posture
  stats: DashboardStats
  running: number
}) {
  const tone = VERDICT_TONE[posture.verdict]
  const { counts } = posture
  const total = counts.protected + counts.atRisk + counts.unprotected
  const empty = posture.verdict === 'unknown' || total === 0
  const route = setupRoute(stats)

  return (
    <section className={cn('rounded-xl border', VERDICT_WASH[tone])} aria-label="Protection posture">
      <div className="flex flex-wrap items-start gap-x-6 gap-y-4 px-5 py-4">
        <div className="min-w-0 flex-1">
          <p className="eyebrow">Estate verdict</p>
          <p
            className={cn(
              'mt-1 text-[22px] leading-7 font-semibold tracking-tight',
              VERDICT_INK[tone],
            )}
          >
            {VERDICT_LABEL[posture.verdict]}
          </p>
          {empty ? (
            <p className="mt-1 max-w-xl text-[13px] leading-5 text-slate-400">{route.hint}</p>
          ) : (
            <p className="mt-1 text-[13px] text-slate-400">
              <Num className="text-slate-200">{formatCount(counts.protected)}</Num> protected ·{' '}
              <Num className={counts.atRisk > 0 ? 'text-warn-300' : 'text-slate-400'}>
                {formatCount(counts.atRisk)}
              </Num>{' '}
              at risk ·{' '}
              <Num className={counts.unprotected > 0 ? 'text-fail-300' : 'text-slate-400'}>
                {formatCount(counts.unprotected)}
              </Num>{' '}
              unprotected
            </p>
          )}
        </div>

        {running > 0 ? (
          <span className="flex shrink-0 items-center gap-2 text-meta text-slate-400">
            <LiveDot tone="brand" />
            <Num>{running}</Num> running now
          </span>
        ) : null}

        {empty ? (
          <Link to={route.to} className="shrink-0">
            <Button variant="primary">{route.label}</Button>
          </Link>
        ) : null}
      </div>

      {posture.reasons.length > 0 ? (
        <ul className="divide-y divide-slate-800/70 border-t border-slate-800/70">
          {posture.reasons.map((reason) => (
            <li
              key={reason.code}
              className="flex flex-wrap items-baseline gap-x-3 gap-y-0.5 px-5 py-2"
            >
              <Num className="shrink-0 text-[13px] font-semibold text-slate-100">
                {formatCount(reason.workloads)}
              </Num>
              <span className="min-w-0 flex-1 text-[13px] text-slate-300">{reason.detail}</span>
              <span className="shrink-0 font-mono text-micro text-slate-600">{reason.code}</span>
            </li>
          ))}
        </ul>
      ) : null}
    </section>
  )
}

/* ---------------------------------------------------------------------------
 * Workload posture table
 *
 * One row per workload with the evidence that decided its status: the last
 * success and how it sits against that workload's own RPO, the last integrity
 * verification, and how many restore points stand behind it.
 * ------------------------------------------------------------------------- */

type PostureFilter = 'all' | WorkloadStatus

const FILTERS: { value: PostureFilter; label: string }[] = [
  { value: 'all', label: 'All' },
  { value: 'at_risk', label: 'At risk' },
  { value: 'unprotected', label: 'Unprotected' },
  { value: 'protected', label: 'Protected' },
]

const STATUS_WEIGHT: Record<WorkloadStatus, number> = {
  unprotected: 0,
  at_risk: 1,
  protected: 2,
}

function WorkloadTable({ workloads }: { workloads: PostureWorkload[] }) {
  const [filter, setFilter] = useState<PostureFilter>('all')

  const counts = useMemo(() => {
    const map: Record<PostureFilter, number> = {
      all: workloads.length,
      protected: 0,
      at_risk: 0,
      unprotected: 0,
    }
    for (const workload of workloads) map[workload.status] += 1
    return map
  }, [workloads])

  // Rows that need a human sort to the top; nothing else re-orders on poll.
  const rows = useMemo(
    () =>
      (filter === 'all' ? workloads : workloads.filter((item) => item.status === filter))
        .slice()
        .sort((a, b) => {
          if (STATUS_WEIGHT[a.status] !== STATUS_WEIGHT[b.status]) {
            return STATUS_WEIGHT[a.status] - STATUS_WEIGHT[b.status]
          }
          return a.name.localeCompare(b.name)
        }),
    [workloads, filter],
  )

  return (
    <Card elevation="flat">
      <div className="flex flex-wrap items-center gap-1.5 border-b border-slate-800/80 px-5 py-2.5">
        {FILTERS.map((option) => (
          <ChipButton
            key={option.value}
            selected={filter === option.value}
            onClick={() => setFilter(option.value)}
          >
            {option.label}
            <span className="ml-1 text-slate-600">{counts[option.value]}</span>
          </ChipButton>
        ))}
      </div>

      {rows.length === 0 ? (
        <p className="px-5 py-10 text-center text-[13px] text-slate-500">
          No workload is in this state.
        </p>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full min-w-[62rem]">
            <thead>
              <tr className="border-b border-slate-800 text-left text-micro tracking-wide text-slate-500 uppercase">
                <th scope="col" className="py-2 pr-4 pl-5 font-semibold">
                  Workload
                </th>
                <th scope="col" className="px-4 py-2 font-semibold">
                  Policy
                </th>
                <th scope="col" className="px-4 py-2 font-semibold">
                  Last successful backup
                </th>
                <th scope="col" className="px-4 py-2 font-semibold">
                  Last verification
                </th>
                <th scope="col" className="px-4 py-2 text-right font-semibold">
                  Restore points
                </th>
                <th scope="col" className="px-4 py-2 font-semibold">
                  Status
                </th>
              </tr>
            </thead>
            <tbody className="divide-y divide-slate-800/70">
              {rows.map((workload) => {
                const rpo = describeRpo(workload.ageHours, workload.rpoHours)
                const overdue = workload.withinRpo === false
                return (
                  <tr
                    key={`${workload.kind}:${String(workload.id)}`}
                    className="align-top transition-colors duration-150 hover:bg-slate-800/30"
                  >
                    <td className="max-w-[20rem] py-2.5 pr-4 pl-5">
                      <WorkloadIdentity
                        emphasis="strong"
                        workload={{
                          hostName: workload.hostName,
                          name: workload.name,
                          vmid: workload.vmid ?? null,
                          node: workload.node,
                        }}
                      />
                    </td>

                    <td className="px-4 py-2.5">
                      {workload.policy ? (
                        <>
                          <p className="truncate text-[13px] text-slate-300">{workload.policy}</p>
                          {workload.enabled ? null : (
                            <p className="mt-0.5 text-micro text-warn-300">Disabled</p>
                          )}
                        </>
                      ) : (
                        <span className="text-[13px] text-fail-300">No policy</span>
                      )}
                    </td>

                    <td className="px-4 py-2.5 whitespace-nowrap">
                      {workload.lastSuccessAt ? (
                        <>
                          <Num
                            className="block text-[13px] text-slate-200"
                            title={formatDateTime(workload.lastSuccessAt)}
                          >
                            {formatRelative(workload.lastSuccessAt)}
                          </Num>
                          {rpo ? (
                            <span
                              className={cn(
                                'mt-0.5 block text-micro',
                                overdue ? 'text-warn-300' : 'text-slate-500',
                              )}
                            >
                              {rpo}
                            </span>
                          ) : null}
                        </>
                      ) : (
                        <span className="text-[13px] text-fail-300">Never</span>
                      )}
                      {workload.lastFailureAt ? (
                        <span className="mt-0.5 block text-micro text-fail-300">
                          Last run failed {formatRelative(workload.lastFailureAt)}
                        </span>
                      ) : null}
                    </td>

                    <td className="px-4 py-2.5 whitespace-nowrap">
                      {workload.lastVerifiedAt ? (
                        <Num
                          className="text-[13px] text-slate-300"
                          title={`Integrity verified ${formatDateTime(workload.lastVerifiedAt)}`}
                        >
                          {formatRelative(workload.lastVerifiedAt)}
                        </Num>
                      ) : (
                        <span className="text-[13px] text-slate-500">Not verified</span>
                      )}
                    </td>

                    <td className="px-4 py-2.5 text-right whitespace-nowrap">
                      <Num
                        className={cn(
                          'text-[13px]',
                          workload.restorePoints === 0 ? 'text-fail-300' : 'text-slate-200',
                        )}
                      >
                        {formatCount(workload.restorePoints)}
                      </Num>
                    </td>

                    <td className="px-4 py-2.5 whitespace-nowrap">
                      <StatusPill
                        tone={STATUS_TONE[workload.status]}
                        label={STATUS_LABEL[workload.status]}
                      />
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}

      <p className="border-t border-slate-800/80 px-5 py-2.5 text-meta text-slate-500">
        Verified means the restore point was re-read from the target and re-hashed end to end.
        Restore testing is not performed, and is never implied here.
      </p>
    </Card>
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

function OutcomeChart({ stats }: { stats: DashboardStats }) {
  // Slice colours come from the theme variables, so the chart follows the
  // light/dark switch like everything else.
  const slices = useMemo(
    () => [
      {
        key: 'succeeded',
        name: 'Succeeded',
        value: stats.last24h.succeeded,
        fill: 'var(--color-ok-500)',
      },
      {
        key: 'failed',
        name: 'Failed',
        value: stats.last24h.failed,
        fill: 'var(--color-fail-500)',
      },
      {
        key: 'running',
        name: 'Running',
        value: stats.last24h.running,
        fill: 'var(--color-accent-500)',
      },
    ],
    [stats.last24h],
  )

  const total = slices.reduce((sum, slice) => sum + slice.value, 0)

  return (
    <div className="flex flex-col gap-5 sm:flex-row sm:items-center">
      <div className="relative h-32 w-32 shrink-0 self-center">
        {total === 0 ? (
          <div className="flex size-full items-center justify-center rounded-full border-[6px] border-slate-800/70 text-meta text-slate-500">
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
              <Num className="figure-lg text-2xl text-slate-50">{formatCount(total)}</Num>
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
  const avoided = Math.max(0, stats.dedupSavedBytes)
  const logical = stored + avoided
  const avoidedPct = logical > 0 ? (avoided / logical) * 100 : 0

  return (
    <Card>
      <CardHeader title="Storage" subtitle="Across every configured target" />
      <div className="space-y-5 px-5 py-4">
        <div className="grid gap-4 sm:grid-cols-2">
          <div>
            <p className="eyebrow">Stored on target</p>
            <Num className="figure-lg mt-1 block text-[26px] leading-8 text-slate-50">
              {formatBytes(stored)}
            </Num>
          </div>
          <div>
            <p className="eyebrow">Avoided by data reduction</p>
            <Num className="figure-lg mt-1 block text-[26px] leading-8 text-slate-50">
              {formatBytes(avoided)}
            </Num>
          </div>
        </div>

        <div>
          <div className="mb-2 flex items-center justify-between text-meta text-slate-500">
            <span>
              Source data protected <Num className="text-slate-400">{formatBytes(logical)}</Num>
            </span>
            <span>
              <Num className="text-slate-300">{clampPct(avoidedPct).toFixed(0)}%</Num> avoided
            </span>
          </div>
          <div className="flex h-2 w-full overflow-hidden rounded-full bg-slate-800">
            <div
              className="h-full bg-accent-500"
              style={{ width: `${100 - clampPct(avoidedPct)}%` }}
              aria-hidden
            />
            <div
              className="h-full bg-slate-600"
              style={{ width: `${clampPct(avoidedPct)}%` }}
              aria-hidden
            />
          </div>
          <div className="mt-2 flex items-center gap-4 text-meta text-slate-500">
            <span className="flex items-center gap-1.5">
              <span className="size-2 rounded-full bg-accent-500" aria-hidden />
              Transferred to target
            </span>
            <span className="flex items-center gap-1.5">
              <span className="size-2 rounded-full bg-slate-600" aria-hidden />
              Avoided
            </span>
          </div>
        </div>
      </div>
    </Card>
  )
}

/* ---------------------------------------------------------------------------
 * Recent runs
 * ------------------------------------------------------------------------- */

/** The percentage always; the ratio only when the run actually has one. */
function ReductionCell({ run }: { run: JobRun }) {
  const reduction = reductionOf(run)
  return (
    <span className="inline-flex items-baseline gap-1.5">
      <Num className="text-meta text-slate-200">{formatReduction(reduction.pct)}</Num>
      {reduction.ratio === null ? null : (
        <Num className="text-micro text-slate-500">{formatRatio(reduction.ratio)}</Num>
      )}
    </span>
  )
}

function RecentRunsTable({ runs }: { runs: JobRun[] }) {
  if (runs.length === 0) {
    return (
      <EmptyState
        className="rounded-none border-0 bg-transparent"
        icon={<Activity className="size-5" aria-hidden />}
        title="No backup runs yet"
        description="Every run, restore and verification lands here the moment it starts."
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
      <table className="w-full min-w-[54rem]">
        <thead>
          <tr className="border-b border-slate-800 text-left text-micro tracking-wide text-slate-500 uppercase">
            <th className="py-2 pr-4 pl-5 font-semibold">Job</th>
            <th className="px-4 py-2 font-semibold">Status</th>
            <th className="px-4 py-2 font-semibold">Started</th>
            <th className="px-4 py-2 font-semibold">Duration</th>
            <th className="px-4 py-2 text-right font-semibold">Source data processed</th>
            <th className="px-4 py-2 text-right font-semibold">Transferred to target</th>
            <th className="px-4 py-2 text-right font-semibold">Data reduction</th>
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
                    <span className="truncate text-micro text-fail-400" title={run.error}>
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
                <ReductionCell run={run} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

/* ---------------------------------------------------------------------------
 * Page
 * ------------------------------------------------------------------------- */

export function DashboardPage() {
  const loader = useCallback(async (): Promise<DashboardData> => {
    const [stats, jobs, posture] = await Promise.all([
      getDashboard(),
      listJobs(),
      // A server that cannot evaluate posture must not produce a green light
      // by omission — the failure is surfaced instead of swallowed.
      getPosture().then(
        (value) => ({ value, error: null as string | null }),
        (err: unknown) => ({ value: null, error: errorMessage(err) }),
      ),
    ])
    return { stats, jobs, posture: posture.value, postureError: posture.error }
  }, [])
  const { data, loading, error, reload, refresh } = useAsync(loader)

  const stats = data?.stats
  const hasRunning = Boolean(
    stats && (stats.last24h.running > 0 || stats.recentRuns.some((run) => run.status === 'running')),
  )
  usePolling(() => void refresh(), 2000, hasRunning)

  const nextRun = useMemo(() => {
    const jobs = data?.jobs ?? []
    return (
      jobs
        .filter((job) => job.enabled && job.nextRun)
        .sort(
          (a, b) =>
            new Date(a.nextRun as string).getTime() - new Date(b.nextRun as string).getTime(),
        )[0] ?? null
    )
  }, [data?.jobs])

  if (loading && !data) {
    return (
      <>
        <PageHeader title="Dashboard" description="Whether this estate is recoverable, and why." />
        <div className="pb-skeleton mb-6 h-32 rounded-xl border border-slate-800/70" />
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
        description="Whether this estate is recoverable, and why."
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

      {data.posture ? (
        <VerdictPanel posture={data.posture} stats={stats} running={stats.last24h.running} />
      ) : (
        <ErrorBlock
          title="Protection posture could not be evaluated"
          message={
            data.postureError ??
            'The server did not answer the posture request. Until it does, this console cannot state whether the estate is recoverable.'
          }
          onRetry={() => void reload()}
        />
      )}

      {data.posture && data.posture.workloads.length > 0 ? (
        <div className="mt-6">
          <SectionHeading
            title="Workload posture"
            count={data.posture.workloads.length}
            hint={
              nextRun?.nextRun
                ? `Next run ${formatRelative(nextRun.nextRun)} · ${nextRun.name}`
                : 'No enabled job has a schedule'
            }
          />
          <WorkloadTable workloads={data.posture.workloads} />
        </div>
      ) : null}

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
                ? 'ProxBack needs an API token for at least one Proxmox VE host before it can inventory workloads.'
                : 'Backups are written to S3-compatible storage — Backblaze B2, MinIO, or AWS S3.'
            }
            action={
              <Link to={stats.hostCount === 0 ? '/hosts' : '/targets'}>
                <Button variant="primary">
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
