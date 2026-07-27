import { createContext, useCallback, useContext, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { ArrowRight, CheckCircle2, Info, X, XCircle } from 'lucide-react'
import { cn } from '../lib/cn'

export type ToastKind = 'success' | 'error' | 'info'

/** Optional one-tap follow-up, e.g. “View in Monitor”. */
export interface ToastAction {
  label: string
  onClick: () => void
}

interface Toast {
  id: number
  kind: ToastKind
  title: string
  detail?: string
  action?: ToastAction
}

interface ToastApi {
  success: (title: string, detail?: string, action?: ToastAction) => void
  error: (title: string, detail?: string, action?: ToastAction) => void
  info: (title: string, detail?: string, action?: ToastAction) => void
}

const ToastContext = createContext<ToastApi | null>(null)

const KIND_STYLES: Record<ToastKind, { ring: string; icon: ReactNode }> = {
  success: {
    ring: 'border-emerald-500/40 bg-emerald-500/10',
    icon: <CheckCircle2 className="size-4 shrink-0 text-emerald-400" aria-hidden />,
  },
  error: {
    ring: 'border-red-500/40 bg-red-500/10',
    icon: <XCircle className="size-4 shrink-0 text-red-400" aria-hidden />,
  },
  info: {
    ring: 'border-accent-500/40 bg-accent-500/10',
    icon: <Info className="size-4 shrink-0 text-accent-400" aria-hidden />,
  },
}

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([])
  const nextId = useRef(1)

  const dismiss = useCallback((id: number) => {
    setToasts((current) => current.filter((toast) => toast.id !== id))
  }, [])

  const push = useCallback(
    (kind: ToastKind, title: string, detail?: string, action?: ToastAction) => {
      const id = nextId.current++
      setToasts((current) => [...current.slice(-4), { id, kind, title, detail, action }])
      window.setTimeout(() => dismiss(id), kind === 'error' || action ? 8000 : 4500)
    },
    [dismiss],
  )

  const api = useMemo<ToastApi>(
    () => ({
      success: (title, detail, action) => push('success', title, detail, action),
      error: (title, detail, action) => push('error', title, detail, action),
      info: (title, detail, action) => push('info', title, detail, action),
    }),
    [push],
  )

  return (
    <ToastContext.Provider value={api}>
      {children}
      <div
        className="pointer-events-none fixed bottom-5 right-5 z-[100] flex w-[min(24rem,calc(100vw-2.5rem))] flex-col gap-2.5"
        role="status"
        aria-live="polite"
      >
        {toasts.map((toast) => (
          <div
            key={toast.id}
            className={cn(
              'pb-toast-in pointer-events-auto flex items-start gap-3 rounded-xl border px-4 py-3 shadow-2xl shadow-black/40 backdrop-blur-md',
              KIND_STYLES[toast.kind].ring,
            )}
          >
            {KIND_STYLES[toast.kind].icon}
            <div className="min-w-0 flex-1">
              <p className="text-sm font-medium text-slate-100">{toast.title}</p>
              {toast.detail ? (
                <p className="mt-0.5 text-xs leading-relaxed break-words text-slate-400">
                  {toast.detail}
                </p>
              ) : null}
              {toast.action ? (
                <button
                  type="button"
                  onClick={() => {
                    toast.action?.onClick()
                    dismiss(toast.id)
                  }}
                  className="mt-2 inline-flex items-center gap-1 rounded-md border border-slate-700 bg-slate-900/60 px-2 py-1 text-[11px] font-medium text-slate-200 transition-colors duration-150 hover:border-slate-600 hover:bg-slate-800 hover:text-white"
                >
                  {toast.action.label}
                  <ArrowRight className="size-3" aria-hidden />
                </button>
              ) : null}
            </div>
            <button
              type="button"
              onClick={() => dismiss(toast.id)}
              className="-mr-1 -mt-0.5 rounded-md p-1 text-slate-500 transition-colors duration-150 hover:bg-white/5 hover:text-slate-300"
              aria-label="Dismiss notification"
            >
              <X className="size-3.5" aria-hidden />
            </button>
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  )
}

export function useToast(): ToastApi {
  const context = useContext(ToastContext)
  if (!context) throw new Error('useToast must be used inside <ToastProvider>.')
  return context
}
