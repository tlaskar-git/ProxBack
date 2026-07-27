/**
 * ProxBack identity.
 *
 * The mark is a stack of three layers cut on the diagonal: the top layer is
 * whole, the ones beneath are the same shape offset and clipped — restore
 * points behind the live one. The notch on the right reads as the "B" of
 * ProxBack at small sizes and as a chevron pointing back in time at large
 * ones. It is drawn from a 24-unit grid so it stays crisp at 16px favicon
 * sizes, and it carries no gradient so it survives monochrome printing,
 * a single-colour favicon, and dark or light backgrounds.
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
      role={title ? 'img' : 'presentation'}
      aria-label={title}
      aria-hidden={title ? undefined : true}
    >
      {/* Oldest layer — furthest back, most recessed. */}
      <path
        d="M4 15.4 12 19.6 20 15.4"
        stroke="currentColor"
        strokeWidth="1.6"
        strokeLinecap="round"
        strokeLinejoin="round"
        opacity="0.32"
      />
      {/* Middle layer. */}
      <path
        d="M4 11.7 12 15.9 20 11.7"
        stroke="currentColor"
        strokeWidth="1.6"
        strokeLinecap="round"
        strokeLinejoin="round"
        opacity="0.62"
      />
      {/* Live layer — the solid volume the eye lands on. */}
      <path
        d="M12 3.2 20.4 7.6a.7.7 0 0 1 0 1.24L12 13.3a.9.9 0 0 1-.84 0L2.76 8.84a.7.7 0 0 1 0-1.24L11.16 3.2a.9.9 0 0 1 .84 0Z"
        fill="currentColor"
      />
    </svg>
  )
}

/**
 * Mark plus wordmark. `tone="brand"` tints the mark with the accent; the
 * wordmark stays neutral so the lockup does not compete with status colour
 * elsewhere on the page.
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
        <span
          className={`${nameSize} font-semibold tracking-[-0.01em] text-slate-50`}
          style={{ fontFeatureSettings: '"ss01"' }}
        >
          Prox<span className="text-slate-400">Back</span>
        </span>
      </div>
      {subtitle ? <p className="mt-0.5 text-micro text-slate-500">{subtitle}</p> : null}
    </div>
  )
}
