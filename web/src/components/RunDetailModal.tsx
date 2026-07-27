import { useCallback, useEffect, useRef, useState } from 'react'
import { AlertTriangle, Ban, ScrollText, Trash2 } from 'lucide-react'
import { cancelRun, deleteRun, errorMessage, getRun, getRunLog } from '../api'
import type { ID, JobRun, RunLogLine } from '../api'
import { Modal } from './Modal'
import { useConfirm } from './Confirm'
import { useToast } from './Toast'
import { Button, Metric, Num, ProgressBar, RunStatusPill, Spinner } from './ui'
import {
  formatBytes,
  formatDateTime,
  formatDuration,
  formatRatio,
  formatRelative,
  formatTime,
} from '../lib/format'

/**
 * Everything about one run: live metrics, the full untruncated error, and the
 * persisted activity log. Polls while the run is still going.
 */
export function RunDetailModal({
  runId,
  onClose,
  onChanged,
}: {
  runId: ID | null
  onClose: () => void
  onChanged: () => void
}) {
  const toast = useToast()
  const confirm = useConfirm()
  const [run, setRun] = useState<JobRun | null>(null)
  const [lines, setLines] = useState<RunLogLine[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const logEnd = useRef<HTMLDivElement | null>(null)

  const load = useCallback(
    async (signal?: AbortSignal) => {
      if (runId === null) return
      try {
        const [detail, log] = await Promise.all([getRun(runId, signal), getRunLog(runId, signal)])
        setRun(detail)
        setLines(log.lines ?? [])
        setError(null)
      } catch (err) {
        if (err instanceof DOMException && err.name === 'AbortError') return
        setError(errorMessage(err))
      }
    },
    [runId],
  )

  useEffect(() => {
    if (runId === null) {
      setRun(null)
      setLines([])
      setError(null)
      return
    }
    const controller = new AbortController()
    setLoading(true)
    void load(controller.signal).finally(() => setLoading(false))
    return () => controller.abort()
  }, [runId, load])

  // Keep a running run's metrics and log fresh.
  useEffect(() => {
    if (runId === null || run?.status !== 'running') return
    const timer = window.setInterval(() => void load(), 2000)
    return () => window.clearInterval(timer)
  }, [runId, run?.status, load])

  // Follow the log tail as new lines land.
  useEffect(() => {
    logEnd.current?.scrollIntoView({ block: 'nearest' })
  }, [lines.length])

  const onCancel = async () => {
    if (!run) return
    setBusy(true)
    try {
      await cancelRun(run.id)
      toast.info('Cancellation requested.')
      await load()
      onChanged()
    } catch (err) {
      toast.error('Could not cancel the run', errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  const onDelete = async () => {
    if (!run) return
    const ok = await confirm({
      title: 'Delete this run from history',
      message: (
        <>
          Remove the <span className="font-medium text-slate-100">{run.jobName}</span> run from{' '}
          {formatDateTime(run.startedAt)} and its activity log? Restore points created by this run
          are kept — only the history entry goes away.
        </>
      ),
      confirmLabel: 'Delete run',
    })
    if (!ok) return
    setBusy(true)
    try {
      await deleteRun(run.id)
      toast.success('Run deleted from history.')
      onChanged()
      onClose()
    } catch (err) {
      toast.error('Could not delete the run', errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  const running = run?.status === 'running'

  return (
    <Modal
      open={runId !== null}
      onClose={onClose}
      width="lg"
      title={run ? run.jobName : 'Run detail'}
      subtitle={
        run
          ? `Started ${formatDateTime(run.startedAt)} · ${
              run.finishedAt ? `finished ${formatRelative(run.finishedAt)}` : 'in progress'
            }`
          : undefined
      }
      footer={
        <>
          <Button onClick={onClose}>Close</Button>
          {running ? (
            <Button
              variant="danger"
              loading={busy}
              icon={<Ban className="size-4" aria-hidden />}
              onClick={() => void onCancel()}
            >
              Cancel run
            </Button>
          ) : run ? (
            <Button
              variant="danger"
              loading={busy}
              icon={<Trash2 className="size-4" aria-hidden />}
              onClick={() => void onDelete()}
            >
              Delete from history
            </Button>
          ) : null}
        </>
      }
    >
      {loading && !run ? (
        <div className="flex items-center gap-2.5 py-6 text-sm text-slate-500">
          <Spinner />
          Loading run detail…
        </div>
      ) : error && !run ? (
        <p className="rounded-lg border border-red-500/30 bg-red-500/10 px-3.5 py-2.5 text-xs text-red-300">
          {error}
        </p>
      ) : run ? (
        <div className="space-y-5">
          <div className="flex flex-wrap items-center gap-3">
            <RunStatusPill status={run.status} />
            <span className="text-xs text-slate-500">{run.currentStep}</span>
          </div>

          {running ? <ProgressBar value={run.progressPct} active /> : null}

          <dl className="grid grid-cols-2 gap-4 rounded-lg border border-slate-800 bg-slate-950/40 px-4 py-3 sm:grid-cols-4">
            <Metric label="Processed" value={<Num>{formatBytes(run.bytesProcessed)}</Num>} />
            <Metric label="Uploaded" value={<Num>{formatBytes(run.bytesUploaded)}</Num>} />
            <Metric label="Dedup" value={<Num>{formatRatio(run.dedupRatio)}</Num>} />
            <Metric
              label="Duration"
              value={<Num>{formatDuration(run.startedAt, run.finishedAt)}</Num>}
            />
          </dl>

          {run.error ? (
            <div className="space-y-1.5 rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-3">
              <p className="flex items-center gap-2 text-xs font-medium text-red-200">
                <AlertTriangle className="size-3.5" aria-hidden />
                Failure
              </p>
              <p className="font-mono text-xs leading-relaxed break-all whitespace-pre-wrap text-red-300">
                {run.error}
              </p>
            </div>
          ) : null}

          <div>
            <p className="mb-2 flex items-center gap-2 text-[11px] tracking-wide text-slate-500 uppercase">
              <ScrollText className="size-3.5" aria-hidden />
              Activity log
            </p>
            {lines.length === 0 ? (
              <p className="rounded-lg border border-slate-800 bg-slate-950/40 px-4 py-3 text-xs text-slate-500">
                No log entries for this run. Runs from earlier versions have no stored log.
              </p>
            ) : (
              <div className="max-h-72 space-y-1 overflow-y-auto rounded-lg border border-slate-800 bg-slate-950/60 px-4 py-3">
                {lines.map((entry, i) => (
                  <p key={i} className="flex gap-3 font-mono text-xs leading-relaxed">
                    <span className="shrink-0 tabular-nums text-slate-600">
                      {formatTime(entry.ts)}
                    </span>
                    <span className="break-all text-slate-400">{entry.line}</span>
                  </p>
                ))}
                <div ref={logEnd} />
              </div>
            )}
          </div>
        </div>
      ) : null}
    </Modal>
  )
}
