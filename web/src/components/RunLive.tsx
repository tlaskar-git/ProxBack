import { CheckCircle2, CircleDashed, Gauge, MinusCircle, Server, XCircle } from 'lucide-react'
import type { ReactNode } from 'react'
import type { RunDetail, RunSource, RunSourceStatus } from '../api'
import { cn } from '../lib/cn'
import {
  clampPct,
  estimateRemainingSeconds,
  formatBytes,
  formatDuration,
  formatRatio,
  formatSeconds,
  formatThroughput,
} from '../lib/format'
import { ArcProgress, LiveDot, Num, ProgressBar, Sparkline } from './ui'
import type { PillTone } from './ui'

/* ---------------------------------------------------------------------------
 * Throughput samples
 *
 * The server reports an instantaneous rate; the trend is the page's job. Each
 * poll appends one sample, so the sparkline is exactly as long as the operator
 * has been watching — no history endpoint, no charting dependency.
 * ------------------------------------------------------------------------- */

/** Two minutes of history at the 2 s poll cadence. */
export const SAMPLE_CAP = 60

export function appendSample(samples: number[] | undefined, value: number): number[] {
  const next = [...(samples ?? []), Number.isFinite(value) ? Math.max(0, value) : 0]
  return next.length > SAMPLE_CAP ? next.slice(next.length - SAMPLE_CAP) : next
}

/* ---------------------------------------------------------------------------
 * Per-source rows — the "objects in this session" view
 * ------------------------------------------------------------------------- */

const SOURCE_TONE: Record<RunSourceStatus, PillTone> = {
  pending: 'slate',
  running: 'blue',
  success: 'green',
  failed: 'red',
  skipped: 'slate',
}

function SourceGlyph({ status }: { status: RunSourceStatus }) {
  switch (status) {
    case 'success':
      return <CheckCircle2 className="size-4 text-emerald-400" aria-hidden />
    case 'failed':
      return <XCircle className="size-4 text-red-400" aria-hidden />
    case 'running':
      return <LiveDot tone="blue" className="mx-1" />
    case 'skipped':
      return <MinusCircle className="size-4 text-slate-600" aria-hidden />
    default:
      return <CircleDashed className="size-4 text-slate-600" aria-hidden />
  }
}

const SOURCE_LABEL: Record<RunSourceStatus, string> = {
  pending: 'Queued',
  running: 'Running',
  success: 'Done',
  failed: 'Failed',
  skipped: 'Skipped',
}

function SourceRow({ source }: { source: RunSource }) {
  const running = source.status === 'running'
  const pending = source.status === 'pending' || source.status === 'skipped'
  const saved = Math.max(0, source.bytesProcessed - source.bytesUploaded)
  const done = source.status === 'success'

  return (
    <tr
      className={cn(
        'border-l-2 transition-colors duration-150',
        running
          ? 'border-l-accent-400 bg-accent-500/[0.06]'
          : source.status === 'failed'
            ? 'border-l-red-500/60'
            : 'border-l-transparent',
        // Queued work is present but recessive — the eye should land on the
        // object currently moving, not on the six waiting behind it.
        pending && 'opacity-55',
      )}
    >
      <td className="py-2 pr-3 pl-3 align-middle">
        <div className="flex items-center gap-2.5">
          <span className="flex size-4 shrink-0 items-center justify-center">
            <SourceGlyph status={source.status} />
          </span>
          <div className="min-w-0">
            <p
              className={cn(
                'truncate text-[13px] leading-4',
                running ? 'font-medium text-white' : 'text-slate-200',
              )}
            >
              {source.name}
            </p>
            {source.node ? (
              <p className="mt-0.5 flex items-center gap-1 text-micro text-slate-500">
                <Server className="size-2.5" aria-hidden />
                <span className="truncate font-mono">{source.node}</span>
              </p>
            ) : null}
          </div>
        </div>
      </td>

      <td className="w-[34%] px-3 py-2 align-middle">
        <div className="flex items-center gap-2.5">
          <ProgressBar
            value={source.progressPct}
            tone={SOURCE_TONE[source.status]}
            active={running}
            className="h-1"
          />
          <Num
            className={cn(
              'w-9 shrink-0 text-right text-meta',
              running ? 'text-accent-300' : 'text-slate-500',
            )}
          >
            {clampPct(source.progressPct).toFixed(0)}%
          </Num>
        </div>
        {source.error ? (
          <p className="mt-1 truncate text-micro text-red-400/90" title={source.error}>
            {source.error}
          </p>
        ) : null}
      </td>

      <td className="px-3 py-2 text-right align-middle whitespace-nowrap">
        <Num className="text-meta text-slate-400">{formatBytes(source.sizeBytes)}</Num>
      </td>
      <td className="px-3 py-2 text-right align-middle whitespace-nowrap">
        <Num className="text-meta text-slate-300">{formatBytes(source.bytesProcessed)}</Num>
      </td>
      <td className="px-3 py-2 text-right align-middle whitespace-nowrap">
        <Num className={cn('text-meta', done && saved > 0 ? 'text-emerald-400' : 'text-slate-500')}>
          {done || running ? formatBytes(saved) : '—'}
        </Num>
      </td>
      <td className="px-3 py-2 text-right align-middle whitespace-nowrap">
        <span
          className={cn(
            'text-meta',
            source.status === 'failed'
              ? 'text-red-300'
              : source.status === 'success'
                ? 'text-emerald-300'
                : running
                  ? 'text-accent-300'
                  : 'text-slate-500',
          )}
        >
          {SOURCE_LABEL[source.status]}
        </span>
      </td>
    </tr>
  )
}

/**
 * One row per protected object, exactly as the run walks them. This is the
 * view that answers "which VM is it on, and how far in?" — the question a
 * single overall percentage cannot.
 */
export function SourceBreakdown({
  sources,
  className,
}: {
  sources: RunSource[]
  className?: string
}) {
  if (sources.length === 0) {
    return (
      <p className={cn('well px-4 py-6 text-center text-meta text-slate-500', className)}>
        No per-object breakdown for this run. Runs from earlier versions did not record one.
      </p>
    )
  }

  const ordered = [...sources].sort((a, b) => a.seq - b.seq)

  return (
    <div className={cn('well overflow-x-auto', className)}>
      <table className="w-full min-w-[42rem]">
        <thead>
          <tr className="border-b border-slate-800/80 text-left text-micro tracking-wide text-slate-600 uppercase">
            <th scope="col" className="py-2 pr-3 pl-3 font-semibold">
              Object
            </th>
            <th scope="col" className="px-3 py-2 font-semibold">
              Progress
            </th>
            <th scope="col" className="px-3 py-2 text-right font-semibold">
              Size
            </th>
            <th scope="col" className="px-3 py-2 text-right font-semibold">
              Read
            </th>
            <th scope="col" className="px-3 py-2 text-right font-semibold">
              Deduped
            </th>
            <th scope="col" className="px-3 py-2 text-right font-semibold">
              State
            </th>
          </tr>
        </thead>
        <tbody className="divide-y divide-slate-800/60">
          {ordered.map((source) => (
            <SourceRow key={source.seq} source={source} />
          ))}
        </tbody>
      </table>
    </div>
  )
}

/* ---------------------------------------------------------------------------
 * Live run card
 * ------------------------------------------------------------------------- */

function Figure({
  label,
  value,
  tone = 'default',
}: {
  label: string
  value: ReactNode
  tone?: 'default' | 'accent' | 'positive'
}) {
  return (
    <div className="min-w-0">
      <p className="eyebrow">{label}</p>
      <p
        className={cn(
          'figure-lg mt-0.5 truncate text-[15px] leading-5',
          tone === 'accent'
            ? 'text-accent-300'
            : tone === 'positive'
              ? 'text-emerald-400'
              : 'text-slate-100',
        )}
      >
        {value}
      </p>
    </div>
  )
}

/**
 * The in-flight run, rendered as an operations card: one prominent gauge, the
 * live rate and its trend, the numbers that matter, and every object in the
 * session underneath.
 */
export function LiveRunCard({
  detail,
  samples,
  actions,
  onOpen,
  sampleIntervalMs = 2000,
}: {
  detail: RunDetail
  samples: number[]
  actions?: ReactNode
  onOpen?: () => void
  /** Poll cadence the samples were collected at — turns their count into a window. */
  sampleIntervalMs?: number
}) {
  const sources = detail.sources
  const finished = sources.filter(
    (source) => source.status === 'success' || source.status === 'skipped',
  ).length
  const failed = sources.filter((source) => source.status === 'failed').length
  const totalBytes = sources.reduce((sum, source) => sum + Math.max(0, source.sizeBytes), 0)

  const remaining = estimateRemainingSeconds({
    startedAt: detail.startedAt,
    progressPct: detail.progressPct,
    bytesProcessed: detail.bytesProcessed,
    totalBytes,
    throughputBps: detail.throughputBps,
  })

  const active = sources.find((source) => source.status === 'running')
  const peak = samples.length > 0 ? Math.max(...samples) : 0

  return (
    <section className="overflow-hidden rounded-xl border border-slate-700/70 bg-slate-900/80 elev-2">
      {/* A single accent hairline across the top is what separates the live
          card from every other panel on the page. */}
      <div className="h-px w-full bg-gradient-to-r from-accent-500/70 via-accent-500/25 to-transparent" />

      <header className="flex flex-wrap items-center gap-x-4 gap-y-2 border-b border-slate-800/80 px-5 py-3">
        <div className="flex min-w-0 items-center gap-2.5">
          <LiveDot tone="blue" />
          <h2 className="truncate text-[15px] leading-5 font-semibold tracking-tight text-white">
            {detail.jobName}
          </h2>
        </div>
        <span className="text-meta text-slate-500">
          {detail.currentStep || 'Working'}
          {/* The step text usually names the object already; only add it when
              it does not, so the header never reads "Streaming db-01 · db-01". */}
          {active && !detail.currentStep.includes(active.name) ? (
            <>
              {' · '}
              <span className="text-slate-400">{active.name}</span>
            </>
          ) : null}
        </span>
        <div className="ml-auto flex items-center gap-2">{actions}</div>
      </header>

      <div className="flex flex-col gap-6 px-5 py-5 lg:flex-row lg:items-center">
        <ArcProgress
          value={detail.progressPct}
          tone={failed > 0 ? 'amber' : 'blue'}
          ariaLabel={`${detail.jobName} is ${clampPct(detail.progressPct).toFixed(0)} percent complete`}
          label={
            <span className="figure-lg text-[34px] leading-9 text-white">
              {clampPct(detail.progressPct).toFixed(0)}
              <span className="ml-0.5 text-lg text-slate-500">%</span>
            </span>
          }
          caption={
            sources.length > 0 ? (
              <>
                <Num>{finished}</Num> of <Num>{sources.length}</Num> done
              </>
            ) : (
              'in progress'
            )
          }
          className="self-center"
        />

        <div className="min-w-0 flex-1 space-y-4">
          <div className="well px-4 py-3">
            <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
              <div>
                <p className="eyebrow flex items-center gap-1.5">
                  <Gauge className="size-3" aria-hidden />
                  Throughput
                </p>
                <p className="figure-lg mt-0.5 text-2xl leading-7 text-accent-300">
                  {formatThroughput(detail.throughputBps)}
                </p>
              </div>
              <p className="text-meta text-slate-500">
                peak <Num className="text-slate-400">{formatThroughput(peak)}</Num>
                <span className="mx-1.5 text-slate-700">·</span>
                last{' '}
                <Num className="text-slate-400">
                  {Math.round((samples.length * sampleIntervalMs) / 1000)}
                </Num>{' '}
                s
              </p>
            </div>
            <Sparkline
              values={samples}
              ariaLabel={`Throughput trend, most recent ${samples.length} samples`}
              className="mt-2"
            />
          </div>

          <dl className="grid grid-cols-2 gap-x-4 gap-y-3 sm:grid-cols-4">
            <Figure label="Elapsed" value={formatDuration(detail.startedAt, null)} />
            <Figure
              label="Remaining"
              value={remaining === null ? 'estimating' : formatSeconds(remaining)}
              tone="accent"
            />
            <Figure label="Read" value={formatBytes(detail.bytesProcessed)} />
            <Figure
              label="Uploaded"
              value={
                <>
                  {formatBytes(detail.bytesUploaded)}
                  {detail.dedupRatio > 1 ? (
                    <span className="ml-1.5 text-meta font-normal text-emerald-400">
                      {formatRatio(detail.dedupRatio)}
                    </span>
                  ) : null}
                </>
              }
            />
          </dl>
        </div>
      </div>

      <div className="border-t border-slate-800/80 bg-slate-950/30 px-5 py-4">
        <div className="mb-2.5 flex flex-wrap items-center gap-x-3 gap-y-1">
          <h3 className="eyebrow text-slate-400">Objects in this session</h3>
          <span className="hidden h-px min-w-8 flex-1 bg-slate-800/80 sm:block" aria-hidden />
          {sources.length > 0 ? (
            <span className="text-meta text-slate-500">
              <Num className="text-slate-300">{finished}</Num> of{' '}
              <Num className="text-slate-300">{sources.length}</Num> complete
              {failed > 0 ? (
                <span className="ml-2 text-red-400">
                  <Num>{failed}</Num> failed
                </span>
              ) : null}
              {totalBytes > 0 ? (
                <span className="ml-2 text-slate-600">
                  <Num>{formatBytes(totalBytes)}</Num> in scope
                </span>
              ) : null}
            </span>
          ) : null}
          {onOpen ? (
            <button
              type="button"
              onClick={onOpen}
              className="text-meta font-medium text-accent-400 transition-colors duration-150 hover:text-accent-300"
            >
              Activity log
            </button>
          ) : null}
        </div>
        <SourceBreakdown sources={sources} />
      </div>
    </section>
  )
}
