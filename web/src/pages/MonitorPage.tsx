import { useCallback, useEffect, useMemo, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { Activity, Ban, ChevronRight, Eraser, RefreshCw, RotateCcw } from 'lucide-react'
import { cancelRun, clearRuns, errorMessage, getRun, listJobs, listRuns, retryRun } from '../api'
import type { ClearRunsScope, ID, Job, JobRun, RunDetail } from '../api'
import { useConfirm } from '../components/Confirm'
import { RunDetailModal } from '../components/RunDetailModal'
import { appendSample, LiveRunCard } from '../components/RunLive'
import { useToast } from '../components/Toast'
import {
  Button,
  Card,
  EmptyState,
  ErrorBlock,
  IconButton,
  LiveDot,
  Num,
  PageHeader,
  RunStatusPill,
  SectionHeading,
  Select,
  SkeletonRows,
} from '../components/ui'
import { useAsync, usePolling } from '../lib/useAsync'
import {
  formatBytes,
  formatDateTime,
  formatDuration,
  formatRatio,
  formatRelative,
} from '../lib/format'

interface MonitorData {
  runs: JobRun[]
  jobs: Job[]
  /** Full detail — sources + throughput — for every run currently in flight. */
  live: RunDetail[]
}

const RUN_LIMIT = 100

/* ---------------------------------------------------------------------------
 * History table
 * ------------------------------------------------------------------------- */

function HistoryRow({
  run,
  onChanged,
  onOpen,
}: {
  run: JobRun
  onChanged: () => void
  onOpen: (id: ID) => void
}) {
  const toast = useToast()
  const confirm = useConfirm()
  const [busy, setBusy] = useState<'cancel' | 'retry' | null>(null)
  const running = run.status === 'running'
  const failed = run.status === 'failed'

  const onCancel = async () => {
    const ok = await confirm({
      title: 'Cancel this run?',
      message: (
        <>
          Stop the run of <span className="font-medium text-slate-100">{run.jobName}</span>? Chunks
          already uploaded stay on the target, but no restore point is created for this run.
        </>
      ),
      confirmLabel: 'Cancel run',
      cancelLabel: 'Keep running',
    })
    if (!ok) return

    setBusy('cancel')
    try {
      await cancelRun(run.id)
      toast.success('Cancellation requested.', 'The engine stops at the next chunk boundary.')
      onChanged()
    } catch (err) {
      toast.error('Could not cancel run', errorMessage(err))
    } finally {
      setBusy(null)
    }
  }

  const onRetry = async () => {
    setBusy('retry')
    try {
      await retryRun(run.id)
      toast.success(`Retrying “${run.jobName}”.`, 'The new run appears at the top of this page.')
      onChanged()
    } catch (err) {
      toast.error('Could not retry run', errorMessage(err))
    } finally {
      setBusy(null)
    }
  }

  return (
    <tr
      className="cursor-pointer align-middle transition-colors duration-150 hover:bg-slate-800/40"
      onClick={() => onOpen(run.id)}
      tabIndex={0}
      onKeyDown={(event) => {
        if (event.key === 'Enter' || event.key === ' ') {
          event.preventDefault()
          onOpen(run.id)
        }
      }}
      aria-label={`Open details for the ${run.jobName} run`}
    >
      <td className="max-w-[22rem] py-2.5 pr-4 pl-5">
        <div className="flex items-center gap-2">
          {running ? <LiveDot tone="blue" /> : null}
          <p className="truncate text-[13px] font-medium text-slate-100">{run.jobName}</p>
        </div>
        {/* Fixed-height second line: the row must not resize when polling
            swaps the step text for an error and back. */}
        <div className="mt-0.5 flex h-4 items-center">
          {failed && run.error ? (
            <span className="truncate text-micro text-red-400/90" title={run.error}>
              {run.error}
            </span>
          ) : run.currentStep ? (
            <span className="truncate text-micro text-slate-500">{run.currentStep}</span>
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
      <td className="w-20 px-4 py-2.5" onClick={(event) => event.stopPropagation()}>
        <div className="flex items-center justify-end gap-1">
          {failed ? (
            <IconButton
              aria-label={`Retry the ${run.jobName} run`}
              title="Run this job again"
              loading={busy === 'retry'}
              onClick={() => void onRetry()}
            >
              <RotateCcw className="size-4" aria-hidden />
            </IconButton>
          ) : null}
          {running ? (
            <IconButton
              aria-label={`Cancel the run of ${run.jobName}`}
              title="Cancel this run"
              loading={busy === 'cancel'}
              onClick={() => void onCancel()}
            >
              <Ban className="size-4" aria-hidden />
            </IconButton>
          ) : (
            <ChevronRight className="size-4 text-slate-600" aria-hidden />
          )}
        </div>
      </td>
    </tr>
  )
}

function HistoryTable({
  runs,
  onChanged,
  onOpen,
}: {
  runs: JobRun[]
  onChanged: () => void
  onOpen: (id: ID) => void
}) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[58rem]">
        <thead>
          <tr className="border-b border-slate-800 text-left text-micro tracking-wide text-slate-500 uppercase">
            <th scope="col" className="py-2 pr-4 pl-5 font-semibold">
              Job
            </th>
            <th scope="col" className="px-4 py-2 font-semibold">
              Status
            </th>
            <th scope="col" className="px-4 py-2 font-semibold">
              Started
            </th>
            <th scope="col" className="px-4 py-2 font-semibold">
              Duration
            </th>
            <th scope="col" className="px-4 py-2 text-right font-semibold">
              Read
            </th>
            <th scope="col" className="px-4 py-2 text-right font-semibold">
              Uploaded
            </th>
            <th scope="col" className="px-4 py-2 text-right font-semibold">
              Dedup
            </th>
            {/* `relative` keeps the visually-hidden label's containing block
                inside the scroll container, so it cannot widen the page. */}
            <th scope="col" className="relative w-20 px-4 py-2">
              <span className="sr-only">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody className="divide-y divide-slate-800/70">
          {runs.map((run) => (
            <HistoryRow key={String(run.id)} run={run} onChanged={onChanged} onOpen={onOpen} />
          ))}
        </tbody>
      </table>
    </div>
  )
}

/* ---------------------------------------------------------------------------
 * Page
 * ------------------------------------------------------------------------- */

export function MonitorPage() {
  const [searchParams, setSearchParams] = useSearchParams()
  const jobIdParam = searchParams.get('jobId') ?? 'all'

  const loader = useCallback(async (): Promise<MonitorData> => {
    const [runs, jobs] = await Promise.all([
      listRuns(jobIdParam === 'all' ? { limit: RUN_LIMIT } : { jobId: jobIdParam, limit: RUN_LIMIT }),
      listJobs(),
    ])
    // Only in-flight runs need the heavier detail payload; finished ones are
    // fully described by the list row until the operator opens them.
    const settled = await Promise.all(
      runs
        .filter((run) => run.status === 'running')
        .map((run) => getRun(run.id).catch(() => null)),
    )
    return { runs, jobs, live: settled.filter((detail): detail is RunDetail => detail !== null) }
  }, [jobIdParam])

  const { data, loading, error, reload, refresh } = useAsync(loader)
  const toast = useToast()
  const confirm = useConfirm()
  const [openRun, setOpenRun] = useState<ID | null>(null)
  const [clearing, setClearing] = useState(false)
  const [samples, setSamples] = useState<Record<string, number[]>>({})

  const runs = useMemo(() => data?.runs ?? [], [data])
  const jobs = data?.jobs ?? []
  const live = useMemo(() => data?.live ?? [], [data])
  const anyRunning = live.length > 0 || runs.some((run) => run.status === 'running')

  // One throughput sample per poll, per live run. Runs that end drop out of
  // the map so it never grows without bound.
  useEffect(() => {
    if (!data) return
    setSamples((current) => {
      const next: Record<string, number[]> = {}
      for (const detail of data.live) {
        const key = String(detail.id)
        next[key] = appendSample(current[key], detail.throughputBps)
      }
      return next
    })
  }, [data])

  const liveIds = useMemo(() => new Set(live.map((detail) => String(detail.id))), [live])
  // Runs rendered as a live card are not repeated in the table below it.
  const history = useMemo(
    () => runs.filter((run) => !liveIds.has(String(run.id))),
    [runs, liveIds],
  )
  const finishedCount = runs.filter((run) => run.status !== 'running').length

  const onClear = async (scope: ClearRunsScope) => {
    const failedOnly = scope === 'failed'
    const ok = await confirm({
      title: failedOnly ? 'Clear failed runs' : 'Clear finished runs',
      message: (
        <>
          Remove {failedOnly ? 'every failed run' : 'every finished run'}
          {jobIdParam === 'all' ? '' : ' of this job'} from the history, along with their activity
          logs? Restore points and backup data are not affected — running runs are left alone.
        </>
      ),
      confirmLabel: failedOnly ? 'Clear failed' : 'Clear finished',
    })
    if (!ok) return
    setClearing(true)
    try {
      const result = await clearRuns(scope, jobIdParam === 'all' ? undefined : jobIdParam)
      toast.success(
        result.deleted === 1
          ? '1 run removed from history.'
          : `${result.deleted} runs removed from history.`,
      )
      await reload()
    } catch (err) {
      toast.error('Could not clear run history', errorMessage(err))
    } finally {
      setClearing(false)
    }
  }

  const onCancelLive = async (detail: RunDetail) => {
    const ok = await confirm({
      title: 'Cancel this run?',
      message: (
        <>
          Stop the run of <span className="font-medium text-slate-100">{detail.jobName}</span>?
          Chunks already uploaded stay on the target, but no restore point is created for this run.
        </>
      ),
      confirmLabel: 'Cancel run',
      cancelLabel: 'Keep running',
    })
    if (!ok) return
    try {
      await cancelRun(detail.id)
      toast.success('Cancellation requested.', 'The engine stops at the next chunk boundary.')
      await refresh()
    } catch (err) {
      toast.error('Could not cancel run', errorMessage(err))
    }
  }

  // Live progress: poll every 2 s while at least one run is in flight.
  usePolling(() => void refresh(), 2000, anyRunning)

  const onFilterChange = (value: string) => {
    if (value === 'all') setSearchParams({}, { replace: true })
    else setSearchParams({ jobId: value }, { replace: true })
  }

  const activeJob = jobs.find((job) => String(job.id) === jobIdParam)

  return (
    <>
      <PageHeader
        title="Monitor"
        description="Runs in flight, object by object. Finished runs drop into the history below."
        actions={
          <>
            <Select
              value={jobIdParam}
              onChange={(event) => onFilterChange(event.target.value)}
              className="w-52"
              aria-label="Filter runs by job"
            >
              <option value="all">All jobs</option>
              {jobs.map((job) => (
                <option key={String(job.id)} value={String(job.id)}>
                  {job.name}
                </option>
              ))}
            </Select>
            <Button
              icon={<RefreshCw className="size-4" aria-hidden />}
              onClick={() => void reload()}
              loading={loading}
            >
              Refresh
            </Button>
            {finishedCount > 0 ? (
              <Button
                variant="dangerQuiet"
                icon={<Eraser className="size-4" aria-hidden />}
                loading={clearing}
                onClick={() => void onClear('finished')}
                title="Remove finished runs from the history"
              >
                Clear history
              </Button>
            ) : null}
          </>
        }
      />

      {loading && !data ? (
        <Card>
          <SkeletonRows count={5} />
        </Card>
      ) : error && !data ? (
        <ErrorBlock message={error} onRetry={() => void reload()} />
      ) : runs.length === 0 ? (
        <EmptyState
          icon={<Activity className="size-5" aria-hidden />}
          title={activeJob ? `No runs for “${activeJob.name}” yet` : 'Nothing has run yet'}
          description={
            activeJob
              ? 'This job has not run. Start it from the Backup Jobs page, or wait for its schedule.'
              : 'A run appears here the moment a backup job starts, manually or on schedule. Restores and verifications show up too.'
          }
          hint="Restores are named “Restore …”, verifications “Verify …”."
          action={
            jobIdParam === 'all' ? undefined : (
              <Button onClick={() => onFilterChange('all')}>Show all jobs</Button>
            )
          }
        />
      ) : (
        <div className="space-y-6">
          {live.length > 0 ? (
            <section>
              <SectionHeading
                title="In flight"
                count={live.length}
                hint="Refreshing every 2 seconds"
              />
              <div className="space-y-4">
                {live.map((detail) => (
                  <LiveRunCard
                    key={String(detail.id)}
                    detail={detail}
                    samples={samples[String(detail.id)] ?? []}
                    onOpen={() => setOpenRun(detail.id)}
                    actions={
                      <Button
                        size="sm"
                        variant="danger"
                        icon={<Ban className="size-3.5" aria-hidden />}
                        onClick={() => void onCancelLive(detail)}
                      >
                        Cancel
                      </Button>
                    }
                  />
                ))}
              </div>
            </section>
          ) : null}

          {history.length > 0 ? (
            <section>
              <SectionHeading
                title="History"
                count={history.length}
                hint={activeJob ? activeJob.name : 'All jobs, newest first'}
              />
              <Card elevation="flat">
                <HistoryTable
                  runs={history}
                  onChanged={() => void refresh()}
                  onOpen={setOpenRun}
                />
              </Card>
            </section>
          ) : null}
        </div>
      )}

      <RunDetailModal
        runId={openRun}
        onClose={() => setOpenRun(null)}
        onChanged={() => void refresh()}
      />
    </>
  )
}
