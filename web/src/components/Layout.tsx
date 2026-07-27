import { useEffect, useState } from 'react'
import { NavLink, Outlet, useLocation } from 'react-router-dom'
import {
  Activity,
  CalendarClock,
  Database,
  History,
  LayoutDashboard,
  Laptop,
  LogOut,
  Menu,
  Server,
  Settings as SettingsIcon,
  ShieldCheck,
  TriangleAlert,
  X,
} from 'lucide-react'
import type { LucideIcon } from 'lucide-react'
import { cn } from '../lib/cn'
import { useSession } from '../session'
import { Button } from './ui'

interface NavItem {
  to: string
  label: string
  icon: LucideIcon
  end?: boolean
}

const NAV_GROUPS: { heading: string; items: NavItem[] }[] = [
  {
    heading: 'Overview',
    items: [{ to: '/', label: 'Dashboard', icon: LayoutDashboard, end: true }],
  },
  {
    heading: 'Infrastructure',
    items: [
      { to: '/hosts', label: 'Proxmox Hosts', icon: Server },
      { to: '/vms', label: 'Virtual Machines', icon: Laptop },
      { to: '/agents', label: 'Agents', icon: ShieldCheck },
    ],
  },
  {
    heading: 'Protection',
    items: [
      { to: '/jobs', label: 'Backup Jobs', icon: CalendarClock },
      { to: '/monitor', label: 'Monitor', icon: Activity },
      { to: '/restore-points', label: 'Restore Points', icon: History },
      { to: '/targets', label: 'Storage Targets', icon: Database },
    ],
  },
  {
    heading: 'System',
    items: [{ to: '/settings', label: 'Settings', icon: SettingsIcon }],
  },
]

function Brand() {
  return (
    <div className="flex h-14 items-center gap-2.5 border-b border-slate-800/80 px-4">
      <div className="flex size-7 items-center justify-center rounded-md bg-accent-500/15 text-accent-400 ring-1 ring-accent-500/30">
        <ShieldCheck className="size-4" aria-hidden />
      </div>
      <div className="leading-none">
        <p className="text-[13px] font-semibold tracking-tight text-white">ProxBack</p>
        <p className="mt-1 text-micro text-slate-500">Proxmox VE backup</p>
      </div>
    </div>
  )
}

/**
 * Dense nav: 32px rows, a section rule instead of a floating caption, and an
 * active state that is a filled slab with a hard accent edge — visible from
 * the far side of the screen, unlike a tint alone.
 */
function SidebarNav({ onNavigate }: { onNavigate?: () => void }) {
  return (
    <nav className="flex-1 overflow-y-auto px-2.5 py-3">
      {NAV_GROUPS.map((group, index) => (
        <div key={group.heading} className={cn(index > 0 && 'mt-5')}>
          <div className="mb-1.5 flex items-center gap-2 px-2">
            <span className="text-micro font-semibold tracking-[0.14em] text-slate-600 uppercase">
              {group.heading}
            </span>
            <span className="h-px flex-1 bg-slate-800/70" aria-hidden />
          </div>
          <ul>
            {group.items.map((item) => (
              <li key={item.to}>
                <NavLink
                  to={item.to}
                  end={item.end}
                  onClick={onNavigate}
                  className={({ isActive }) =>
                    cn(
                      'group relative flex h-8 items-center gap-2.5 rounded-md px-2.5 text-[13px] transition-colors duration-150',
                      isActive
                        ? 'bg-slate-800/80 font-medium text-white before:absolute before:top-1.5 before:bottom-1.5 before:-left-2.5 before:w-[3px] before:rounded-r-full before:bg-accent-400'
                        : 'text-slate-400 hover:bg-slate-800/40 hover:text-slate-100',
                    )
                  }
                >
                  {({ isActive }) => (
                    <>
                      <item.icon
                        className={cn(
                          'size-4 shrink-0',
                          isActive ? 'text-accent-400' : 'text-slate-500 group-hover:text-slate-300',
                        )}
                        aria-hidden
                      />
                      {item.label}
                    </>
                  )}
                </NavLink>
              </li>
            ))}
          </ul>
        </div>
      ))}
    </nav>
  )
}

export function Layout() {
  const { user, serverName, signOut, mustChangePassword, serverVersion } = useSession()
  const [drawerOpen, setDrawerOpen] = useState(false)
  const location = useLocation()

  useEffect(() => {
    setDrawerOpen(false)
  }, [location.pathname])

  return (
    <div className="flex min-h-full bg-slate-950">
      {/* Desktop sidebar */}
      <aside className="fixed inset-y-0 left-0 hidden w-56 flex-col border-r border-slate-800/90 bg-slate-900/45 lg:flex">
        <Brand />
        <SidebarNav />
        <div className="flex items-center justify-between gap-2 border-t border-slate-800/80 px-4 py-2.5">
          <p className="truncate text-micro text-slate-600">Signed in as {user.username}</p>
          {serverVersion ? (
            <span className="shrink-0 font-mono text-micro text-slate-600" title="Server version">
              v{serverVersion}
            </span>
          ) : null}
        </div>
      </aside>

      {/* Mobile drawer */}
      {drawerOpen ? (
        <div className="fixed inset-0 z-40 lg:hidden">
          <button
            type="button"
            aria-label="Close navigation"
            onClick={() => setDrawerOpen(false)}
            className="pb-overlay-in absolute inset-0 cursor-default bg-slate-950/80 backdrop-blur-sm"
          />
          <aside className="pb-modal-in relative flex h-full w-56 flex-col border-r border-slate-800 bg-slate-900">
            <Brand />
            <SidebarNav onNavigate={() => setDrawerOpen(false)} />
          </aside>
        </div>
      ) : null}

      <div className="flex min-w-0 flex-1 flex-col lg:pl-56">
        <header className="sticky top-0 z-30 flex h-14 items-center gap-3 border-b border-slate-800/90 bg-slate-950/85 px-4 backdrop-blur-md sm:px-6">
          <button
            type="button"
            onClick={() => setDrawerOpen(true)}
            className="-ml-1 rounded-lg p-2 text-slate-400 transition-colors duration-150 hover:bg-slate-800 hover:text-slate-100 lg:hidden"
            aria-label="Open navigation"
          >
            {drawerOpen ? <X className="size-[18px]" aria-hidden /> : <Menu className="size-[18px]" aria-hidden />}
          </button>

          <div className="flex min-w-0 items-center gap-2.5">
            <span className="truncate text-[13px] font-medium text-slate-200">{serverName}</span>
            <span className="hidden rounded border border-slate-800 bg-slate-900 px-1.5 py-0.5 text-micro tracking-[0.1em] text-slate-500 uppercase sm:inline">
              Backup server
            </span>
          </div>

          <div className="ml-auto flex items-center gap-2">
            <span className="hidden text-meta text-slate-500 sm:inline">{user.username}</span>
            <Button size="sm" icon={<LogOut className="size-3.5" aria-hidden />} onClick={signOut}>
              Sign out
            </Button>
          </div>
        </header>

        <main className="min-w-0 flex-1 px-4 py-6 sm:px-6 lg:px-8">
          <div className="mx-auto max-w-7xl">
            {mustChangePassword ? (
              <div className="mb-5 flex flex-wrap items-center gap-x-3 gap-y-2 rounded-xl border border-amber-500/30 bg-amber-500/10 px-4 py-3 text-sm text-amber-200">
                <TriangleAlert className="size-4 shrink-0 text-amber-400" aria-hidden />
                <span className="min-w-0 flex-1">
                  This server still uses the default <span className="font-semibold">admin</span> /{' '}
                  <span className="font-semibold">admin</span> password. Anyone who can reach it can
                  sign in.
                </span>
                <NavLink
                  to="/settings"
                  className="rounded-lg border border-amber-500/40 px-3 py-1.5 text-xs font-medium text-amber-200 transition-colors duration-150 hover:bg-amber-500/15"
                >
                  Change password
                </NavLink>
              </div>
            ) : null}
            <Outlet />
          </div>
        </main>
      </div>
    </div>
  )
}
