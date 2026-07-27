/**
 * Advanced protection.
 *
 * An optional step, collapsed by default, holding the controls a six-guest
 * estate never needs: quiescing, scope exclusions, retry and duration limits,
 * a start window, pre/post scripts and a per-job transfer ceiling.
 *
 * Every group starts closed and every default is the safe one, so skipping the
 * whole step produces exactly the behaviour of a job created without it. The
 * summary at the top says, in one line, whether anything here is actually in
 * force — which is also what the Review step and the job card show.
 */

import { useState } from 'react'
import { Plus, X } from 'lucide-react'
import { DEFAULT_POLICY, isDefaultPolicy, policyHighlights } from '../api'
import type { JobKind, JobPolicy, Quiesce } from '../api'
import { cn } from '../lib/cn'
import {
  Button,
  Disclosure,
  Field,
  Hint,
  IconButton,
  Input,
  Segmented,
} from './ui'

const QUIESCE_OPTIONS: { value: Quiesce; label: string }[] = [
  { value: 'none', label: 'Crash-consistent' },
  { value: 'guest-agent', label: 'Guest-agent quiescing' },
]

function clampInt(value: string, min: number, max: number): number {
  const n = Number(value)
  if (!Number.isFinite(n)) return min
  return Math.max(min, Math.min(max, Math.round(n)))
}

/** Editable list of short string entries — excluded disks, excluded paths. */
function TokenList({
  label,
  hint,
  placeholder,
  values,
  onChange,
}: {
  label: string
  hint: string
  placeholder: string
  values: string[]
  onChange: (next: string[]) => void
}) {
  const [draft, setDraft] = useState('')

  const add = () => {
    const value = draft.trim()
    if (!value || values.includes(value)) {
      setDraft('')
      return
    }
    onChange([...values, value])
    setDraft('')
  }

  return (
    <div className="space-y-2">
      <Field label={label} hint={hint}>
        {({ id }) => (
          <div className="flex gap-2">
            <Input
              id={id}
              value={draft}
              placeholder={placeholder}
              onChange={(event) => setDraft(event.target.value)}
              onKeyDown={(event) => {
                if (event.key === 'Enter') {
                  event.preventDefault()
                  add()
                }
              }}
            />
            <Button
              onClick={add}
              disabled={!draft.trim()}
              icon={<Plus className="size-4" aria-hidden />}
            >
              Add
            </Button>
          </div>
        )}
      </Field>

      {values.length > 0 ? (
        <ul className="divide-y divide-slate-800 rounded-lg border border-slate-800">
          {values.map((value) => (
            <li key={value} className="flex items-center gap-3 px-3 py-1.5">
              <code className="min-w-0 flex-1 truncate font-mono text-xs text-slate-300">
                {value}
              </code>
              <IconButton
                aria-label={`Remove ${value}`}
                onClick={() => onChange(values.filter((item) => item !== value))}
              >
                <X className="size-3.5" aria-hidden />
              </IconButton>
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  )
}

export function PolicyStep({
  value,
  kind,
  onChange,
}: {
  value: JobPolicy
  kind: JobKind
  onChange: (next: JobPolicy) => void
}) {
  const patch = (part: Partial<JobPolicy>) => onChange({ ...value, ...part })
  const highlights = policyHighlights(value, kind)
  const isDefault = isDefaultPolicy(value)
  const windowOn = value.window !== null

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2 rounded-lg border border-slate-800 bg-slate-950/40 px-3.5 py-2.5">
        <p className="min-w-0 flex-1 text-[13px] text-slate-300">
          {isDefault ? 'Standard protection — nothing here is in force.' : highlights.join(' · ')}
        </p>
        {isDefault ? null : (
          <Button size="sm" onClick={() => onChange({ ...DEFAULT_POLICY })}>
            Reset to defaults
          </Button>
        )}
      </div>

      <Disclosure summary="Consistency" hint={value.quiesce === 'none' ? 'Crash-consistent' : 'Quiesced'}>
        <div className="space-y-2">
          <Segmented
            label="Consistency"
            value={value.quiesce}
            options={QUIESCE_OPTIONS}
            onChange={(quiesce) => patch({ quiesce })}
          />
          <Hint>
            {value.quiesce === 'none'
              ? 'The disk is read as it stands. A database mid-write recovers the way it would after a power cut.'
              : 'ProxBack asks qemu-guest-agent to flush and freeze the filesystem first. Needs the agent installed and running in the guest.'}
          </Hint>
        </div>
      </Disclosure>

      <Disclosure
        summary="Scope"
        hint={
          kind === 'vm'
            ? value.excludeDisks.length === 0
              ? 'All disks'
              : `${value.excludeDisks.length} excluded`
            : value.excludePaths.length === 0
              ? 'All selected paths'
              : `${value.excludePaths.length} excluded`
        }
      >
        {kind === 'vm' ? (
          <TokenList
            label="Exclude disks"
            hint="Proxmox disk identifiers, e.g. scsi1. Everything else in the guest is protected."
            placeholder="scsi1"
            values={value.excludeDisks}
            onChange={(excludeDisks) => patch({ excludeDisks })}
          />
        ) : (
          <TokenList
            label="Exclude paths"
            hint="Glob patterns matched against each file's path."
            placeholder="**/node_modules"
            values={value.excludePaths}
            onChange={(excludePaths) => patch({ excludePaths })}
          />
        )}
      </Disclosure>

      <Disclosure
        summary="Run control"
        hint={
          value.retryCount === 0 && value.maxDurationMinutes === 0 && !windowOn
            ? 'No limits'
            : 'Limited'
        }
      >
        <div className="space-y-4">
          <div className="grid gap-3 sm:grid-cols-2">
            <Field label="Retries after a failure" hint="0–5. Applies per workload.">
              {({ id }) => (
                <Input
                  id={id}
                  type="number"
                  min={0}
                  max={5}
                  value={String(value.retryCount)}
                  onChange={(event) => patch({ retryCount: clampInt(event.target.value, 0, 5) })}
                />
              )}
            </Field>
            <Field label="Minutes between retries" hint="1–120.">
              {({ id }) => (
                <Input
                  id={id}
                  type="number"
                  min={1}
                  max={120}
                  disabled={value.retryCount === 0}
                  value={String(value.retryDelayMinutes)}
                  onChange={(event) =>
                    patch({ retryDelayMinutes: clampInt(event.target.value, 1, 120) })
                  }
                />
              )}
            </Field>
          </div>

          <Field label="Maximum run duration (minutes)" hint="0 means no limit.">
            {({ id }) => (
              <Input
                id={id}
                type="number"
                min={0}
                max={10080}
                className="w-32"
                value={String(value.maxDurationMinutes)}
                onChange={(event) =>
                  patch({ maxDurationMinutes: clampInt(event.target.value, 0, 10080) })
                }
              />
            )}
          </Field>

          <div className="space-y-2">
            <label className="flex items-center gap-2.5 text-[13px] text-slate-300">
              <input
                type="checkbox"
                className="size-4 accent-accent-500"
                checked={windowOn}
                onChange={(event) =>
                  patch({ window: event.target.checked ? { start: '22:00', end: '06:00' } : null })
                }
              />
              Only start inside a backup window
            </label>
            {windowOn && value.window ? (
              <div className="grid gap-3 sm:grid-cols-2">
                <Field label="Window opens">
                  {({ id }) => (
                    <Input
                      id={id}
                      type="time"
                      value={value.window?.start ?? '22:00'}
                      onChange={(event) =>
                        patch({
                          window: { start: event.target.value, end: value.window?.end ?? '06:00' },
                        })
                      }
                    />
                  )}
                </Field>
                <Field label="Window closes">
                  {({ id }) => (
                    <Input
                      id={id}
                      type="time"
                      value={value.window?.end ?? '06:00'}
                      onChange={(event) =>
                        patch({
                          window: { start: value.window?.start ?? '22:00', end: event.target.value },
                        })
                      }
                    />
                  )}
                </Field>
              </div>
            ) : null}
            <Hint>
              A run already under way is never cut off at the closing time — the window governs when
              a run may start.
            </Hint>
          </div>
        </div>
      </Disclosure>

      <Disclosure
        summary="Automation"
        hint={value.preScript || value.postScript ? 'Scripts set' : 'None'}
      >
        <div className="space-y-4">
          <Field label="Before the backup" hint="Runs on the node helper or agent. Output is captured.">
            {({ id }) => (
              <Input
                id={id}
                value={value.preScript}
                placeholder="/usr/local/bin/quiesce-app.sh"
                onChange={(event) => patch({ preScript: event.target.value })}
              />
            )}
          </Field>
          <Field label="After the backup">
            {({ id }) => (
              <Input
                id={id}
                value={value.postScript}
                placeholder="/usr/local/bin/resume-app.sh"
                onChange={(event) => patch({ postScript: event.target.value })}
              />
            )}
          </Field>
          <Field label="Script timeout (seconds)">
            {({ id }) => (
              <Input
                id={id}
                type="number"
                min={1}
                max={3600}
                className="w-32"
                value={String(value.scriptTimeoutSeconds)}
                onChange={(event) =>
                  patch({ scriptTimeoutSeconds: clampInt(event.target.value, 1, 3600) })
                }
              />
            )}
          </Field>
        </div>
      </Disclosure>

      <Disclosure
        summary="Transfer"
        hint={
          value.uploadLimitMbpsOverride === 0
            ? 'Global limit'
            : `${value.uploadLimitMbpsOverride} Mbps`
        }
      >
        <Field
          label="Transfer ceiling for this job (Mbps)"
          hint="0 inherits the server-wide limit from Settings."
        >
          {({ id }) => (
            <Input
              id={id}
              type="number"
              min={0}
              max={10000}
              className="w-32"
              value={String(value.uploadLimitMbpsOverride)}
              onChange={(event) =>
                patch({ uploadLimitMbpsOverride: clampInt(event.target.value, 0, 10000) })
              }
            />
          )}
        </Field>
      </Disclosure>
    </div>
  )
}

/** Compact read-only rendering of an effective policy, for Review and job cards. */
export function PolicySummary({
  policy,
  kind,
  className,
}: {
  policy: JobPolicy
  kind: JobKind
  className?: string
}) {
  const highlights = policyHighlights(policy, kind)
  if (highlights.length === 0) {
    return <span className={cn('text-slate-400', className)}>Standard protection</span>
  }
  return (
    <span className={cn('inline-flex flex-wrap gap-1', className)}>
      {highlights.map((item) => (
        <span
          key={item}
          className="rounded border border-slate-700/80 bg-slate-800/60 px-1.5 text-[11px] leading-5 text-slate-300"
        >
          {item}
        </span>
      ))}
    </span>
  )
}

