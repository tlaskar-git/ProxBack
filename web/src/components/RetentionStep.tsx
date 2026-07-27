/**
 * Retention.
 *
 * The default is one number — "keep the last N" — because that is the whole
 * requirement for most estates. Grandfather-father-son lives behind an
 * Advanced disclosure.
 *
 * What makes retention legible is not the inputs, it is the consequence: for a
 * job that already holds restore points, the server is asked what the policy
 * currently in the form would keep and what it would prune, and the answer is
 * shown as a dated list with the rule that saved each survivor. Nobody should
 * have to reason about five interacting counters in their head.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { CalendarDays, Trash2 } from 'lucide-react'
import {
  describeRetention,
  errorMessage,
  getRetentionPreview,
  isSimpleRetention,
  retentionRuleCount,
} from '../api'
import type { ID, RetentionPolicy, RetentionPreview, RetentionPreviewEntry } from '../api'
import { cn } from '../lib/cn'
import { formatDate, formatRelative } from '../lib/format'
import { Button, Disclosure, Field, Hint, Input, Num, Spinner } from './ui'

const PRESETS = [3, 7, 14, 30] as const

interface GfsField {
  key: keyof RetentionPolicy
  label: string
  hint: string
}

const GFS_FIELDS: GfsField[] = [
  { key: 'keepDaily', label: 'Daily', hint: 'newest point of each day' },
  { key: 'keepWeekly', label: 'Weekly', hint: 'newest point of each week' },
  { key: 'keepMonthly', label: 'Monthly', hint: 'newest point of each month' },
  { key: 'keepYearly', label: 'Yearly', hint: 'newest point of each year' },
]

function clamp(value: string): number {
  const n = Number(value)
  if (!Number.isFinite(n)) return 0
  return Math.max(0, Math.min(9999, Math.round(n)))
}

function PreviewRow({ entry, pruned }: { entry: RetentionPreviewEntry; pruned: boolean }) {
  return (
    <li className="flex flex-wrap items-baseline gap-x-3 gap-y-0.5 px-3 py-1.5">
      <Num
        className={cn('shrink-0 text-[13px]', pruned ? 'text-slate-500 line-through' : 'text-slate-200')}
      >
        {formatDate(entry.createdAt)}
      </Num>
      <span className="shrink-0 text-micro text-slate-600">{formatRelative(entry.createdAt)}</span>
      {pruned ? null : (
        <span className="ml-auto flex flex-wrap gap-1">
          {entry.reasons.map((reason) => (
            <span
              key={reason}
              className="rounded border border-slate-700/80 bg-slate-800/60 px-1.5 text-[11px] leading-4 text-slate-400"
            >
              {reason}
            </span>
          ))}
        </span>
      )}
    </li>
  )
}

function Preview({ jobId, retention }: { jobId: ID; retention: RetentionPolicy }) {
  const [preview, setPreview] = useState<RetentionPreview | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const timer = useRef<number | undefined>(undefined)

  const load = useCallback(
    (signal: AbortSignal) => {
      setLoading(true)
      getRetentionPreview(jobId, retention, signal)
        .then((result) => {
          setPreview(result)
          setError(null)
        })
        .catch((err: unknown) => {
          if (err instanceof DOMException && err.name === 'AbortError') return
          setError(errorMessage(err))
        })
        .finally(() => setLoading(false))
    },
    [jobId, retention],
  )

  // Debounced: typing "30" must not fire a request for "3".
  useEffect(() => {
    const controller = new AbortController()
    window.clearTimeout(timer.current)
    timer.current = window.setTimeout(() => load(controller.signal), 350)
    return () => {
      window.clearTimeout(timer.current)
      controller.abort()
    }
  }, [load])

  if (error) {
    return (
      <p className="rounded-lg border border-slate-800 bg-slate-950/40 px-3.5 py-2.5 text-xs text-slate-400">
        The server could not preview this policy: {error}
      </p>
    )
  }

  if (!preview && loading) {
    return (
      <div className="flex items-center gap-2.5 rounded-lg border border-slate-800 bg-slate-950/40 px-3.5 py-4 text-xs text-slate-500">
        <Spinner />
        Working out what this policy keeps…
      </div>
    )
  }

  if (!preview) return null

  if (preview.keeps.length === 0 && preview.prunes.length === 0) {
    return (
      <p className="rounded-lg border border-slate-800 bg-slate-950/40 px-3.5 py-2.5 text-xs text-slate-400">
        This job holds no restore points yet, so there is nothing for the policy to act on. The
        preview fills in after its first successful run.
      </p>
    )
  }

  return (
    <div className={cn('grid gap-3 sm:grid-cols-2', loading && 'opacity-60')}>
      <div className="rounded-lg border border-slate-800 bg-slate-950/40">
        <p className="flex items-center gap-2 border-b border-slate-800/80 px-3 py-2 text-meta font-medium text-ok-300">
          <CalendarDays className="size-3.5" aria-hidden />
          Kept
          <Num className="ml-auto text-slate-400">{preview.keeps.length}</Num>
        </p>
        {preview.keeps.length === 0 ? (
          <p className="px-3 py-3 text-xs text-warn-300">
            Nothing survives this policy. Recovery would be impossible.
          </p>
        ) : (
          <ul className="max-h-56 divide-y divide-slate-800/60 overflow-y-auto">
            {preview.keeps.map((entry) => (
              <PreviewRow key={String(entry.backupId)} entry={entry} pruned={false} />
            ))}
          </ul>
        )}
      </div>

      <div className="rounded-lg border border-slate-800 bg-slate-950/40">
        <p className="flex items-center gap-2 border-b border-slate-800/80 px-3 py-2 text-meta font-medium text-slate-400">
          <Trash2 className="size-3.5" aria-hidden />
          Pruned
          <Num className="ml-auto text-slate-400">{preview.prunes.length}</Num>
        </p>
        {preview.prunes.length === 0 ? (
          <p className="px-3 py-3 text-xs text-slate-500">Nothing would be removed.</p>
        ) : (
          <ul className="max-h-56 divide-y divide-slate-800/60 overflow-y-auto">
            {preview.prunes.map((entry) => (
              <PreviewRow key={String(entry.backupId)} entry={entry} pruned />
            ))}
          </ul>
        )}
      </div>
    </div>
  )
}

export function RetentionStep({
  value,
  onChange,
  /** Present when editing: enables the live keep/prune preview. */
  jobId,
}: {
  value: RetentionPolicy
  onChange: (next: RetentionPolicy) => void
  jobId?: ID
}) {
  const simple = isSimpleRetention(value)
  const total = retentionRuleCount(value)
  const summary = useMemo(() => describeRetention(value), [value])

  const patch = (part: Partial<RetentionPolicy>) => onChange({ ...value, ...part })

  return (
    <div className="space-y-5">
      <div>
        <Field label="Keep last">
          {({ id }) => (
            <div className="flex flex-wrap items-center gap-2">
              <Input
                id={id}
                type="number"
                min={0}
                max={9999}
                value={String(value.keepLast)}
                className="w-24"
                onChange={(event) => patch({ keepLast: clamp(event.target.value) })}
              />
              {PRESETS.map((preset) => (
                <Button
                  key={preset}
                  size="sm"
                  variant={value.keepLast === preset ? 'primary' : 'secondary'}
                  onClick={() => patch({ keepLast: preset })}
                >
                  {preset}
                </Button>
              ))}
            </div>
          )}
        </Field>
        <Hint className="mt-1.5">
          The most recent restore points, whatever day they fall on. This alone is enough for most
          estates.
        </Hint>
      </div>

      <Disclosure
        summary="Advanced — long-term retention"
        hint={simple ? 'off' : 'in use'}
        defaultOpen={!simple}
      >
        <div className="space-y-4">
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
            {GFS_FIELDS.map((field) => (
              <Field key={field.key} label={field.label} hint={field.hint}>
                {({ id }) => (
                  <Input
                    id={id}
                    type="number"
                    min={0}
                    max={9999}
                    value={String(value[field.key])}
                    onChange={(event) => patch({ [field.key]: clamp(event.target.value) })}
                  />
                )}
              </Field>
            ))}
          </div>
          <Hint>
            A restore point survives if any rule keeps it, so these add to “keep last” rather than
            competing with it. Zero switches a rule off.
          </Hint>
        </div>
      </Disclosure>

      <div className="rounded-lg border border-slate-800 bg-slate-950/40 px-3.5 py-2.5">
        <p className="text-[13px] text-slate-200">{summary}</p>
        {total === 0 ? (
          <p className="mt-1 text-xs text-fail-300">
            Every restore point would be pruned after each run and nothing would remain to recover
            from. Keep at least one.
          </p>
        ) : null}
      </div>

      {jobId === undefined ? (
        <Hint>
          Once this job has run, this step shows exactly which restore points the policy keeps and
          which it prunes.
        </Hint>
      ) : (
        <div className="space-y-2">
          <p className="eyebrow">What this policy does to the points held today</p>
          <Preview jobId={jobId} retention={value} />
        </div>
      )}
    </div>
  )
}
