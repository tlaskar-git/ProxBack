import type { ReactNode } from 'react'
import { BrandMark } from '../components/Brand'
import { ThemeToggle } from '../theme'

/**
 * Centred card used by the Setup and Login screens.
 *
 * No glow behind it. The mark and one rule carry the identity — a sign-in
 * screen dressed as a product landing page was a large part of why this
 * console read as anonymous.
 */
export function AuthShell({
  title,
  subtitle,
  children,
  footer,
}: {
  title: string
  subtitle: string
  children: ReactNode
  footer?: ReactNode
}) {
  return (
    <div className="relative flex min-h-full items-center justify-center bg-slate-950 px-4 py-12">
      <div className="absolute top-4 right-4">
        <ThemeToggle />
      </div>

      <div className="w-full max-w-md">
        <div className="mb-7 flex flex-col items-center text-center">
          <BrandMark className="mb-3 size-8 text-accent-400" title="ProxBack" />
          <p className="text-lg font-semibold tracking-tight text-slate-50">ProxBack</p>
          <p className="text-xs text-slate-500">Backup and recovery for Proxmox VE</p>
        </div>

        <div className="rounded-xl border border-slate-800 bg-slate-900 p-6 elev-2">
          <h1 className="text-base font-semibold text-slate-50">{title}</h1>
          <p className="mt-1 text-sm text-slate-400">{subtitle}</p>
          <div className="mt-6">{children}</div>
        </div>

        {footer ? <div className="mt-5 text-center text-xs text-slate-600">{footer}</div> : null}
      </div>
    </div>
  )
}
