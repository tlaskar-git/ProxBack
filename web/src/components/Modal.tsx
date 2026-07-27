import { useEffect } from 'react'
import type { ReactNode } from 'react'
import { X } from 'lucide-react'
import { cn } from '../lib/cn'
import { IconButton } from './ui'

export type ModalWidth = 'sm' | 'md' | 'lg' | 'xl'

const WIDTHS: Record<ModalWidth, string> = {
  sm: 'max-w-md',
  md: 'max-w-lg',
  lg: 'max-w-2xl',
  xl: 'max-w-4xl',
}

export function Modal({
  open,
  onClose,
  title,
  subtitle,
  children,
  footer,
  width = 'md',
}: {
  open: boolean
  onClose: () => void
  title: string
  subtitle?: ReactNode
  children: ReactNode
  footer?: ReactNode
  width?: ModalWidth
}) {
  useEffect(() => {
    if (!open) return
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKeyDown)
    const previousOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      document.removeEventListener('keydown', onKeyDown)
      document.body.style.overflow = previousOverflow
    }
  }, [open, onClose])

  if (!open) return null

  return (
    <div className="fixed inset-0 z-50 flex items-start justify-center overflow-y-auto p-4 sm:p-6">
      <button
        type="button"
        aria-label="Close dialog"
        onClick={onClose}
        className="scrim pb-overlay-in fixed inset-0 cursor-default"
      />
      <div
        role="dialog"
        aria-modal="true"
        aria-label={title}
        className={cn(
          'pb-modal-in relative my-auto w-full rounded-xl border border-slate-800 bg-slate-900 elev-3',
          WIDTHS[width],
        )}
      >
        <div className="flex items-start justify-between gap-4 border-b border-slate-800 px-5 py-4">
          <div className="min-w-0">
            <h2 className="text-base font-semibold text-slate-50">{title}</h2>
            {subtitle ? <p className="mt-1 text-xs text-slate-400">{subtitle}</p> : null}
          </div>
          <IconButton onClick={onClose} aria-label="Close dialog">
            <X className="size-4" aria-hidden />
          </IconButton>
        </div>

        <div className="px-5 py-5">{children}</div>

        {footer ? (
          <div className="flex flex-wrap items-center justify-end gap-2 border-t border-slate-800 bg-slate-950/40 px-5 py-4">
            {footer}
          </div>
        ) : null}
      </div>
    </div>
  )
}
