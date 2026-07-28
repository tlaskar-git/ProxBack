import { useEffect, useMemo, useState } from 'react'
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
  ScrollText,
  Server,
  Settings as SettingsIcon,
  ShieldCheck,
  TriangleAlert,
  Users,
  X,
} from 'lucide-react'
import type { LucideIcon } from 'lucide-react'
import { ROLE_LABEL } from '../api'
import type { Capabilities, Role } from '../api'
import { cn } from '../lib/cn'
import { useSession } from '../session'
import { ThemeToggle } from '../theme'
import { BrandLockup } from './Brand'
import { Button } from './ui'

interface NavItem {
  to: string
  label: string
  icon: LucideIcon
  end?: boolean
  /**
   * Capability required to see this item at all. Nav is the one place hiding is
   * the right call: a page whose every control is refused is not a page, and
   * the server refuses it anyway.
   */
  needs?: keyof Capabilities
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
    items: [
      { to: '/users', label: 'Users', icon: Users, needs: 'manageUsers' },
      { to: '/audit', label: 'Audit', icon: ScrollText, needs: 'viewAudit' },
      { to: '/settings', label: 'Settings', icon: SettingsIcon },
    ],
  },
]

/** Drops the items this role cannot use, and any group left empty by that. */
function navFor(can: Capabilities): { heading: string; items: NavItem[] }[] {
  return NAV_GROUPS.map((group) => ({
    heading: group.heading,
    items: group.items.filter((item) => (item.needs ? can[item.needs] : true)),
  })).filter((group) => group.items.length > 0)
}

/**
 * The signed-in role, beside the name. Slate rather than a status colour: a
 * role is an attribute of the account, not a verdict about the estate.
 */
function RoleBadge({ role, className }: { role: Role; className?: string }) {
  return (
    <span
      className={cn(
        'shrink-0 rounded border border-slate-800 bg-slate-900 px-1.5 py-0.5 text-micro font-medium tracking-[0.08em] text-slate-500 uppercase',
        className,
      )}
      title={`Signed in with the ${ROLE_LABEL[role].toLowerCase()} role`}
    >
      {role}
    </span>
  )
}

function SidebarBrand() {
  return (
    <div className="flex flex-col items-center border-b border-slate-800/80 px-4 py-6">
      <BrandLockup size="lg" subtitle="Proxmox VE recovery" />
    </div>
  )
}

/**
 * Dense nav: 32px rows, a section rule instead of a floating caption, and an
 * active state that is a filled slab with a hard accent edge — visible from
 * the far side of the screen, unlike a tint alone.
 */
function SidebarNav({
  groups,
  onNavigate,
}: {
  groups: { heading: string; items: NavItem[] }[]
  onNavigate?: () => void
}) {
  return (
    <nav className="flex-1 overflow-y-auto px-2.5 py-3">
      {groups.map((group, index) => (
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
                        ? 'bg-slate-800/80 font-medium text-slate-50 before:absolute before:top-1.5 before:bottom-1.5 before:-left-2.5 before:w-[3px] before:rounded-r-full before:bg-accent-400'
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
  const { user, role, can, serverName, signOut, mustChangePassword, serverVersion } = useSession()
  const [drawerOpen, setDrawerOpen] = useState(false)
  const location = useLocation()
  const groups = useMemo(() => navFor(can), [can])

  useEffect(() => {
    setDrawerOpen(false)
  }, [location.pathname])

  return (
    <div className="flex min-h-full bg-slate-950">
      {/* Desktop sidebar */}
      <aside className="fixed inset-y-0 left-0 hidden w-56 flex-col border-r border-slate-800/90 bg-slate-900/45 lg:flex">
        <SidebarBrand />
        <SidebarNav groups={groups} />
        <div className="border-t border-slate-800/80 px-4 py-2.5">
          <div className="flex items-center gap-2">
            <p className="min-w-0 flex-1 truncate text-micro text-slate-600">
              Signed in as {user.username}
            </p>
            {serverVersion ? (
              <span className="shrink-0 font-mono text-micro text-slate-600" title="Server version">
                v{serverVersion}
              </span>
            ) : null}
          </div>
          <RoleBadge role={role} className="mt-1.5 inline-block" />
        </div>
      </aside>

      {/* Mobile drawer */}
      {drawerOpen ? (
        <div className="fixed inset-0 z-40 lg:hidden">
          <button
            type="button"
            aria-label="Close navigation"
            onClick={() => setDrawerOpen(false)}
            className="scrim pb-overlay-in absolute inset-0 cursor-default"
          />
          <aside className="pb-modal-in relative flex h-full w-56 flex-col border-r border-slate-800 bg-slate-900">
            <SidebarBrand />
            <SidebarNav groups={groups} onNavigate={() => setDrawerOpen(false)} />
            <div className="border-t border-slate-800/80 px-4 py-2.5">
              <p className="truncate text-micro text-slate-600">Signed in as {user.username}</p>
              <RoleBadge role={role} className="mt-1.5 inline-block" />
            </div>
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
            <ThemeToggle />
            <span className="hidden items-center gap-1.5 text-meta text-slate-500 sm:inline-flex">
              {user.username}
              <RoleBadge role={role} />
            </span>
            <Button size="sm" icon={<LogOut className="size-3.5" aria-hidden />} onClick={signOut}>
              Sign out
            </Button>
          </div>
        </header>

        <main className="min-w-0 flex-1 px-4 py-6 sm:px-6 lg:px-8">
          <div className="mx-auto max-w-7xl">
            {mustChangePassword ? (
              <div className="mb-5 flex flex-wrap items-center gap-x-3 gap-y-2 rounded-xl border border-warn-500/30 bg-warn-500/10 px-4 py-3 text-sm text-warn-200">
                <TriangleAlert className="size-4 shrink-0 text-warn-400" aria-hidden />
                <span className="min-w-0 flex-1">
                  This server still uses the default <span className="font-semibold">admin</span> /{' '}
                  <span className="font-semibold">admin</span> password. Anyone who can reach it can
                  sign in.
                </span>
                <NavLink
                  to="/settings"
                  className="rounded-lg border border-warn-500/40 px-3 py-1.5 text-xs font-medium text-warn-200 transition-colors duration-150 hover:bg-warn-500/15"
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
