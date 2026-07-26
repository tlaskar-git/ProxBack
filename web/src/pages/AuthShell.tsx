import type { ReactNode } from 'react'
import { ShieldCheck } from 'lucide-react'

/** Centred card used by the Setup and Login screens. */
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
    <div className="relative flex min-h-full items-center justify-center overflow-hidden bg-slate-950 px-4 py-12">
      <div
        className="pointer-events-none absolute -top-40 left-1/2 size-[36rem] -translate-x-1/2 rounded-full bg-accent-500/10 blur-3xl"
        aria-hidden
      />
      <div className="relative w-full max-w-md">
        <div className="mb-7 flex flex-col items-center text-center">
          <div className="mb-3.5 flex size-11 items-center justify-center rounded-xl bg-accent-500/15 text-accent-400 ring-1 ring-accent-500/30">
            <ShieldCheck className="size-5" aria-hidden />
          </div>
          <p className="text-lg font-semibold tracking-tight text-white">ProxBack</p>
          <p className="text-xs text-slate-500">Backup &amp; replication for Proxmox VE</p>
        </div>

        <div className="rounded-xl border border-slate-800 bg-slate-900/70 p-6 shadow-2xl shadow-black/40 backdrop-blur-sm">
          <h1 className="text-base font-semibold text-white">{title}</h1>
          <p className="mt-1 text-sm text-slate-400">{subtitle}</p>
          <div className="mt-6">{children}</div>
        </div>

        {footer ? <div className="mt-5 text-center text-xs text-slate-600">{footer}</div> : null}
      </div>
    </div>
  )
}
