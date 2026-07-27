/**
 * Theme choice.
 *
 * Three settings — Light, System, Dark — persisted in localStorage and applied
 * as `data-theme="light|dark"` on the root element. Every colour in the panel
 * is a CSS custom property re-pointed by that attribute (see index.css), so
 * switching costs one attribute write and no re-render of the tree below.
 *
 * `system` is the default and stays live: if the operating system flips at
 * dusk, the console follows without a reload.
 */

import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { Monitor, MoonStar, Sun } from 'lucide-react'
import { cn } from './lib/cn'

export type ThemeChoice = 'light' | 'dark' | 'system'
export type ResolvedTheme = 'light' | 'dark'

const STORAGE_KEY = 'proxback.theme'

function readStored(): ThemeChoice {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    return raw === 'light' || raw === 'dark' ? raw : 'system'
  } catch {
    return 'system'
  }
}

function systemTheme(): ResolvedTheme {
  if (typeof window === 'undefined' || !window.matchMedia) return 'dark'
  return window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark'
}

interface ThemeState {
  choice: ThemeChoice
  resolved: ResolvedTheme
  setChoice: (choice: ThemeChoice) => void
}

const ThemeContext = createContext<ThemeState | null>(null)

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [choice, setChoiceState] = useState<ThemeChoice>(readStored)
  const [system, setSystem] = useState<ResolvedTheme>(systemTheme)

  // Follow the OS while the choice is "system".
  useEffect(() => {
    if (typeof window === 'undefined' || !window.matchMedia) return
    const query = window.matchMedia('(prefers-color-scheme: light)')
    const onChange = () => setSystem(query.matches ? 'light' : 'dark')
    query.addEventListener('change', onChange)
    return () => query.removeEventListener('change', onChange)
  }, [])

  const resolved: ResolvedTheme = choice === 'system' ? system : choice

  useEffect(() => {
    document.documentElement.setAttribute('data-theme', resolved)
    // Keeps native form controls, scrollbars and the browser chrome in step.
    document.documentElement.style.colorScheme = resolved
  }, [resolved])

  const setChoice = useCallback((next: ThemeChoice) => {
    setChoiceState(next)
    try {
      if (next === 'system') localStorage.removeItem(STORAGE_KEY)
      else localStorage.setItem(STORAGE_KEY, next)
    } catch {
      /* private mode — the choice still holds for this session */
    }
  }, [])

  const value = useMemo<ThemeState>(
    () => ({ choice, resolved, setChoice }),
    [choice, resolved, setChoice],
  )

  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>
}

export function useTheme(): ThemeState {
  const state = useContext(ThemeContext)
  if (!state) throw new Error('useTheme must be used inside <ThemeProvider>.')
  return state
}

const OPTIONS: { value: ThemeChoice; label: string; icon: typeof Sun }[] = [
  { value: 'light', label: 'Light', icon: Sun },
  { value: 'system', label: 'System', icon: Monitor },
  { value: 'dark', label: 'Dark', icon: MoonStar },
]

/**
 * Icon-sized three-way switch. It is a radiogroup rather than a cycling
 * button so the current choice is readable without pressing anything, and so
 * a keyboard user can jump straight to the one they want.
 */
export function ThemeToggle({ className }: { className?: string }) {
  const { choice, setChoice } = useTheme()
  return (
    <div
      role="radiogroup"
      aria-label="Colour theme"
      className={cn(
        'inline-flex items-center rounded-lg border border-slate-800 bg-slate-950/50 p-0.5',
        className,
      )}
    >
      {OPTIONS.map((option) => {
        const active = option.value === choice
        return (
          <button
            key={option.value}
            type="button"
            role="radio"
            aria-checked={active}
            aria-label={`${option.label} theme`}
            title={`${option.label} theme`}
            onClick={() => setChoice(option.value)}
            className={cn(
              'flex size-6 items-center justify-center rounded-md transition-colors duration-150',
              active
                ? 'bg-accent-500/15 text-accent-300'
                : 'text-slate-500 hover:bg-slate-800/70 hover:text-slate-300',
            )}
          >
            <option.icon className="size-3.5" aria-hidden />
          </button>
        )
      })}
    </div>
  )
}
