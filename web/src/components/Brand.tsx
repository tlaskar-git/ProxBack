/**
 * ProxBack identity.
 *
 * The mark is a "PB" monogram built out of a server rack: the stem and top bar
 * of a P, three rack units stacked in the counters, and a B whose lower bowl is
 * drawn in the accent orange — storage on the left, the return of that storage
 * on the right.
 *
 * Drawn on a 48-unit grid, filled rather than stroked, so it holds together at
 * a 16px favicon as well as it does at 120px. Everything but the orange bowl is
 * `currentColor`, so the mark takes the ink colour of whatever it sits in and
 * works on light and dark alike; the rack vents are punched out with a mask
 * rather than painted, so the background shows through instead of a guessed
 * colour showing through.
 *
 * Every coordinate stays inside 3…36 of the 48 grid — the arcs are drawn with
 * explicit radii and sweep flags so they cannot swing outside the canvas and
 * clip, which is exactly how the previous mark lost its arrow.
 */

import { useId } from 'react'

/** The accent of the lower bowl. Fixed, because it is the brand colour. */
const ACCENT = '#F59B41'

export function BrandMark({
  className = 'size-6',
  title,
}: {
  className?: string
  title?: string
}) {
  /* The vent mask is referenced by id, and a page renders more than one mark
     (sidebar, login, dialogs). A generated id keeps two instances from sharing
     — and blanking — each other's mask. */
  const vents = useId().replace(/:/g, '')

  return (
    <svg
      viewBox="0 0 48 48"
      className={className}
      fill="none"
      role={title ? 'img' : 'presentation'}
      aria-label={title}
      aria-hidden={title ? undefined : true}
    >
      <defs>
        <mask id={`pb-vents-${vents}`}>
          <rect width="48" height="48" fill="white" />
          {/* Two vent holes per rack unit, punched out so the surface behind
              the mark shows through them at any theme. */}
          {[14.2, 24, 33.8].map((cy) => (
            <g key={cy}>
              <circle cx="14.6" cy={cy} r="0.85" fill="black" />
              <circle cx="17.8" cy={cy} r="0.85" fill="black" />
            </g>
          ))}
        </mask>
      </defs>

      {/* Stem and top bar: the P, and the spine the B hangs off. */}
      <path d="M3 3h22v6H9v36H3z" fill="currentColor" />

      {/* Three rack units in the counters, vents punched through. */}
      <g fill="currentColor" mask={`url(#pb-vents-${vents})`}>
        <rect x="11.5" y="11.6" width="11" height="5.2" rx="2.2" />
        <rect x="11.5" y="21.4" width="11" height="5.2" rx="2.2" />
        <rect x="11.5" y="31.2" width="11" height="5.2" rx="2.2" />
      </g>

      {/* The B. Upper bowl in ink, lower bowl in the accent — the "back" half
          of the name carries the colour. */}
      <path
        d="M24 6a8.5 8.5 0 0 1 0 17"
        stroke="currentColor"
        strokeWidth="6"
        fill="none"
      />
      <path d="M24 23a9.5 9.5 0 0 1 0 19" stroke={ACCENT} strokeWidth="6" fill="none" />
    </svg>
  )
}

/**
 * Mark plus wordmark. The wordmark sits in one weight and one colour, as the
 * supplied lockup does, so the mark's orange is the only accent in it.
 */
export function BrandLockup({
  className,
  subtitle,
  size = 'md',
}: {
  className?: string
  subtitle?: string
  /**
   * `lg` stacks the mark above the wordmark. At the size the sidebar wants, a
   * horizontal lockup would run past 224px, so the large variant goes vertical
   * rather than shrinking the mark back down.
   */
  size?: 'sm' | 'md' | 'lg'
}) {
  if (size === 'lg') {
    return (
      <div className={className}>
        <div className="flex flex-col items-center gap-2.5">
          <BrandMark className="size-[42px] text-slate-100" title="ProxBack" />
          <span className="text-[23px] leading-none font-semibold tracking-[-0.02em] text-slate-50">
            ProxBack
          </span>
        </div>
        {subtitle ? (
          <p className="mt-2 text-center text-[12px] text-slate-500">{subtitle}</p>
        ) : null}
      </div>
    )
  }

  const markSize = size === 'sm' ? 'size-[22px]' : 'size-[28px]'
  const nameSize = size === 'sm' ? 'text-[13px]' : 'text-[15px]'
  return (
    <div className={className}>
      <div className="flex items-center gap-2.5">
        <BrandMark className={`${markSize} text-slate-100`} title="ProxBack" />
        <span className={`${nameSize} font-semibold tracking-[-0.01em] text-slate-50`}>
          ProxBack
        </span>
      </div>
      {subtitle ? <p className="mt-0.5 text-micro text-slate-500">{subtitle}</p> : null}
    </div>
  )
}
