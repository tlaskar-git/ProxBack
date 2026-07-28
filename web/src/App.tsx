import { useCallback, useEffect, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { Route, Routes, useNavigate } from 'react-router-dom'
import { Lock } from 'lucide-react'
import {
  capabilitiesFor,
  errorMessage,
  getMe,
  getSettings,
  getSetupStatus,
  isApiError,
  logout as apiLogout,
  roleOf,
  ROLE_LABEL,
} from './api'
import type { Role, User } from './api'
import { Layout } from './components/Layout'
import { BrandMark } from './components/Brand'
import { Button, EmptyState, PageHeader, Spinner } from './components/ui'
import { useToast } from './components/Toast'
import { SessionProvider, useSession } from './session'
import type { Session } from './session'
import { LoginPage } from './pages/LoginPage'
import { SetupPage } from './pages/SetupPage'
import { DashboardPage } from './pages/DashboardPage'
import { HostsPage } from './pages/HostsPage'
import { VirtualMachinesPage } from './pages/VirtualMachinesPage'
import { JobsPage } from './pages/JobsPage'
import { MonitorPage } from './pages/MonitorPage'
import { RestorePointsPage } from './pages/RestorePointsPage'
import { TargetsPage } from './pages/TargetsPage'
import { AgentsPage } from './pages/AgentsPage'
import { SettingsPage } from './pages/SettingsPage'
import { UsersPage } from './pages/UsersPage'
import { AuditPage } from './pages/AuditPage'
import { NotFoundPage } from './pages/NotFoundPage'

type Phase =
  | { state: 'loading' }
  | { state: 'setup' }
  | { state: 'login'; defaultLogin: boolean }
  | {
      state: 'ready'
      user: User
      role: Role
      mustChangePassword: boolean
      serverVersion: string
    }
  | { state: 'unreachable'; message: string }

function BootScreen({ message, onRetry }: { message?: string; onRetry?: () => void }) {
  return (
    <div className="flex min-h-full flex-col items-center justify-center gap-4 bg-slate-950 px-6 text-center">
      <BrandMark className="size-8 text-accent-400" title="ProxBack" />
      {message ? (
        <>
          <div>
            <p className="text-base font-semibold text-slate-100">ProxBack is not responding</p>
            <p className="mt-1.5 max-w-sm text-sm text-slate-400">{message}</p>
          </div>
          {onRetry ? (
            <Button variant="primary" onClick={onRetry}>
              Retry
            </Button>
          ) : null}
        </>
      ) : (
        <div className="flex items-center gap-2.5 text-sm text-slate-500">
          <Spinner />
          Connecting to the backup server…
        </div>
      )}
    </div>
  )
}

/**
 * Auth gate: `GET /api/setup/status` decides between first-run setup and the
 * normal flow, then `GET /api/me` decides between the login screen and the app.
 */
export default function App() {
  const [phase, setPhase] = useState<Phase>({ state: 'loading' })
  const [serverName, setServerName] = useState('ProxBack')
  const toast = useToast()
  const navigate = useNavigate()

  const bootstrap = useCallback(async () => {
    setPhase({ state: 'loading' })
    try {
      const status = await getSetupStatus()
      if (status.needsSetup) {
        setPhase({ state: 'setup' })
        return
      }
      try {
        const me = await getMe()
        setPhase({
          state: 'ready',
          user: me.user,
          role: roleOf(me),
          mustChangePassword: !!me.mustChangePassword,
          serverVersion: me.serverVersion ?? '',
        })
      } catch (err) {
        if (isApiError(err) && err.isUnauthorized) {
          setPhase({ state: 'login', defaultLogin: !!status.defaultLogin })
          return
        }
        throw err
      }
    } catch (err) {
      setPhase({ state: 'unreachable', message: errorMessage(err) })
    }
  }, [])

  useEffect(() => {
    void bootstrap()
  }, [bootstrap])

  // The topbar server name comes from the settings endpoint.
  useEffect(() => {
    if (phase.state !== 'ready') return
    let cancelled = false
    getSettings()
      .then((settings) => {
        if (!cancelled && settings.serverName) setServerName(settings.serverName)
      })
      .catch(() => {
        /* fall back to the product name */
      })
    return () => {
      cancelled = true
    }
  }, [phase.state])

  const onAuthenticated = useCallback(
    (user: User) => {
      setPhase({
        state: 'ready',
        user,
        // Use the role the login response carries, and otherwise assume the
        // least until `GET /api/me` answers a moment later: guessing admin
        // would flash controls this user may not have, and a viewer watching
        // them appear and then vanish is worse than watching them arrive.
        role: user.role ?? 'viewer',
        mustChangePassword: false,
        serverVersion: '',
      })
      // The login response carries neither the role nor the default-password
      // flag; refresh both.
      getMe()
        .then((me) =>
          setPhase({
            state: 'ready',
            user: me.user,
            role: roleOf(me),
            mustChangePassword: !!me.mustChangePassword,
            serverVersion: me.serverVersion ?? '',
          }),
        )
        .catch(() => {
          /* the session is already usable */
        })
      navigate('/', { replace: true })
    },
    [navigate],
  )

  const signOut = useCallback(() => {
    void apiLogout()
      .catch(() => {
        /* the local session is dropped either way */
      })
      .finally(() => {
        setPhase({ state: 'login', defaultLogin: false })
        toast.info('Signed out.')
        navigate('/', { replace: true })
      })
  }, [navigate, toast])

  const setMustChangePassword = useCallback((value: boolean) => {
    setPhase((prev) => (prev.state === 'ready' ? { ...prev, mustChangePassword: value } : prev))
  }, [])

  const session = useMemo<Session | null>(() => {
    if (phase.state !== 'ready') return null
    return {
      user: phase.user,
      role: phase.role,
      can: capabilitiesFor(phase.role),
      serverName,
      setServerName,
      signOut,
      mustChangePassword: phase.mustChangePassword,
      setMustChangePassword,
      serverVersion: phase.serverVersion,
    }
  }, [phase, serverName, signOut, setMustChangePassword])

  if (phase.state === 'loading') return <BootScreen />
  if (phase.state === 'unreachable') {
    return <BootScreen message={phase.message} onRetry={() => void bootstrap()} />
  }
  if (phase.state === 'setup') return <SetupPage onAuthenticated={onAuthenticated} />
  if (phase.state === 'login') {
    return <LoginPage onAuthenticated={onAuthenticated} defaultLogin={phase.defaultLogin} />
  }
  if (!session) return <BootScreen />

  return (
    <SessionProvider value={session}>
      <Routes>
        <Route element={<Layout />}>
          <Route index element={<DashboardPage />} />
          <Route path="hosts" element={<HostsPage />} />
          <Route path="vms" element={<VirtualMachinesPage />} />
          <Route path="jobs" element={<JobsPage />} />
          <Route path="monitor" element={<MonitorPage />} />
          <Route path="restore-points" element={<RestorePointsPage />} />
          <Route path="targets" element={<TargetsPage />} />
          <Route path="agents" element={<AgentsPage />} />
          <Route path="settings" element={<SettingsPage />} />
          <Route
            path="users"
            element={
              <AdminOnly title="Users">
                <UsersPage />
              </AdminOnly>
            }
          />
          <Route
            path="audit"
            element={
              <AdminOnly title="Audit">
                <AuditPage />
              </AdminOnly>
            }
          />
          <Route path="*" element={<NotFoundPage />} />
        </Route>
      </Routes>
    </SessionProvider>
  )
}

/**
 * Admin-only pages are not in a non-admin's navigation, but a bookmark or a
 * pasted link still lands here. Say plainly that the page exists and that this
 * account may not read it — the server would answer 403 anyway, and an
 * explanation beats a raw error or a pretended 404.
 */
function AdminOnly({ title, children }: { title: string; children: ReactNode }) {
  const { can, role } = useSession()
  const navigate = useNavigate()
  if (can.manageUsers) return <>{children}</>
  return (
    <>
      <PageHeader title={title} description="Administrators only." />
      <EmptyState
        icon={<Lock className="size-5" aria-hidden />}
        title="This page is for administrators"
        description={`You are signed in as ${ROLE_LABEL[role].toLowerCase()}. ${title} is limited to administrators, who can give you the role if you need it.`}
        action={
          <Button variant="primary" onClick={() => navigate('/')}>
            Back to the dashboard
          </Button>
        }
      />
    </>
  )
}
