import { createContext, useCallback, useContext, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { CheckCircle2, Info, X, XCircle } from 'lucide-react'
import { cn } from '../lib/cn'

export type ToastKind = 'success' | 'error' | 'info'

interface Toast {
  id: number
  kind: ToastKind
  title: string
  detail?: string
}

interface ToastApi {
  success: (title: string, detail?: string) => void
  error: (title: string, detail?: string) => void
  info: (title: string, detail?: string) => void
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
    (kind: ToastKind, title: string, detail?: string) => {
      const id = nextId.current++
      setToasts((current) => [...current.slice(-4), { id, kind, title, detail }])
      window.setTimeout(() => dismiss(id), kind === 'error' ? 8000 : 4500)
    },
    [dismiss],
  )

  const api = useMemo<ToastApi>(
    () => ({
      success: (title, detail) => push('success', title, detail),
      error: (title, detail) => push('error', title, detail),
      info: (title, detail) => push('info', title, detail),
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
            </div>
            <button
              type="button"
              onClick={() => dismiss(toast.id)}
              className="-mr-1 -mt-0.5 rounded-md p-1 text-slate-500 transition hover:bg-white/5 hover:text-slate-300"
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
