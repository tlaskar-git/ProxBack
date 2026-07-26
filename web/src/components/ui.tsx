import type {
  ButtonHTMLAttributes,
  HTMLAttributes,
  InputHTMLAttributes,
  ReactNode,
  SelectHTMLAttributes,
} from 'react'
import { useId } from 'react'
import { Loader2 } from 'lucide-react'
import { cn } from '../lib/cn'

/* ---------------------------------------------------------------------------
 * Button
 * ------------------------------------------------------------------------- */

export type ButtonVariant = 'primary' | 'secondary' | 'ghost' | 'danger' | 'success'
export type ButtonSize = 'sm' | 'md' | 'lg'

const VARIANTS: Record<ButtonVariant, string> = {
  primary:
    'bg-accent-500 text-white shadow-lg shadow-accent-500/20 hover:bg-accent-400 active:bg-accent-600',
  secondary:
    'border border-slate-700 bg-slate-800/70 text-slate-200 hover:border-slate-600 hover:bg-slate-800 hover:text-white',
  ghost: 'text-slate-400 hover:bg-slate-800/70 hover:text-slate-100',
  danger:
    'border border-red-500/40 bg-red-500/10 text-red-300 hover:border-red-500/60 hover:bg-red-500/20 hover:text-red-200',
  success:
    'border border-emerald-500/40 bg-emerald-500/10 text-emerald-300 hover:border-emerald-500/60 hover:bg-emerald-500/20 hover:text-emerald-200',
}

const SIZES: Record<ButtonSize, string> = {
  sm: 'h-8 gap-1.5 px-3 text-xs',
  md: 'h-[38px] gap-2 px-4 text-sm',
  lg: 'h-11 gap-2 px-5 text-sm',
}

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant
  size?: ButtonSize
  loading?: boolean
  icon?: ReactNode
}

export function Button({
  variant = 'secondary',
  size = 'md',
  loading = false,
  icon,
  className,
  children,
  disabled,
  type = 'button',
  ...rest
}: ButtonProps) {
  return (
    <button
      type={type}
      disabled={disabled || loading}
      className={cn(
        'inline-flex shrink-0 items-center justify-center rounded-lg font-medium whitespace-nowrap transition-colors duration-150 disabled:pointer-events-none disabled:opacity-45',
        VARIANTS[variant],
        SIZES[size],
        className,
      )}
      {...rest}
    >
      {loading ? <Loader2 className="size-4 animate-spin" aria-hidden /> : icon}
      {children}
    </button>
  )
}

/** Square icon-only button — always give it an `aria-label`. */
export function IconButton({
  variant = 'ghost',
  className,
  children,
  loading = false,
  disabled,
  ...rest
}: ButtonProps) {
  return (
    <button
      type="button"
      disabled={disabled || loading}
      className={cn(
        'inline-flex size-[34px] shrink-0 items-center justify-center rounded-lg transition-colors duration-150 disabled:pointer-events-none disabled:opacity-45',
        VARIANTS[variant],
        className,
      )}
      {...rest}
    >
      {loading ? <Loader2 className="size-4 animate-spin" aria-hidden /> : children}
    </button>
  )
}

/* ---------------------------------------------------------------------------
 * Surfaces
 * ------------------------------------------------------------------------- */

export function Card({
  className,
  children,
  ...rest
}: { className?: string; children: ReactNode } & HTMLAttributes<HTMLDivElement>) {
  return (
    <div
      className={cn(
        'rounded-xl border border-slate-800 bg-slate-900/60 shadow-sm shadow-black/20',
        className,
      )}
      {...rest}
    >
      {children}
    </div>
  )
}

export function CardHeader({
  title,
  subtitle,
  actions,
  className,
}: {
  title: ReactNode
  subtitle?: ReactNode
  actions?: ReactNode
  className?: string
}) {
  return (
    <div
      className={cn(
        'flex flex-wrap items-start justify-between gap-3 border-b border-slate-800 px-5 py-4',
        className,
      )}
    >
      <div className="min-w-0">
        <h2 className="text-sm font-semibold tracking-wide text-slate-100">{title}</h2>
        {subtitle ? <p className="mt-0.5 text-xs text-slate-500">{subtitle}</p> : null}
      </div>
      {actions ? <div className="flex items-center gap-2">{actions}</div> : null}
    </div>
  )
}

/* ---------------------------------------------------------------------------
 * Page scaffolding
 * ------------------------------------------------------------------------- */

export function PageHeader({
  title,
  description,
  actions,
}: {
  title: string
  description?: string
  actions?: ReactNode
}) {
  return (
    <header className="mb-6 flex flex-wrap items-end justify-between gap-4">
      <div>
        <h1 className="text-2xl font-semibold text-white">{title}</h1>
        {description ? <p className="mt-1 max-w-2xl text-sm text-slate-400">{description}</p> : null}
      </div>
      {actions ? <div className="flex flex-wrap items-center gap-2">{actions}</div> : null}
    </header>
  )
}

/* ---------------------------------------------------------------------------
 * Status pill
 * ------------------------------------------------------------------------- */

export type PillTone = 'green' | 'amber' | 'red' | 'blue' | 'slate'

const TONES: Record<PillTone, string> = {
  green: 'border-emerald-500/30 bg-emerald-500/10 text-emerald-300',
  amber: 'border-amber-500/30 bg-amber-500/10 text-amber-300',
  red: 'border-red-500/30 bg-red-500/10 text-red-300',
  blue: 'border-accent-500/30 bg-accent-500/10 text-accent-300',
  slate: 'border-slate-700 bg-slate-800/60 text-slate-400',
}

const DOTS: Record<PillTone, string> = {
  green: 'bg-emerald-400',
  amber: 'bg-amber-400',
  red: 'bg-red-400',
  blue: 'bg-accent-400',
  slate: 'bg-slate-500',
}

export function StatusPill({
  tone,
  label,
  pulse = false,
  className,
}: {
  tone: PillTone
  label: string
  pulse?: boolean
  className?: string
}) {
  return (
    <span
      className={cn(
        'inline-flex items-center gap-1.5 rounded-full border px-2.5 py-0.5 text-xs font-medium capitalize',
        TONES[tone],
        className,
      )}
    >
      <span
        className={cn('size-1.5 rounded-full', DOTS[tone], pulse && 'animate-pulse')}
        aria-hidden
      />
      {label}
    </span>
  )
}

/** Maps free-form server status strings onto pill tones. */
export function toneForStatus(status: string | null | undefined): PillTone {
  switch ((status ?? '').toLowerCase()) {
    case 'online':
    case 'ok':
    case 'success':
    case 'running':
    case 'connected':
    case 'healthy':
      return 'green'
    case 'stopped':
    case 'offline':
    case 'paused':
    case 'suspended':
    case 'canceled':
    case 'cancelled':
    case 'disabled':
      return 'slate'
    case 'error':
    case 'failed':
    case 'unreachable':
      return 'red'
    case 'warning':
    case 'degraded':
    case 'pending':
      return 'amber'
    default:
      return 'slate'
  }
}

/** Run-status pill with the right tone and a pulse while running. */
export function RunStatusPill({ status }: { status: string }) {
  const tone: PillTone =
    status === 'success'
      ? 'green'
      : status === 'failed'
        ? 'red'
        : status === 'running'
          ? 'blue'
          : 'slate'
  return <StatusPill tone={tone} label={status} pulse={status === 'running'} />
}

/* ---------------------------------------------------------------------------
 * Progress bar
 * ------------------------------------------------------------------------- */

export function ProgressBar({
  value,
  tone = 'blue',
  active = false,
  className,
}: {
  value: number
  tone?: PillTone
  active?: boolean
  className?: string
}) {
  const pct = Math.max(0, Math.min(100, Number.isFinite(value) ? value : 0))
  const fill: Record<PillTone, string> = {
    green: 'bg-emerald-500',
    amber: 'bg-amber-500',
    red: 'bg-red-500',
    blue: 'bg-accent-500',
    slate: 'bg-slate-600',
  }
  return (
    <div
      className={cn('h-1.5 w-full overflow-hidden rounded-full bg-slate-800', className)}
      role="progressbar"
      aria-valuenow={Math.round(pct)}
      aria-valuemin={0}
      aria-valuemax={100}
    >
      <div
        className={cn('h-full rounded-full transition-[width] duration-500', fill[tone], active && 'pb-bar-active')}
        style={{ width: `${pct}%` }}
      />
    </div>
  )
}

/* ---------------------------------------------------------------------------
 * Loading & empty states
 * ------------------------------------------------------------------------- */

export function Spinner({ className }: { className?: string }) {
  return <Loader2 className={cn('size-5 animate-spin text-accent-400', className)} aria-hidden />
}

export function LoadingBlock({ label = 'Loading…' }: { label?: string }) {
  return (
    <div className="flex items-center justify-center gap-3 py-16 text-sm text-slate-500">
      <Spinner />
      {label}
    </div>
  )
}

export function SkeletonCards({ count = 6, height = 'h-32' }: { count?: number; height?: string }) {
  return (
    <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
      {Array.from({ length: count }, (_, index) => (
        <div
          key={index}
          className={cn('pb-skeleton rounded-xl border border-slate-800/70', height)}
        />
      ))}
    </div>
  )
}

export function SkeletonRows({ count = 5 }: { count?: number }) {
  return (
    <div className="divide-y divide-slate-800">
      {Array.from({ length: count }, (_, index) => (
        <div key={index} className="flex items-center gap-4 px-5 py-4">
          <div className="pb-skeleton size-8 rounded-lg" />
          <div className="flex-1 space-y-2">
            <div className="pb-skeleton h-3 w-1/3 rounded" />
            <div className="pb-skeleton h-2.5 w-1/5 rounded" />
          </div>
          <div className="pb-skeleton h-6 w-20 rounded-full" />
        </div>
      ))}
    </div>
  )
}

export function EmptyState({
  icon,
  title,
  description,
  action,
  className,
}: {
  icon: ReactNode
  title: string
  description: string
  action?: ReactNode
  className?: string
}) {
  return (
    <div
      className={cn(
        'flex flex-col items-center justify-center rounded-xl border border-dashed border-slate-800 bg-slate-900/40 px-6 py-16 text-center',
        className,
      )}
    >
      <div className="mb-4 flex size-12 items-center justify-center rounded-xl border border-slate-800 bg-slate-900 text-accent-400">
        {icon}
      </div>
      <h3 className="text-base font-semibold text-slate-100">{title}</h3>
      <p className="mt-1.5 max-w-md text-sm leading-relaxed text-slate-400">{description}</p>
      {action ? <div className="mt-6">{action}</div> : null}
    </div>
  )
}

export function ErrorBlock({ message, onRetry }: { message: string; onRetry?: () => void }) {
  return (
    <div className="flex flex-col items-start gap-3 rounded-xl border border-red-500/30 bg-red-500/5 px-5 py-4">
      <div>
        <p className="text-sm font-medium text-red-300">Could not load this page</p>
        <p className="mt-1 text-sm text-slate-400">{message}</p>
      </div>
      {onRetry ? (
        <Button size="sm" onClick={onRetry}>
          Try again
        </Button>
      ) : null}
    </div>
  )
}

/* ---------------------------------------------------------------------------
 * Form controls
 * ------------------------------------------------------------------------- */

export function Field({
  label,
  hint,
  error,
  children,
  className,
}: {
  label: string
  hint?: ReactNode
  error?: string
  children: (props: { id: string; describedBy: string | undefined }) => ReactNode
  className?: string
}) {
  const id = useId()
  const hintId = hint || error ? `${id}-hint` : undefined
  return (
    <div className={cn('space-y-1.5', className)}>
      <label htmlFor={id} className="block text-xs font-medium tracking-wide text-slate-400">
        {label}
      </label>
      {children({ id, describedBy: hintId })}
      {error ? (
        <p id={hintId} className="text-xs text-red-400">
          {error}
        </p>
      ) : hint ? (
        <p id={hintId} className="text-xs leading-relaxed text-slate-500">
          {hint}
        </p>
      ) : null}
    </div>
  )
}

const CONTROL_CLASS =
  'w-full rounded-lg border border-slate-700 bg-slate-950/60 px-3 py-2 text-sm text-slate-100 transition placeholder:text-slate-600 hover:border-slate-600 focus:border-accent-500 focus:outline-none disabled:opacity-50'

export function Input({ className, ...rest }: InputHTMLAttributes<HTMLInputElement>) {
  return <input className={cn(CONTROL_CLASS, className)} {...rest} />
}

export function Select({
  className,
  children,
  ...rest
}: SelectHTMLAttributes<HTMLSelectElement> & { children: ReactNode }) {
  return (
    <select className={cn(CONTROL_CLASS, 'appearance-none pr-8', className)} {...rest}>
      {children}
    </select>
  )
}

export function Checkbox({
  label,
  hint,
  checked,
  onChange,
  disabled,
}: {
  label: string
  hint?: ReactNode
  checked: boolean
  onChange: (checked: boolean) => void
  disabled?: boolean
}) {
  return (
    <label
      className={cn(
        'flex cursor-pointer items-start gap-3 rounded-lg border border-slate-800 bg-slate-950/40 px-3 py-2.5 transition hover:border-slate-700',
        disabled && 'cursor-not-allowed opacity-50',
      )}
    >
      <input
        type="checkbox"
        checked={checked}
        disabled={disabled}
        onChange={(event) => onChange(event.target.checked)}
        className="mt-0.5 size-4 shrink-0 accent-accent-500"
      />
      <span className="min-w-0">
        <span className="block text-sm text-slate-200">{label}</span>
        {hint ? <span className="mt-0.5 block text-xs text-slate-500">{hint}</span> : null}
      </span>
    </label>
  )
}

/** Small on/off switch used for job enable/disable. */
export function Toggle({
  checked,
  onChange,
  label,
  disabled,
}: {
  checked: boolean
  onChange: (checked: boolean) => void
  label: string
  disabled?: boolean
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className={cn(
        'relative inline-flex h-[22px] w-10 shrink-0 items-center rounded-full border transition-colors disabled:opacity-50',
        checked ? 'border-accent-400/60 bg-accent-500/80' : 'border-slate-700 bg-slate-800',
      )}
    >
      <span
        className={cn(
          'ml-0.5 size-4 rounded-full bg-white shadow transition-transform',
          checked ? 'translate-x-[18px]' : 'translate-x-0',
        )}
      />
    </button>
  )
}

/* ---------------------------------------------------------------------------
 * Misc
 * ------------------------------------------------------------------------- */

/** Key/value line used inside detail cards. */
export function Metric({
  label,
  value,
  className,
}: {
  label: string
  value: ReactNode
  className?: string
}) {
  return (
    <div className={cn('min-w-0', className)}>
      <dt className="text-[11px] tracking-wide text-slate-500 uppercase">{label}</dt>
      <dd className="mt-0.5 truncate text-sm text-slate-200">{value}</dd>
    </div>
  )
}

export function Mono({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <code className={cn('font-mono text-xs break-all text-slate-300', className)}>{children}</code>
  )
}

export function SectionNote({ children }: { children: ReactNode }) {
  return (
    <p className="rounded-lg border border-accent-500/20 bg-accent-500/5 px-3.5 py-2.5 text-xs leading-relaxed text-accent-200/80">
      {children}
    </p>
  )
}
