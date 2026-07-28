/**
 * ProxBack identity.
 *
 * The mark is a stack of storage platters with a restore arrow curving back
 * into it: "data kept, and brought back". Both halves are literal on purpose —
 * a stack of discs reads as storage to anyone, and the counter-clockwise arrow
 * is the universal restore/rewind gesture, so the mark is guessable without a
 * caption.
 *
 * Drawn on a 24-unit grid with 1.9 stroke weights so the platter separations
 * survive a 16px favicon, and with no gradient so it works monochrome, in a
 * single-colour favicon, and on light or dark backgrounds.
 */

export function BrandMark({
  className = 'size-6',
  title,
}: {
  className?: string
  title?: string
}) {
  return (
    <svg
      viewBox="0 0 24 24"
      className={className}
      fill="none"
      stroke="currentColor"
      strokeWidth="1.9"
      strokeLinecap="round"
      strokeLinejoin="round"
      role={title ? 'img' : 'presentation'}
      aria-label={title}
      aria-hidden={title ? undefined : true}
    >
      {/* Top platter — the solid one, so the stack reads as full. */}
      <ellipse cx="12" cy="6.2" rx="7.4" ry="3.1" fill="currentColor" stroke="none" />
      {/* Stack walls. */}
      <path d="M4.6 6.2v5.1c0 1.71 3.31 3.1 7.4 3.1s7.4-1.39 7.4-3.1V6.2" />
      {/* Lower platter edge — a third layer of depth without extra clutter. */}
      <path d="M4.6 11.3v5.1c0 1.44 2.35 2.66 5.56 3" />
      <path d="M19.4 11.3v3" />
      {/* Restore arrow: sweeps up the right side and points back into the stack. */}
      <path d="M19.4 20.4a5.2 5.2 0 1 0-4.1-8.4" />
      <path d="M15.1 15.4v-3.6h3.6" />
    </svg>
  )
}

/**
 * Mark plus wordmark. The mark carries the brand colour; the wordmark stays
 * neutral so the lockup never competes with a status colour on the same screen.
 */
export function BrandLockup({
  className,
  subtitle,
  size = 'md',
}: {
  className?: string
  subtitle?: string
  size?: 'sm' | 'md'
}) {
  const markSize = size === 'sm' ? 'size-5' : 'size-[26px]'
  const nameSize = size === 'sm' ? 'text-[13px]' : 'text-[15px]'
  return (
    <div className={className}>
      <div className="flex items-center gap-2.5">
        <BrandMark className={`${markSize} text-accent-400`} title="ProxBack" />
        <span className={`${nameSize} font-semibold tracking-[-0.01em] text-slate-50`}>
          Prox<span className="text-slate-400">Back</span>
        </span>
      </div>
      {subtitle ? <p className="mt-0.5 text-micro text-slate-500">{subtitle}</p> : null}
    </div>
  )
}
