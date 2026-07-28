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
      {/* Storage: a stack of platters, drawn in the upper-left so the restore
          arrow has room. Every coordinate stays inside 2…22 of the 24 grid, so
          nothing clips at any render size. */}
      <ellipse cx="10" cy="5.6" rx="6.2" ry="2.6" fill="currentColor" stroke="none" />
      <path d="M3.8 5.6v8.2c0 1.44 2.78 2.6 6.2 2.6s6.2-1.16 6.2-2.6V5.6" />
      <path d="M3.8 9.7c0 1.44 2.78 2.6 6.2 2.6s6.2-1.16 6.2-2.6" />
      {/* Restore: a circular arrow, closed on itself and fully bounded. */}
      <path d="M20.2 17.4a4 4 0 1 1-1.5-3.12" />
      <path d="M19.1 10.9v3.5h-3.5" />
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
  /**
   * `lg` stacks the mark above the wordmark. At the size the sidebar wants,
   * a horizontal lockup would run past 224px, so the large variant goes
   * vertical rather than shrinking the mark back down.
   */
  size?: 'sm' | 'md' | 'lg'
}) {
  if (size === 'lg') {
    return (
      <div className={className}>
        <div className="flex flex-col items-center gap-2.5">
          <BrandMark className="size-[39px] text-accent-400" title="ProxBack" />
          <span className="text-[23px] leading-none font-semibold tracking-[-0.02em] text-slate-50">
            Prox<span className="text-slate-400">Back</span>
          </span>
        </div>
        {subtitle ? (
          <p className="mt-2 text-center text-[12px] text-slate-500">{subtitle}</p>
        ) : null}
      </div>
    )
  }

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
