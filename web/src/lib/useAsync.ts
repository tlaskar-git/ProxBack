import { useCallback, useEffect, useRef, useState } from 'react'
import { errorMessage } from '../api'

export interface AsyncResult<T> {
  data: T | null
  loading: boolean
  error: string | null
  /** Re-run the loader and show the loading state. */
  reload: () => Promise<void>
  /** Re-run the loader in the background, keeping the current data on screen. */
  refresh: () => Promise<void>
  /** Patch the local copy after a mutation without a round-trip. */
  setData: (next: T) => void
}

/**
 * Runs `loader` on mount and whenever its identity changes.
 * Pass a `useCallback`-stabilised loader.
 */
export function useAsync<T>(loader: () => Promise<T>): AsyncResult<T> {
  const [data, setData] = useState<T | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const mounted = useRef(true)

  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])

  const run = useCallback(
    async (showLoading: boolean) => {
      if (showLoading) setLoading(true)
      try {
        const next = await loader()
        if (!mounted.current) return
        setData(next)
        setError(null)
      } catch (err) {
        if (!mounted.current) return
        setError(errorMessage(err))
      } finally {
        if (mounted.current) setLoading(false)
      }
    },
    [loader],
  )

  useEffect(() => {
    void run(true)
  }, [run])

  const reload = useCallback(() => run(true), [run])
  const refresh = useCallback(() => run(false), [run])

  return { data, loading, error, reload, refresh, setData }
}

/**
 * Calls `tick` every `intervalMs` while `enabled` is true.
 * The latest callback is always used, so no stale closures.
 */
export function usePolling(tick: () => void, intervalMs: number, enabled: boolean): void {
  const latest = useRef(tick)
  latest.current = tick

  useEffect(() => {
    if (!enabled) return
    const id = window.setInterval(() => latest.current(), intervalMs)
    return () => window.clearInterval(id)
  }, [intervalMs, enabled])
}
