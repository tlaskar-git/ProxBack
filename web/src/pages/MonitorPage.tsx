import { useCallback, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { Activity, Ban, RefreshCw } from 'lucide-react'
import { cancelRun, errorMessage, listJobs, listRuns } from '../api'
import type { Job, JobRun } from '../api'
import { useConfirm } from '../components/Confirm'
import { useToast } from '../components/Toast'
import {
  Button,
  Card,
  EmptyState,
  ErrorBlock,
  IconButton,
  Num,
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

/**
 * One run per table row. The second line inside the Job cell has a fixed
 * height so a row never changes size as polling flips it between progress
 * bar, current step, and error text.
 */
function RunRow({ run, onChanged }: { run: JobRun; onChanged: () => void }) {
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
    <tr className="align-top transition-colors duration-150 hover:bg-slate-800/30">
      <td className="max-w-[20rem] px-5 py-3">
        <p className="truncate text-sm font-medium text-slate-100">{run.jobName}</p>
        <div className="mt-1.5 flex h-4 items-center gap-2">
          {running ? (
            <>
              <ProgressBar value={run.progressPct} active className="max-w-36" />
              <Num className="shrink-0 text-[11px] font-medium text-accent-300">
                {clampPct(run.progressPct).toFixed(0)}%
              </Num>
              <span className="truncate text-[11px] text-slate-500">
                {run.currentStep || 'Working…'}
              </span>
            </>
          ) : run.status === 'failed' && run.error ? (
            <span className="truncate text-[11px] text-red-400/90" title={run.error}>
              {run.error}
            </span>
          ) : run.currentStep ? (
            <span className="truncate text-[11px] text-slate-500">{run.currentStep}</span>
          ) : null}
        </div>
      </td>
      <td className="px-5 py-3 whitespace-nowrap">
        <RunStatusPill status={run.status} />
      </td>
      <td className="px-5 py-3 whitespace-nowrap" title={formatDateTime(run.startedAt)}>
        <Num className="text-sm text-slate-400">{formatRelative(run.startedAt)}</Num>
      </td>
      <td className="px-5 py-3 whitespace-nowrap">
        <Num className="text-sm text-slate-400">
          {formatDuration(run.startedAt, run.finishedAt)}
        </Num>
      </td>
      <td className="px-5 py-3 text-right whitespace-nowrap">
        <Num className="text-sm text-slate-300">{formatBytes(run.bytesProcessed)}</Num>
      </td>
      <td className="px-5 py-3 text-right whitespace-nowrap">
        <Num className="text-sm text-slate-300">{formatBytes(run.bytesUploaded)}</Num>
      </td>
      <td className="px-5 py-3 text-right whitespace-nowrap">
        <Num className="text-sm text-emerald-400">{formatRatio(run.dedupRatio)}</Num>
      </td>
      <td className="w-14 px-5 py-3 text-right">
        {running ? (
          <IconButton
            aria-label={`Cancel the run of ${run.jobName}`}
            title="Cancel this run"
            loading={canceling}
            onClick={() => void onCancel()}
          >
            <Ban className="size-4" aria-hidden />
          </IconButton>
        ) : null}
      </td>
    </tr>
  )
}

function RunTable({ runs, onChanged }: { runs: JobRun[]; onChanged: () => void }) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[58rem] text-sm">
        <thead>
          <tr className="border-b border-slate-800 text-left text-[11px] tracking-wide text-slate-500 uppercase">
            <th scope="col" className="px-5 py-2.5 font-medium">
              Job
            </th>
            <th scope="col" className="px-5 py-2.5 font-medium">
              Status
            </th>
            <th scope="col" className="px-5 py-2.5 font-medium">
              Started
            </th>
            <th scope="col" className="px-5 py-2.5 font-medium">
              Duration
            </th>
            <th scope="col" className="px-5 py-2.5 text-right font-medium">
              Processed
            </th>
            <th scope="col" className="px-5 py-2.5 text-right font-medium">
              Uploaded
            </th>
            <th scope="col" className="px-5 py-2.5 text-right font-medium">
              Dedup
            </th>
            <th scope="col" className="w-14 px-5 py-2.5">
              <span className="sr-only">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody className="divide-y divide-slate-800/70">
          {runs.map((run) => (
            <RunRow key={String(run.id)} run={run} onChanged={onChanged} />
          ))}
        </tbody>
      </table>
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
        description="Every backup, restore, and verification run, newest first. Running rows update live."
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
          <SkeletonRows count={5} />
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
              : 'Runs appear here as soon as a backup job starts — manually or on its schedule. Restores and verifications show up too, named “Restore …” and “Verify …”.'
          }
          action={
            jobIdParam === 'all' ? undefined : (
              <Button onClick={() => onFilterChange('all')}>Show all jobs</Button>
            )
          }
        />
      ) : (
        <Card>
          <RunTable runs={runs} onChanged={() => void refresh()} />
        </Card>
      )}
    </>
  )
}
