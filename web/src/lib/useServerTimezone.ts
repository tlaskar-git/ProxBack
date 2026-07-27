import { useEffect, useState } from 'react'
import { getSettings } from '../api'

/**
 * The server's timezone, read once per session.
 *
 * Every job schedule fires in the server's local zone, not the browser's, so
 * any screen that shows a time a job will run has to say which zone it means.
 * The value never changes while the panel is open, so it is fetched once and
 * shared — the scheduling editor can appear on several pages without each one
 * issuing its own request.
 */
let cached: string | null = null
let inFlight: Promise<string> | null = null

function loadTimezone(): Promise<string> {
  if (cached !== null) return Promise.resolve(cached)
  inFlight ??= getSettings()
    .then((settings) => {
      cached = settings.timezone ?? ''
      return cached
    })
    .catch(() => {
      // A failed read is not worth surfacing: the editor falls back to the
      // neutral "server local" wording.
      inFlight = null
      return ''
    })
  return inFlight
}

export function useServerTimezone(enabled = true): string | undefined {
  const [timezone, setTimezone] = useState<string | undefined>(cached ?? undefined)

  useEffect(() => {
    if (!enabled || cached !== null) return
    let active = true
    void loadTimezone().then((value) => {
      if (active) setTimezone(value)
    })
    return () => {
      active = false
    }
  }, [enabled])

  return timezone || undefined
}
