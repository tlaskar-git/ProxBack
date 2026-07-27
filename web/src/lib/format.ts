/** Presentation helpers: byte sizes, dates, durations, percentages. */

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

const CRON_LABELS: Record<string, string> = {
  '0 * * * *': 'Hourly, on the hour',
  '0 2 * * *': 'Daily at 02:00',
  '0 3 * * 0': 'Weekly, Sunday at 03:00',
}

/** Friendly label for a schedule value (`"manual"` or a 5-field cron). */
export function describeSchedule(schedule: string | null | undefined): string {
  if (!schedule || schedule === 'manual') return 'Manual only'
  return CRON_LABELS[schedule.trim()] ?? `Cron · ${schedule}`
}

/**
 * Next-run label for a job row. The server sends `nextRun: null` both for
 * manual schedules and for disabled jobs, so say which one it is.
 */
export function describeNextRun(
  nextRun: string | null | undefined,
  job: { enabled: boolean; schedule: string },
): string {
  if (nextRun) return `Next run ${formatRelative(nextRun)}`
  if (!job.enabled) return 'Disabled'
  if (!job.schedule || job.schedule === 'manual') return 'Manual'
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
