import { createContext, useContext } from 'react'
import type { Capabilities, Role, User } from './api'

export interface Session {
  user: User
  /**
   * v0.6.0 role of the signed-in user, from `GET /api/me`.
   *
   * The console uses it to hide what this user cannot do. **Hiding is a
   * courtesy, not a security boundary** — the server carries a required role on
   * every mutating route and answers a forbidden request with a 403. Never
   * treat a hidden control as protection.
   */
  role: Role
  /** What this role may change, derived once by `capabilitiesFor`. */
  can: Capabilities
  /** Display name of this ProxBack server, from `GET /api/settings`. */
  serverName: string
  setServerName: (name: string) => void
  signOut: () => void
  /** True while the seeded admin/admin credentials are unchanged. */
  mustChangePassword: boolean
  setMustChangePassword: (value: boolean) => void
  /** Version of the server build, e.g. "0.2.0". Empty when unknown. */
  serverVersion: string
}

const SessionContext = createContext<Session | null>(null)

export const SessionProvider = SessionContext.Provider

export function useSession(): Session {
  const session = useContext(SessionContext)
  if (!session) throw new Error('useSession must be used inside the authenticated app shell.')
  return session
}

/** Shorthand for the capability set — the common case at a call site. */
export function useCan(): Capabilities {
  return useSession().can
}

/**
 * Why a control is disabled for this role, phrased for the person reading it
 * and naming who to ask.
 *
 * Preferred over hiding wherever the absence would be confusing: a viewer
 * should understand that the product has a Run button they lack rights to, not
 * conclude that it does not exist.
 */
export function roleDeniedReason(role: Role, what = 'change this'): string {
  if (role === 'viewer') {
    return `Viewers can read everything but cannot ${what}. Ask an administrator.`
  }
  if (role === 'operator') {
    return `Operators cannot ${what}. Ask an administrator.`
  }
  return `Your role does not allow you to ${what}.`
}
