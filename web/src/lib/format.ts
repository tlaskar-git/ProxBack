/** Presentation helpers: byte sizes, dates, durations, percentages, schedules. */

import { parseSchedule } from '../api'
import type { Schedule, ScheduleValue } from '../api'

const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'] as const

/** Human byte size using binary units, e.g. `4.19 GiB`. */
export function formatBytes(bytes: number | null | undefined, digits = 1): string {
  if (bytes === null || bytes === undefined || !Number.isFinite(bytes)) return '—'
  if (bytes <= 0) return '0 B'
  const exponent = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), BYTE_UNITS.length - 1)
  const value = bytes / 1024 ** exponent
  const unit = BYTE_UNITS[exponent] ?? 'B'
  const precision = exponent === 0 ? 0 : value >= 100 ? 0 : digits
  return `${value.toFixed(precision)} ${unit}`
}

/** Compact integer, e.g. `1,204`. */
export function formatCount(value: number | null | undefined): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return '—'
  return value.toLocaleString('en-US')
}

/** Clamp a server-provided percentage into 0–100. */
export function clampPct(pct: number | null | undefined): number {
  if (pct === null || pct === undefined || !Number.isFinite(pct)) return 0
  return Math.max(0, Math.min(100, pct))
}

export function formatPct(pct: number | null | undefined, digits = 0): string {
  return `${clampPct(pct).toFixed(digits)}%`
}

/**
 * Dedup ratio as a multiplier, e.g. `3.4×`. The engine reports
 * processed ÷ uploaded, so 1 means nothing was deduplicated.
 */
export function formatRatio(ratio: number | null | undefined): string {
  if (ratio === null || ratio === undefined || !Number.isFinite(ratio) || ratio <= 0) return '—'
  return `${ratio.toFixed(ratio >= 10 ? 0 : 1)}×`
}

function toDate(value: string | null | undefined): Date | null {
  if (!value) return null
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? null : date
}

/** Absolute local timestamp, e.g. `26 Jul 2026, 14:05`. */
export function formatDateTime(value: string | null | undefined): string {
  const date = toDate(value)
  if (!date) return '—'
  return date.toLocaleString('en-GB', {
    day: '2-digit',
    month: 'short',
    year: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  })
}

/** Local wall-clock time only, e.g. `14:05:07` — for log line stamps. */
export function formatTime(value: string | null | undefined): string {
  const date = toDate(value)
  if (!date) return '—'
  return date.toLocaleTimeString('en-GB', {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  })
}

/** Absolute local date only, e.g. `26 Jul 2026`. */
export function formatDate(value: string | null | undefined): string {
  const date = toDate(value)
  if (!date) return '—'
  return date.toLocaleDateString('en-GB', { day: '2-digit', month: 'short', year: 'numeric' })
}

/** Relative time, e.g. `4 min ago` / `in 22 h`. */
export function formatRelative(value: string | null | undefined): string {
  const date = toDate(value)
  if (!date) return '—'
  const deltaMs = date.getTime() - Date.now()
  const abs = Math.abs(deltaMs)
  const future = deltaMs > 0

  const units: [limit: number, divisor: number, label: string][] = [
    [60_000, 1000, 's'],
    [3_600_000, 60_000, 'min'],
    [86_400_000, 3_600_000, 'h'],
    [2_592_000_000, 86_400_000, 'd'],
  ]

  if (abs < 10_000) return 'just now'
  for (const [limit, divisor, label] of units) {
    if (abs < limit) {
      const amount = Math.round(abs / divisor)
      return future ? `in ${amount} ${label}` : `${amount} ${label} ago`
    }
  }
  return formatDate(value)
}

/** Duration between two timestamps (or from `from` until now), e.g. `2 m 14 s`. */
export function formatDuration(
  from: string | null | undefined,
  to?: string | null | undefined,
): string {
  const start = toDate(from)
  if (!start) return '—'
  const end = toDate(to) ?? new Date()
  return formatSeconds(Math.max(0, Math.round((end.getTime() - start.getTime()) / 1000)))
}

/** Seconds as a coarse duration, e.g. `3 d 4 h`, `12 m 08 s`. */
export function formatSeconds(totalSeconds: number | null | undefined): string {
  if (totalSeconds === null || totalSeconds === undefined || !Number.isFinite(totalSeconds)) {
    return '—'
  }
  const seconds = Math.max(0, Math.floor(totalSeconds))
  if (seconds < 60) return `${seconds} s`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes} m ${String(seconds % 60).padStart(2, '0')} s`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours} h ${String(minutes % 60).padStart(2, '0')} m`
  const days = Math.floor(hours / 24)
  return `${days} d ${hours % 24} h`
}

/** VM uptime seconds, or an em dash when the guest is not running. */
export function formatUptime(seconds: number | null | undefined): string {
  if (!seconds || seconds <= 0) return '—'
  return formatSeconds(seconds)
}

/* ---------------------------------------------------------------------------
 * Throughput
 * ------------------------------------------------------------------------- */

/** Live transfer rate, e.g. `118 MiB/s`. Zero and unknown read as `—`. */
export function formatThroughput(bytesPerSecond: number | null | undefined): string {
  if (
    bytesPerSecond === null ||
    bytesPerSecond === undefined ||
    !Number.isFinite(bytesPerSecond) ||
    bytesPerSecond <= 0
  ) {
    return '—'
  }
  return `${formatBytes(bytesPerSecond)}/s`
}

/**
 * Seconds of work left. Prefers the live rate against the remaining bytes and
 * falls back to extrapolating elapsed time over the reported percentage, so an
 * estimate still appears before the first throughput sample lands.
 */
export function estimateRemainingSeconds(input: {
  startedAt: string | null | undefined
  progressPct: number
  bytesProcessed: number
  totalBytes: number
  throughputBps: number
}): number | null {
  const { totalBytes, bytesProcessed, throughputBps } = input
  if (throughputBps > 0 && totalBytes > bytesProcessed) {
    return Math.round((totalBytes - bytesProcessed) / throughputBps)
  }
  const pct = clampPct(input.progressPct)
  if (pct <= 0.5 || pct >= 100) return null
  const start = toDate(input.startedAt)
  if (!start) return null
  const elapsed = (Date.now() - start.getTime()) / 1000
  if (elapsed <= 0) return null
  return Math.round((elapsed / pct) * (100 - pct))
}

/* ---------------------------------------------------------------------------
 * Schedules
 *
 * Job schedules fire in the *server's* timezone, which need not be the
 * browser's. Every derived label below is therefore computed on a naive
 * calendar anchored to the server's current wall clock, so the weekday and the
 * date it names are the ones the operator will actually see fire.
 * ------------------------------------------------------------------------- */

const WEEKDAY_SHORT = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'] as const
const WEEKDAY_LONG = [
  'Sunday',
  'Monday',
  'Tuesday',
  'Wednesday',
  'Thursday',
  'Friday',
  'Saturday',
] as const
const MONTH_SHORT = [
  'Jan',
  'Feb',
  'Mar',
  'Apr',
  'May',
  'Jun',
  'Jul',
  'Aug',
  'Sep',
  'Oct',
  'Nov',
  'Dec',
] as const

/** Short weekday name for 0–6, Sunday first. */
export function weekdayShort(day: number): string {
  return WEEKDAY_SHORT[((day % 7) + 7) % 7] ?? '—'
}

/** Full weekday name for 0–6, Sunday first. */
export function weekdayLong(day: number): string {
  return WEEKDAY_LONG[((day % 7) + 7) % 7] ?? '—'
}

/** Ordinal day of month, e.g. `1st`, `22nd`. */
export function ordinal(n: number): string {
  const tens = n % 100
  if (tens >= 11 && tens <= 13) return `${n}th`
  switch (n % 10) {
    case 1:
      return `${n}st`
    case 2:
      return `${n}nd`
    case 3:
      return `${n}rd`
    default:
      return `${n}th`
  }
}

/** Join a list the way English does: `Sunday, Monday and Saturday`. */
function joinAnd(parts: string[]): string {
  if (parts.length === 0) return ''
  if (parts.length === 1) return parts[0] as string
  return `${parts.slice(0, -1).join(', ')} and ${parts[parts.length - 1] as string}`
}

/** `HH:MM` split into numbers, tolerant of malformed input. */
function timeParts(time: string): { hour: number; minute: number } {
  const [h, m] = time.split(':')
  const hour = Number(h)
  const minute = Number(m)
  return {
    hour: Number.isFinite(hour) ? Math.max(0, Math.min(23, hour)) : 0,
    minute: Number.isFinite(minute) ? Math.max(0, Math.min(59, minute)) : 0,
  }
}

export function padTime(hour: number, minute: number): string {
  return `${String(hour).padStart(2, '0')}:${String(minute).padStart(2, '0')}`
}

/**
 * The server's current wall clock as a naive `Date`. Calendar arithmetic on it
 * (weekday, "is it still today?", month lengths) yields the server's answer,
 * not the browser's.
 */
function serverNow(timezone?: string): Date {
  const now = new Date()
  if (!timezone) return now
  try {
    const parts = new Intl.DateTimeFormat('en-GB', {
      timeZone: timezone,
      year: 'numeric',
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
      hour12: false,
    }).formatToParts(now)
    const get = (type: string) => Number(parts.find((part) => part.type === type)?.value ?? NaN)
    const year = get('year')
    const month = get('month')
    const day = get('day')
    // Intl renders midnight as hour 24 in some engines; normalise to 0.
    const hour = get('hour') % 24
    if (!Number.isFinite(year) || !Number.isFinite(month) || !Number.isFinite(day)) return now
    return new Date(year, month - 1, day, hour, get('minute'), get('second'))
  } catch {
    // Unknown zone name — the browser clock is the best available answer.
    return now
  }
}

/** Last calendar day of the month `date` falls in. */
function lastDayOfMonth(year: number, month: number): number {
  return new Date(year, month + 1, 0).getDate()
}

/**
 * When this schedule fires next, as a naive date in the server's timezone.
 * `null` for `manual` (never fires) and `advanced` (cron is not parsed in the
 * browser — the server's `nextRun` is authoritative there).
 */
export function nextRunOf(schedule: Schedule, timezone?: string): Date | null {
  const now = serverNow(timezone)

  switch (schedule.kind) {
    case 'manual':
    case 'advanced':
      return null

    case 'hourly': {
      const next = new Date(now)
      next.setSeconds(0, 0)
      next.setMinutes(schedule.minute)
      if (next <= now) next.setHours(next.getHours() + 1)
      return next
    }

    case 'daily': {
      const { hour, minute } = timeParts(schedule.time)
      const next = new Date(now)
      next.setSeconds(0, 0)
      next.setHours(hour, minute)
      if (next <= now) next.setDate(next.getDate() + 1)
      return next
    }

    case 'weekly': {
      if (schedule.weekdays.length === 0) return null
      const { hour, minute } = timeParts(schedule.time)
      const wanted = new Set(schedule.weekdays)
      for (let offset = 0; offset < 8; offset += 1) {
        const candidate = new Date(now)
        candidate.setSeconds(0, 0)
        candidate.setDate(candidate.getDate() + offset)
        candidate.setHours(hour, minute)
        if (wanted.has(candidate.getDay()) && candidate > now) return candidate
      }
      return null
    }

    case 'monthly': {
      const { hour, minute } = timeParts(schedule.time)
      for (let offset = 0; offset < 14; offset += 1) {
        const probe = new Date(now.getFullYear(), now.getMonth() + offset, 1)
        const last = lastDayOfMonth(probe.getFullYear(), probe.getMonth())
        // 31 is the contract's "last day of the month" sentinel.
        const day = Math.min(schedule.dayOfMonth, last)
        const candidate = new Date(probe.getFullYear(), probe.getMonth(), day, hour, minute, 0, 0)
        if (candidate > now) return candidate
      }
      return null
    }
  }
}

/** `Sunday 3 Aug, 02:00` — a naive date rendered without timezone conversion. */
export function formatWallClock(date: Date | null): string {
  if (!date) return '—'
  const weekday = WEEKDAY_LONG[date.getDay()] ?? ''
  const month = MONTH_SHORT[date.getMonth()] ?? ''
  return `${weekday} ${date.getDate()} ${month}, ${padTime(date.getHours(), date.getMinutes())}`
}

/**
 * Short label for a schedule, matching the wording the server sends in
 * `scheduleLabel`. Used as the fallback when a server omits that field, and as
 * the live preview inside the scheduling editor.
 */
export function describeSchedule(value: ScheduleValue | null | undefined): string {
  const schedule = parseSchedule(value)
  switch (schedule.kind) {
    case 'manual':
      return 'Manual only'
    case 'hourly':
      return schedule.minute === 0
        ? 'Hourly, on the hour'
        : `Hourly at :${String(schedule.minute).padStart(2, '0')}`
    case 'daily':
      return `Daily at ${schedule.time}`
    case 'weekly': {
      const days = [...schedule.weekdays].sort((a, b) => a - b)
      if (days.length === 7) return `Every day at ${schedule.time}`
      if (days.length === 5 && days.join() === '1,2,3,4,5') {
        return `Mon to Fri at ${schedule.time}`
      }
      if (days.length === 2 && days.join() === '0,6') return `Weekends at ${schedule.time}`
      return `Weekly on ${days.map(weekdayShort).join(', ')} at ${schedule.time}`
    }
    case 'monthly':
      return schedule.dayOfMonth === 31
        ? `Monthly on the last day at ${schedule.time}`
        : `Monthly on the ${ordinal(schedule.dayOfMonth)} at ${schedule.time}`
    case 'advanced':
      return schedule.cron ? `Advanced · ${schedule.cron}` : 'Advanced'
  }
}

/**
 * Full-sentence confirmation for the scheduling editor, e.g.
 * `Runs every Sunday and Saturday at 03:00`.
 */
export function describeScheduleSentence(schedule: Schedule): string {
  switch (schedule.kind) {
    case 'manual':
      return 'Never runs on its own. You start it from the Backup Jobs page.'
    case 'hourly':
      return schedule.minute === 0
        ? 'Runs every hour, on the hour.'
        : `Runs every hour at ${String(schedule.minute).padStart(2, '0')} minutes past.`
    case 'daily':
      return `Runs every day at ${schedule.time}.`
    case 'weekly': {
      if (schedule.weekdays.length === 0) return 'Pick at least one day.'
      const days = [...schedule.weekdays].sort((a, b) => a - b)
      if (days.length === 7) return `Runs every day at ${schedule.time}.`
      return `Runs every ${joinAnd(days.map(weekdayLong))} at ${schedule.time}.`
    }
    case 'monthly':
      return schedule.dayOfMonth === 31
        ? `Runs on the last day of every month at ${schedule.time}.`
        : `Runs on the ${ordinal(schedule.dayOfMonth)} of every month at ${schedule.time}.`
    case 'advanced':
      return schedule.cron
        ? `Runs on the cron expression ${schedule.cron}.`
        : 'Enter a five-field cron expression.'
  }
}

/**
 * Next-run label for a job row. The server sends `nextRun: null` both for
 * manual schedules and for disabled jobs, so say which one it is.
 */
export function describeNextRun(
  nextRun: string | null | undefined,
  job: { enabled: boolean; schedule: ScheduleValue },
): string {
  if (nextRun) return `Next run ${formatRelative(nextRun)}`
  if (!job.enabled) return 'Disabled'
  if (parseSchedule(job.schedule).kind === 'manual') return 'Manual'
  return 'Not scheduled'
}

/** Loose 5-field cron validation, good enough to catch typos in the UI. */
export function isValidCron(expr: string): boolean {
  const fields = expr.trim().split(/\s+/)
  if (fields.length !== 5) return false
  return fields.every((field) => /^[0-9*,\-/]+$/.test(field))
}

/** Truncate long identifiers for display without breaking layout. */
export function shortId(id: string | number, keep = 8): string {
  const text = String(id)
  return text.length <= keep + 3 ? text : `${text.slice(0, keep)}…`
}
