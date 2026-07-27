import { createContext, useCallback, useContext, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { TriangleAlert } from 'lucide-react'
import { Modal } from './Modal'
import { Button } from './ui'

export interface ConfirmOptions {
  title: string
  message: ReactNode
  confirmLabel?: string
  cancelLabel?: string
  destructive?: boolean
}

type ConfirmFn = (options: ConfirmOptions) => Promise<boolean>

const ConfirmContext = createContext<ConfirmFn | null>(null)

/**
 * Provides `useConfirm()` — an async confirmation dialog so every destructive
 * action in the panel gets the same explicit gate.
 */
export function ConfirmProvider({ children }: { children: ReactNode }) {
  const [options, setOptions] = useState<ConfirmOptions | null>(null)
  const resolver = useRef<((value: boolean) => void) | null>(null)

  const confirm = useCallback<ConfirmFn>((next) => {
    setOptions(next)
    return new Promise<boolean>((resolve) => {
      resolver.current = resolve
    })
  }, [])

  const settle = useCallback((value: boolean) => {
    setOptions(null)
    resolver.current?.(value)
    resolver.current = null
  }, [])

  const value = useMemo(() => confirm, [confirm])

  return (
    <ConfirmContext.Provider value={value}>
      {children}
      <Modal
        open={options !== null}
        onClose={() => settle(false)}
        title={options?.title ?? ''}
        width="sm"
        footer={
          <>
            <Button onClick={() => settle(false)}>{options?.cancelLabel ?? 'Cancel'}</Button>
            <Button
              variant={options?.destructive === false ? 'primary' : 'danger'}
              onClick={() => settle(true)}
            >
              {options?.confirmLabel ?? 'Delete'}
            </Button>
          </>
        }
      >
        <div className="flex gap-3.5">
          <div className="flex size-9 shrink-0 items-center justify-center rounded-lg border border-warn-500/30 bg-warn-500/10 text-warn-400">
            <TriangleAlert className="size-4" aria-hidden />
          </div>
          <div className="min-w-0 pt-1 text-sm leading-relaxed text-slate-300">
            {options?.message}
          </div>
        </div>
      </Modal>
    </ConfirmContext.Provider>
  )
}

export function useConfirm(): ConfirmFn {
  const context = useContext(ConfirmContext)
  if (!context) throw new Error('useConfirm must be used inside <ConfirmProvider>.')
  return context
}
