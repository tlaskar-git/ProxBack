import { createContext, useContext } from 'react'
import type { User } from './api'

export interface Session {
  user: User
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
