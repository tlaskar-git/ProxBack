import { useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { CalendarDays, CalendarRange, Clock, Hand, Repeat, Terminal } from 'lucide-react'
import type { LucideIcon } from 'lucide-react'
import type { Schedule, ScheduleKind } from '../api'
import { cn } from '../lib/cn'
import {
  describeScheduleSentence,
  formatWallClock,
  isValidCron,
  nextRunOf,
  ordinal,
  padTime,
  weekdayLong,
} from '../lib/format'
import { Input, Num } from './ui'

/* ---------------------------------------------------------------------------
 * Draft state
 *
 * The editor keeps every field alive while the operator switches cadence, so
 * moving Daily → Weekly → Daily does not silently reset the time they just
 * chose. `toSchedule` projects the draft onto the contract's union.
 * ------------------------------------------------------------------------- */

interface Draft {
  kind: ScheduleKind
  minute: number
  time: string
  weekdays: number[]
  dayOfMonth: number
  cron: string
  /** The cadence to fall back to when an advanced expression is cleared. */
  fallback: Exclude<ScheduleKind, 'advanced'>
}

const DEFAULT_DRAFT: Draft = {
  kind: 'daily',
  minute: 0,
  time: '02:00',
  weekdays: [0],
  dayOfMonth: 1,
  cron: '',
  fallback: 'daily',
}

function draftFrom(schedule: Schedule): Draft {
  switch (schedule.kind) {
    case 'manual':
      return { ...DEFAULT_DRAFT, kind: 'manual', fallback: 'manual' }
    case 'hourly':
      return { ...DEFAULT_DRAFT, kind: 'hourly', fallback: 'hourly', minute: schedule.minute }
    case 'daily':
      return { ...DEFAULT_DRAFT, kind: 'daily', fallback: 'daily', time: schedule.time }
    case 'weekly':
      return {
        ...DEFAULT_DRAFT,
        kind: 'weekly',
        fallback: 'weekly',
        time: schedule.time,
        weekdays: schedule.weekdays.length > 0 ? schedule.weekdays : [0],
      }
    case 'monthly':
      return {
        ...DEFAULT_DRAFT,
        kind: 'monthly',
        fallback: 'monthly',
        time: schedule.time,
        dayOfMonth: schedule.dayOfMonth,
      }
    case 'advanced':
      return { ...DEFAULT_DRAFT, kind: 'advanced', cron: schedule.cron }
  }
}

function toSchedule(draft: Draft): Schedule {
  switch (draft.kind) {
    case 'manual':
      return { kind: 'manual' }
    case 'hourly':
      return { kind: 'hourly', minute: draft.minute }
    case 'daily':
      return { kind: 'daily', time: draft.time }
    case 'weekly':
      return { kind: 'weekly', time: draft.time, weekdays: [...draft.weekdays].sort((a, b) => a - b) }
    case 'monthly':
      return { kind: 'monthly', time: draft.time, dayOfMonth: draft.dayOfMonth }
    case 'advanced':
      return { kind: 'advanced', cron: draft.cron.trim() }
  }
}

/** True when the schedule is complete enough to save. */
export function isScheduleComplete(schedule: Schedule): boolean {
  if (schedule.kind === 'weekly') return schedule.weekdays.length > 0
  if (schedule.kind === 'advanced') return isValidCron(schedule.cron)
  return true
}

/* ---------------------------------------------------------------------------
 * Cadence tiles
 * ------------------------------------------------------------------------- */

const CADENCES: { kind: ScheduleKind; label: string; sub: string; icon: LucideIcon }[] = [
  { kind: 'manual', label: 'Manual', sub: 'You start it', icon: Hand },
  { kind: 'hourly', label: 'Hourly', sub: 'Every hour', icon: Repeat },
  { kind: 'daily', label: 'Daily', sub: 'Once a day', icon: Clock },
  { kind: 'weekly', label: 'Weekly', sub: 'Chosen days', icon: CalendarDays },
  { kind: 'monthly', label: 'Monthly', sub: 'One day a month', icon: CalendarRange },
]

function CadenceTile({
  active,
  onClick,
  label,
  sub,
  icon: Icon,
}: {
  active: boolean
  onClick: () => void
  label: string
  sub: string
  icon: LucideIcon
}) {
  return (
    <button
      type="button"
      role="radio"
      aria-checked={active}
      onClick={onClick}
      className={cn(
        'flex flex-col items-center gap-1.5 rounded-lg border px-2 py-3 transition-colors duration-150',
        active
          ? 'border-accent-500/60 bg-accent-500/12 text-accent-100 elev-1'
          : 'border-slate-800 bg-slate-950/40 text-slate-300 hover:border-slate-700 hover:bg-slate-900/70',
      )}
    >
      <Icon
        className={cn('size-4', active ? 'text-accent-300' : 'text-slate-500')}
        aria-hidden
      />
      <span className="text-[13px] leading-4 font-medium">{label}</span>
      <span className={cn('text-micro', active ? 'text-accent-300/70' : 'text-slate-500')}>
        {sub}
      </span>
    </button>
  )
}

/* ---------------------------------------------------------------------------
 * Time control
 *
 * Two selects welded into one bordered control. Native selects rather than a
 * custom popover because they are keyboard- and screen-reader-correct on every
 * platform for free, and they never trap focus inside the wizard modal.
 * ------------------------------------------------------------------------- */

const HOURS = Array.from({ length: 24 }, (_, hour) => hour)

function minuteOptions(current: number): number[] {
  const steps = Array.from({ length: 12 }, (_, index) => index * 5)
  return steps.includes(current) ? steps : [...steps, current].sort((a, b) => a - b)
}

function partsOf(time: string): { hour: number; minute: number } {
  const [h, m] = time.split(':')
  const hour = Number(h)
  const minute = Number(m)
  return {
    hour: Number.isFinite(hour) ? hour : 0,
    minute: Number.isFinite(minute) ? minute : 0,
  }
}

const UNIT_SELECT =
  'appearance-none rounded-md bg-transparent py-1 pr-1 pl-1.5 text-center font-mono text-[15px] tabular-nums text-slate-100 hover:bg-slate-800/70 focus-visible:bg-slate-800'

function TimeField({
  value,
  onChange,
  label,
}: {
  value: string
  onChange: (next: string) => void
  label: string
}) {
  const { hour, minute } = partsOf(value)
  return (
    <div className="inline-flex items-center gap-1.5 rounded-lg border border-slate-700 bg-slate-950/60 px-2.5 py-1.5">
      <Clock className="size-3.5 shrink-0 text-slate-500" aria-hidden />
      <select
        className={UNIT_SELECT}
        aria-label={`${label} — hour`}
        value={String(hour)}
        onChange={(event) => onChange(padTime(Number(event.target.value), minute))}
      >
        {HOURS.map((option) => (
          <option key={option} value={option}>
            {String(option).padStart(2, '0')}
          </option>
        ))}
      </select>
      <span className="font-mono text-[15px] text-slate-600" aria-hidden>
        :
      </span>
      <select
        className={UNIT_SELECT}
        aria-label={`${label} — minute`}
        value={String(minute)}
        onChange={(event) => onChange(padTime(hour, Number(event.target.value)))}
      >
        {minuteOptions(minute).map((option) => (
          <option key={option} value={option}>
            {String(option).padStart(2, '0')}
          </option>
        ))}
      </select>
    </div>
  )
}

/** Common backup windows, so the usual choice is one click rather than two selects. */
const TIME_PRESETS = ['22:00', '00:00', '02:00', '04:00'] as const

function TimeRow({
  value,
  onChange,
  label,
}: {
  value: string
  onChange: (next: string) => void
  label: string
}) {
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
      <TimeField value={value} onChange={onChange} label={label} />
      <div className="flex flex-wrap items-center gap-1">
        {TIME_PRESETS.map((preset) => (
          <button
            key={preset}
            type="button"
            onClick={() => onChange(preset)}
            aria-pressed={value === preset}
            className={cn(
              'rounded-md border px-2 py-1 font-mono text-meta tabular-nums transition-colors duration-150',
              value === preset
                ? 'border-accent-500/50 bg-accent-500/12 text-accent-200'
                : 'border-slate-800 bg-slate-950/40 text-slate-500 hover:border-slate-700 hover:text-slate-300',
            )}
          >
            {preset}
          </button>
        ))}
      </div>
    </div>
  )
}

/* ---------------------------------------------------------------------------
 * Weekday selector
 * ------------------------------------------------------------------------- */

/** Sunday-first initials, matching the contract's 0 = Sunday. */
const WEEKDAY_INITIALS = ['S', 'M', 'T', 'W', 'T', 'F', 'S'] as const

const WEEKDAY_PRESETS: { label: string; days: number[] }[] = [
  { label: 'Mon–Fri', days: [1, 2, 3, 4, 5] },
  { label: 'Weekend', days: [0, 6] },
  { label: 'Every day', days: [0, 1, 2, 3, 4, 5, 6] },
]

function WeekdayPicker({
  selected,
  onChange,
}: {
  selected: number[]
  onChange: (next: number[]) => void
}) {
  const toggle = (day: number) => {
    onChange(
      selected.includes(day)
        ? selected.filter((item) => item !== day)
        : [...selected, day].sort((a, b) => a - b),
    )
  }

  return (
    <div className="space-y-2.5">
      <div className="flex flex-wrap gap-1.5" role="group" aria-label="Days of the week">
        {WEEKDAY_INITIALS.map((initial, day) => {
          const active = selected.includes(day)
          return (
            <button
              key={day}
              type="button"
              onClick={() => toggle(day)}
              aria-pressed={active}
              aria-label={weekdayLong(day)}
              title={weekdayLong(day)}
              className={cn(
                'size-9 rounded-lg border text-[13px] font-semibold transition-colors duration-150',
                active
                  ? 'border-accent-500/60 bg-accent-500/15 text-accent-200 elev-1'
                  : 'border-slate-800 bg-slate-950/50 text-slate-500 hover:border-slate-700 hover:text-slate-300',
              )}
            >
              {initial}
            </button>
          )
        })}
      </div>
      <div className="flex flex-wrap items-center gap-1">
        {WEEKDAY_PRESETS.map((preset) => {
          const active =
            preset.days.length === selected.length &&
            preset.days.every((day) => selected.includes(day))
          return (
            <button
              key={preset.label}
              type="button"
              onClick={() => onChange(preset.days)}
              aria-pressed={active}
              className={cn(
                'rounded-md border px-2 py-1 text-meta transition-colors duration-150',
                active
                  ? 'border-accent-500/50 bg-accent-500/12 text-accent-200'
                  : 'border-slate-800 bg-slate-950/40 text-slate-500 hover:border-slate-700 hover:text-slate-300',
              )}
            >
              {preset.label}
            </button>
          )
        })}
      </div>
      {selected.length === 0 ? (
        <p className="text-meta text-amber-400">Pick at least one day.</p>
      ) : null}
    </div>
  )
}

/* ---------------------------------------------------------------------------
 * Day-of-month selector
 * ------------------------------------------------------------------------- */

const MONTH_DAYS = Array.from({ length: 30 }, (_, index) => index + 1)

function MonthDayPicker({
  value,
  onChange,
}: {
  value: number
  onChange: (next: number) => void
}) {
  return (
    <div className="space-y-2.5">
      <div
        className="grid w-fit grid-cols-7 gap-1"
        role="group"
        aria-label="Day of the month"
      >
        {MONTH_DAYS.map((day) => {
          const active = value === day
          return (
            <button
              key={day}
              type="button"
              onClick={() => onChange(day)}
              aria-pressed={active}
              aria-label={`Day ${day} of the month`}
              className={cn(
                'size-8 rounded-md border font-mono text-meta tabular-nums transition-colors duration-150',
                active
                  ? 'border-accent-500/60 bg-accent-500/15 text-accent-200 elev-1'
                  : 'border-transparent bg-slate-950/50 text-slate-400 hover:border-slate-700 hover:text-slate-100',
              )}
            >
              {day}
            </button>
          )
        })}
      </div>
      <button
        type="button"
        onClick={() => onChange(31)}
        aria-pressed={value === 31}
        className={cn(
          'rounded-md border px-2.5 py-1 text-meta transition-colors duration-150',
          value === 31
            ? 'border-accent-500/50 bg-accent-500/12 text-accent-200'
            : 'border-slate-800 bg-slate-950/40 text-slate-500 hover:border-slate-700 hover:text-slate-300',
        )}
      >
        Last day of the month
      </button>
      {value !== 31 && value > 28 ? (
        <p className="text-meta text-slate-500">
          Months without a {ordinal(value)} run on their last day instead.
        </p>
      ) : null}
    </div>
  )
}

/* ---------------------------------------------------------------------------
 * The editor
 * ------------------------------------------------------------------------- */

function SettingRow({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="space-y-2">
      <p className="eyebrow">{label}</p>
      {children}
    </div>
  )
}

/**
 * The scheduling control. Cron never appears in the normal path: cadence,
 * time, and days are picked visually, the result is confirmed in one English
 * sentence with the next fire time, and the raw expression lives behind a
 * closed disclosure for the people who genuinely want it.
 */
export function ScheduleEditor({
  value,
  onChange,
  timezone,
}: {
  value: Schedule
  onChange: (next: Schedule) => void
  /** Server timezone from `GET /api/settings`; every schedule fires in it. */
  timezone?: string
}) {
  const [draft, setDraft] = useState<Draft>(() => draftFrom(value))
  const [advancedOpen, setAdvancedOpen] = useState(value.kind === 'advanced')

  // Always called from an event handler, so reading `draft` from the closure is
  // safe — and notifying the parent has to happen outside the state updater,
  // which React may re-run during a render pass.
  const apply = (patch: Partial<Draft>) => {
    const next = { ...draft, ...patch }
    setDraft(next)
    onChange(toSchedule(next))
  }

  const schedule = useMemo(() => toSchedule(draft), [draft])
  const nextRun = useMemo(() => nextRunOf(schedule, timezone), [schedule, timezone])
  const cronInvalid = draft.kind === 'advanced' && draft.cron.trim() !== '' && !isValidCron(draft.cron)

  return (
    <div className="space-y-5">
      <SettingRow label="How often">
        <div
          role="radiogroup"
          aria-label="How often this job runs"
          className="grid grid-cols-2 gap-2 sm:grid-cols-5"
        >
          {CADENCES.map((cadence) => (
            <CadenceTile
              key={cadence.kind}
              active={draft.kind === cadence.kind}
              onClick={() => {
                setAdvancedOpen(false)
                apply({
                  kind: cadence.kind,
                  fallback: cadence.kind as Exclude<ScheduleKind, 'advanced'>,
                })
              }}
              label={cadence.label}
              sub={cadence.sub}
              icon={cadence.icon}
            />
          ))}
        </div>
      </SettingRow>

      {draft.kind === 'hourly' ? (
        <SettingRow label="Minutes past the hour">
          <div className="flex flex-wrap gap-1.5" role="group" aria-label="Minutes past the hour">
            {[0, 5, 10, 15, 20, 30, 40, 45, 50].map((minute) => (
              <button
                key={minute}
                type="button"
                onClick={() => apply({ minute })}
                aria-pressed={draft.minute === minute}
                aria-label={`${minute} minutes past the hour`}
                className={cn(
                  'rounded-lg border px-2.5 py-1.5 font-mono text-[13px] tabular-nums transition-colors duration-150',
                  draft.minute === minute
                    ? 'border-accent-500/60 bg-accent-500/15 text-accent-200 elev-1'
                    : 'border-slate-800 bg-slate-950/50 text-slate-400 hover:border-slate-700 hover:text-slate-100',
                )}
              >
                :{String(minute).padStart(2, '0')}
              </button>
            ))}
          </div>
        </SettingRow>
      ) : null}

      {draft.kind === 'daily' || draft.kind === 'weekly' || draft.kind === 'monthly' ? (
        <SettingRow label="At">
          <TimeRow
            value={draft.time}
            onChange={(time) => apply({ time })}
            label="Time this job runs"
          />
        </SettingRow>
      ) : null}

      {draft.kind === 'weekly' ? (
        <SettingRow label="On these days">
          <WeekdayPicker
            selected={draft.weekdays}
            onChange={(weekdays) => apply({ weekdays })}
          />
        </SettingRow>
      ) : null}

      {draft.kind === 'monthly' ? (
        <SettingRow label="On this day">
          <MonthDayPicker value={draft.dayOfMonth} onChange={(day) => apply({ dayOfMonth: day })} />
        </SettingRow>
      ) : null}

      {/* Plain-English confirmation. This is the line the operator actually
          reads before they click Create, so it gets the weight. */}
      <div
        className={cn(
          'rounded-lg border px-4 py-3',
          draft.kind === 'manual'
            ? 'border-slate-800 bg-slate-950/50'
            : 'border-accent-500/25 bg-accent-500/[0.07]',
        )}
        aria-live="polite"
      >
        <p
          className={cn(
            'text-[13px] leading-5 font-medium',
            draft.kind === 'manual' ? 'text-slate-300' : 'text-accent-100',
          )}
        >
          {describeScheduleSentence(schedule)}
        </p>
        <p className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-meta text-slate-500">
          {nextRun ? (
            <span>
              Next run <Num className="text-slate-300">{formatWallClock(nextRun)}</Num>
            </span>
          ) : draft.kind === 'advanced' ? (
            <span>Next run is calculated by the server.</span>
          ) : null}
          {/* Go reports "Local" when the host exposes no zone name (Windows
              dev hosts); naming it would tell the operator nothing. */}
          {timezone && timezone !== 'Local' ? (
            <span>
              Server time zone <span className="font-mono text-slate-400">{timezone}</span>
            </span>
          ) : (
            <span>Times are in the server’s local time zone.</span>
          )}
        </p>
      </div>

      {/* Advanced. Closed by default, never the default cadence, and never on
          screen until deliberately opened. */}
      <details
        open={advancedOpen}
        onToggle={(event) => setAdvancedOpen(event.currentTarget.open)}
        className="group rounded-lg border border-slate-800 bg-slate-950/40"
      >
        <summary className="flex list-none items-center gap-2 px-3.5 py-2.5 text-meta text-slate-500 transition-colors duration-150 hover:text-slate-300 [&::-webkit-details-marker]:hidden">
          <Terminal className="size-3.5" aria-hidden />
          Advanced — cron expression
          <span className="ml-auto rounded border border-slate-800 px-1.5 py-px text-micro text-slate-600 uppercase">
            Advanced
          </span>
        </summary>
        <div className="space-y-2.5 border-t border-slate-800 px-3.5 py-3">
          <p className="text-meta leading-relaxed text-slate-500">
            For schedules the options above cannot express — every 15 minutes, quarterly, the second
            Tuesday. Five fields: minute, hour, day of month, month, day of week. Filling this in
            replaces the schedule chosen above.
          </p>
          <div className="flex flex-wrap items-center gap-2">
            <Input
              value={draft.cron}
              placeholder="*/15 * * * *"
              aria-label="Cron expression"
              className="w-56 font-mono"
              onChange={(event) => {
                const cron = event.target.value
                // Emptying the field hands control back to the cadence tiles
                // rather than stranding the job on an invalid expression.
                apply({ kind: cron.trim() ? 'advanced' : draft.fallback, cron })
              }}
            />
            {draft.kind === 'advanced' && draft.cron.trim() ? (
              <span
                className={cn(
                  'text-meta',
                  cronInvalid ? 'text-red-400' : 'text-accent-300',
                )}
              >
                {cronInvalid ? 'Enter five space-separated fields.' : 'In use'}
              </span>
            ) : null}
          </div>
        </div>
      </details>
    </div>
  )
}
