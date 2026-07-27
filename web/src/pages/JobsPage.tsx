import { useCallback, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import {
  Activity,
  CalendarClock,
  Clock,
  Database,
  Laptop,
  Pencil,
  Play,
  Plus,
  RefreshCw,
  ShieldCheck,
  Tag,
  Trash2,
} from 'lucide-react'
import {
  agentSourceOf,
  deleteJob,
  errorMessage,
  listAgents,
  listJobs,
  listTargets,
  listVMs,
  patchJob,
  runJob,
  vmSourcesOf,
} from '../api'
import type { Agent, CachedVM, Job, Target } from '../api'
import { JobWizard } from '../components/JobWizard'
import { useConfirm } from '../components/Confirm'
import { useToast } from '../components/Toast'
import {
  Button,
  Card,
  Chip,
  EmptyState,
  ErrorBlock,
  IconButton,
  Num,
  PageHeader,
  ProgressBar,
  RunStatusPill,
  SkeletonRows,
  StatusPill,
  Toggle,
} from '../components/ui'
import { useAsync, usePolling } from '../lib/useAsync'
import {
  clampPct,
  describeNextRun,
  describeSchedule,
  formatBytes,
  formatRelative,
} from '../lib/format'

interface JobsData {
  jobs: Job[]
  targets: Target[]
  vms: CachedVM[]
  agents: Agent[]
}

function sourceSummary(job: Job, agents: Agent[]): string {
  if (job.kind === 'vm') {
    const sources = vmSourcesOf(job)
    if (sources.length === 0) return 'No sources'
    if (sources.length <= 3) return sources.map((source) => source.name).join(', ')
    return `${sources
      .slice(0, 2)
      .map((source) => source.name)
      .join(', ')} + ${sources.length - 2} more`
  }
  const source = agentSourceOf(job)
  if (!source) return 'No sources'
  const agent = agents.find((item) => String(item.id) === String(source.agentId))
  const paths = source.paths.length
  return `${agent?.hostname ?? `Agent ${source.agentId}`} — ${paths} ${paths === 1 ? 'path' : 'paths'}`
}

function JobRow({
  job,
  agents,
  onChanged,
  onEdit,
}: {
  job: Job
  agents: Agent[]
  onChanged: () => void
  onEdit: () => void
}) {
  const toast = useToast()
  const confirm = useConfirm()
  const navigate = useNavigate()
  const [busy, setBusy] = useState<'run' | 'toggle' | 'delete' | null>(null)

  const lastRun = job.lastRun
  const running = lastRun?.status === 'running'

  const onRun = async () => {
    setBusy('run')
    try {
      await runJob(job.id)
      toast.success(`Job “${job.name}” started.`, 'Follow the progress on the Monitor page.')
      onChanged()
      navigate(`/monitor?jobId=${encodeURIComponent(String(job.id))}`)
    } catch (err) {
      toast.error('Could not start job', errorMessage(err))
    } finally {
      setBusy(null)
    }
  }

  const onToggle = async (enabled: boolean) => {
    setBusy('toggle')
    try {
      await patchJob(job.id, { enabled })
      toast.success(enabled ? `Job “${job.name}” enabled.` : `Job “${job.name}” disabled.`)
      onChanged()
    } catch (err) {
      toast.error('Could not update job', errorMessage(err))
    } finally {
      setBusy(null)
    }
  }

  const onDelete = async () => {
    const ok = await confirm({
      title: 'Delete backup job',
      message: (
        <>
          Delete <span className="font-medium text-slate-100">{job.name}</span>? The schedule and run
          history go away. Restore points already written to the target are kept.
        </>
      ),
      confirmLabel: 'Delete job',
    })
    if (!ok) return

    setBusy('delete')
    try {
      await deleteJob(job.id)
      toast.success(`Job “${job.name}” deleted.`)
      onChanged()
    } catch (err) {
      toast.error('Could not delete job', errorMessage(err))
    } finally {
      setBusy(null)
    }
  }

  return (
    <div className="px-5 py-4">
      <div className="flex flex-wrap items-start gap-4">
        <div
          className={`flex size-9 shrink-0 items-center justify-center rounded-lg border border-slate-800 bg-slate-950/60 ${
            job.enabled ? 'text-accent-400' : 'text-slate-600'
          }`}
        >
          {job.kind === 'vm' ? (
            <Laptop className="size-4" aria-hidden />
          ) : (
            <ShieldCheck className="size-4" aria-hidden />
          )}
        </div>

        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <p className="truncate text-sm font-semibold text-slate-100">{job.name}</p>
            <StatusPill
              tone={job.kind === 'vm' ? 'blue' : 'slate'}
              label={job.kind === 'vm' ? 'VM image' : 'Agent files'}
            />
            {!job.enabled ? <StatusPill tone="amber" label="disabled" /> : null}
          </div>

          {job.kind === 'vm' && job.tagFilter ? (
            <p className="mt-1.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-slate-500">
              <Chip icon={<Tag className="size-2.5 shrink-0" aria-hidden />} title="Proxmox tag">
                {job.tagFilter}
              </Chip>
              <span>every VM carrying this tag, resolved at run time</span>
            </p>
          ) : (
            <p className="mt-1 truncate text-xs text-slate-500">{sourceSummary(job, agents)}</p>
          )}

          <div className="mt-2.5 flex flex-wrap items-center gap-x-5 gap-y-1.5 text-xs text-slate-500">
            <span className="inline-flex items-center gap-1.5">
              <Database className="size-3.5 text-slate-600" aria-hidden />
              {job.targetName || 'No target'}
            </span>
            <span className="inline-flex items-center gap-1.5">
              <CalendarClock className="size-3.5 text-slate-600" aria-hidden />
              {describeSchedule(job.schedule)}
            </span>
            <span className="inline-flex items-center gap-1.5">
              <Clock className="size-3.5 text-slate-600" aria-hidden />
              {describeNextRun(job.nextRun, job)}
            </span>
            <span>
              Keep last <Num>{job.retention}</Num>
            </span>
            {lastRun ? (
              <span className="inline-flex items-center gap-2">
                <RunStatusPill status={lastRun.status} />
                <span>{formatRelative(lastRun.startedAt)}</span>
                {lastRun.status !== 'running' ? (
                  <span className="text-slate-600">
                    <Num>{formatBytes(lastRun.bytesUploaded)}</Num> uploaded
                  </span>
                ) : null}
              </span>
            ) : (
              <span className="text-slate-600">Never run</span>
            )}
          </div>

          {running && lastRun ? (
            <div className="mt-3 max-w-xl">
              <div className="mb-1.5 flex items-center justify-between text-xs">
                <span className="truncate text-slate-400">{lastRun.currentStep || 'Working…'}</span>
                <Num className="text-slate-500">{clampPct(lastRun.progressPct).toFixed(0)}%</Num>
              </div>
              <ProgressBar value={lastRun.progressPct} active />
            </div>
          ) : null}

          {lastRun?.status === 'failed' && lastRun.error ? (
            <p className="mt-2 max-w-2xl rounded-md border border-red-500/25 bg-red-500/5 px-2.5 py-1.5 text-xs text-red-300/90">
              {lastRun.error}
            </p>
          ) : null}
        </div>

        <div className="flex shrink-0 items-center gap-2">
          <Toggle
            checked={job.enabled}
            onChange={(checked) => void onToggle(checked)}
            disabled={busy !== null}
            label={`${job.enabled ? 'Disable' : 'Enable'} ${job.name}`}
          />
          <Button
            size="sm"
            variant="primary"
            icon={<Play className="size-3.5" aria-hidden />}
            loading={busy === 'run'}
            disabled={running || busy !== null}
            onClick={() => void onRun()}
          >
            {running ? 'Running' : 'Run Now'}
          </Button>
          <IconButton aria-label={`Edit ${job.name}`} title="Edit job" onClick={onEdit}>
            <Pencil className="size-4" aria-hidden />
          </IconButton>
          <IconButton
            variant="dangerQuiet"
            aria-label={`Delete ${job.name}`}
            title="Delete job"
            loading={busy === 'delete'}
            onClick={() => void onDelete()}
          >
            <Trash2 className="size-4" aria-hidden />
          </IconButton>
        </div>
      </div>
    </div>
  )
}

export function JobsPage() {
  const navigate = useNavigate()
  const loader = useCallback(async (): Promise<JobsData> => {
    const [jobs, targets, vms, agents] = await Promise.all([
      listJobs(),
      listTargets(),
      listVMs(),
      listAgents(),
    ])
    return { jobs, targets, vms, agents }
  }, [])
  const { data, loading, error, reload, refresh } = useAsync(loader)
  const [wizardOpen, setWizardOpen] = useState(false)
  const [editing, setEditing] = useState<Job | null>(null)

  const jobs = data?.jobs ?? []
  const anyRunning = jobs.some((job) => job.lastRun?.status === 'running')
  usePolling(() => void refresh(), 2000, anyRunning)

  const openCreate = () => {
    setEditing(null)
    setWizardOpen(true)
  }

  const openEdit = (job: Job) => {
    setEditing(job)
    setWizardOpen(true)
  }

  return (
    <>
      <PageHeader
        title="Backup Jobs"
        description="Schedules that snapshot virtual machines or collect agent files, then write them to a storage target."
        actions={
          <>
            <Button
              icon={<Activity className="size-4" aria-hidden />}
              onClick={() => navigate('/monitor')}
            >
              Monitor
            </Button>
            <Button
              icon={<RefreshCw className="size-4" aria-hidden />}
              onClick={() => void reload()}
              loading={loading}
            >
              Refresh
            </Button>
            <Button
              variant="primary"
              icon={<Plus className="size-4" aria-hidden />}
              onClick={openCreate}
            >
              Create Job
            </Button>
          </>
        }
      />

      {loading && !data ? (
        <Card>
          <SkeletonRows count={4} />
        </Card>
      ) : error && !data ? (
        <ErrorBlock message={error} onRetry={() => void reload()} />
      ) : jobs.length === 0 ? (
        <EmptyState
          icon={<CalendarClock className="size-5" aria-hidden />}
          title="No backup jobs yet"
          description="A job ties sources — virtual machines or an agent’s paths — to a storage target, a schedule, and a retention count. Create one to start protecting data."
          action={
            <Button
              variant="primary"
              icon={<Plus className="size-4" aria-hidden />}
              onClick={openCreate}
            >
              Create Job
            </Button>
          }
        />
      ) : (
        <Card className="divide-y divide-slate-800">
          {jobs.map((job) => (
            <JobRow
              key={String(job.id)}
              job={job}
              agents={data?.agents ?? []}
              onChanged={() => void refresh()}
              onEdit={() => openEdit(job)}
            />
          ))}
        </Card>
      )}

      <JobWizard
        open={wizardOpen}
        onClose={() => setWizardOpen(false)}
        onSaved={() => void refresh()}
        targets={data?.targets ?? []}
        vms={data?.vms ?? []}
        agents={data?.agents ?? []}
        editJob={editing}
      />
    </>
  )
}
