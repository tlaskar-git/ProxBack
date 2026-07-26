import { useCallback, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { Activity, Ban, Clock, RefreshCw, TriangleAlert } from 'lucide-react'
import { cancelRun, errorMessage, listJobs, listRuns } from '../api'
import type { Job, JobRun } from '../api'
import { useConfirm } from '../components/Confirm'
import { useToast } from '../components/Toast'
import {
  Button,
  Card,
  EmptyState,
  ErrorBlock,
  PageHeader,
  ProgressBar,
  RunStatusPill,
  Select,
  SkeletonRows,
} from '../components/ui'
import { useAsync, usePolling } from '../lib/useAsync'
import {
  clampPct,
  formatBytes,
  formatDateTime,
  formatDuration,
  formatRatio,
  formatRelative,
} from '../lib/format'

interface MonitorData {
  runs: JobRun[]
  jobs: Job[]
}

const RUN_LIMIT = 100

function RunCard({ run, onChanged }: { run: JobRun; onChanged: () => void }) {
  const toast = useToast()
  const confirm = useConfirm()
  const [canceling, setCanceling] = useState(false)
  const running = run.status === 'running'

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

    setCanceling(true)
    try {
      await cancelRun(run.id)
      toast.success('Cancellation requested.', 'The engine stops at the next chunk boundary.')
      onChanged()
    } catch (err) {
      toast.error('Could not cancel run', errorMessage(err))
    } finally {
      setCanceling(false)
    }
  }

  return (
    <div className="px-5 py-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <p className="truncate text-sm font-semibold text-slate-100">{run.jobName}</p>
            <RunStatusPill status={run.status} />
          </div>
          <p className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-slate-500">
            <span className="inline-flex items-center gap-1.5">
              <Clock className="size-3.5 text-slate-600" aria-hidden />
              Started {formatDateTime(run.startedAt)}
            </span>
            <span>·</span>
            <span>{formatDuration(run.startedAt, run.finishedAt)}</span>
            {run.finishedAt ? (
              <>
                <span>·</span>
                <span>Finished {formatRelative(run.finishedAt)}</span>
              </>
            ) : null}
          </p>
        </div>

        {running ? (
          <Button
            size="sm"
            variant="danger"
            loading={canceling}
            icon={<Ban className="size-3.5" aria-hidden />}
            onClick={() => void onCancel()}
          >
            Cancel
          </Button>
        ) : null}
      </div>

      {running ? (
        <div className="mt-3.5">
          <div className="mb-1.5 flex items-center justify-between gap-3 text-xs">
            <span className="truncate text-slate-300">{run.currentStep || 'Working…'}</span>
            <span className="shrink-0 font-medium text-accent-300">
              {clampPct(run.progressPct).toFixed(0)}%
            </span>
          </div>
          <ProgressBar value={run.progressPct} active />
        </div>
      ) : run.currentStep ? (
        <p className="mt-3 text-xs text-slate-500">{run.currentStep}</p>
      ) : null}

      <dl className="mt-4 grid grid-cols-2 gap-4 sm:grid-cols-4">
        <div>
          <dt className="text-[11px] tracking-wide text-slate-500 uppercase">Processed</dt>
          <dd className="mt-0.5 text-sm text-slate-200">{formatBytes(run.bytesProcessed)}</dd>
        </div>
        <div>
          <dt className="text-[11px] tracking-wide text-slate-500 uppercase">Uploaded</dt>
          <dd className="mt-0.5 text-sm text-slate-200">{formatBytes(run.bytesUploaded)}</dd>
        </div>
        <div>
          <dt className="text-[11px] tracking-wide text-slate-500 uppercase">Dedup ratio</dt>
          <dd className="mt-0.5 text-sm text-emerald-400">{formatRatio(run.dedupRatio)}</dd>
        </div>
        <div>
          <dt className="text-[11px] tracking-wide text-slate-500 uppercase">Saved</dt>
          <dd className="mt-0.5 text-sm text-slate-200">
            {formatBytes(Math.max(0, run.bytesProcessed - run.bytesUploaded))}
          </dd>
        </div>
      </dl>

      {run.status === 'failed' && run.error ? (
        <div className="mt-4 flex gap-2.5 rounded-lg border border-red-500/30 bg-red-500/5 px-3.5 py-2.5">
          <TriangleAlert className="mt-0.5 size-4 shrink-0 text-red-400" aria-hidden />
          <p className="min-w-0 text-xs leading-relaxed break-words text-red-300">{run.error}</p>
        </div>
      ) : null}
    </div>
  )
}

export function MonitorPage() {
  const [searchParams, setSearchParams] = useSearchParams()
  const jobIdParam = searchParams.get('jobId') ?? 'all'

  const loader = useCallback(async (): Promise<MonitorData> => {
    const [runs, jobs] = await Promise.all([
      listRuns(jobIdParam === 'all' ? { limit: RUN_LIMIT } : { jobId: jobIdParam, limit: RUN_LIMIT }),
      listJobs(),
    ])
    return { runs, jobs }
  }, [jobIdParam])

  const { data, loading, error, reload, refresh } = useAsync(loader)

  const runs = data?.runs ?? []
  const jobs = data?.jobs ?? []
  const anyRunning = runs.some((run) => run.status === 'running')

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
        description="Every backup and restore run, newest first. Running jobs update live."
        actions={
          <>
            <Select
              value={jobIdParam}
              onChange={(event) => onFilterChange(event.target.value)}
              className="w-56"
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
          </>
        }
      />

      {anyRunning ? (
        <p className="mb-4 inline-flex items-center gap-2 rounded-full border border-accent-500/30 bg-accent-500/10 px-3 py-1 text-xs text-accent-300">
          <span className="size-1.5 animate-pulse rounded-full bg-accent-400" aria-hidden />
          Live — refreshing every 2 seconds
        </p>
      ) : null}

      {loading && !data ? (
        <Card>
          <SkeletonRows count={4} />
        </Card>
      ) : error && !data ? (
        <ErrorBlock message={error} onRetry={() => void reload()} />
      ) : runs.length === 0 ? (
        <EmptyState
          icon={<Activity className="size-5" aria-hidden />}
          title={activeJob ? `No runs for “${activeJob.name}” yet` : 'No job runs yet'}
          description={
            activeJob
              ? 'This job has not run. Start it from the Backup Jobs page, or wait for its schedule.'
              : 'Runs appear here as soon as a backup job starts — manually or on its schedule. Restores show up too, named “Restore …”.'
          }
          action={
            jobIdParam === 'all' ? undefined : (
              <Button onClick={() => onFilterChange('all')}>Show all jobs</Button>
            )
          }
        />
      ) : (
        <Card className="divide-y divide-slate-800">
          {runs.map((run) => (
            <RunCard key={String(run.id)} run={run} onChanged={() => void refresh()} />
          ))}
        </Card>
      )}
    </>
  )
}
