import { useCallback, useMemo, useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { Cpu, HardDrive, Laptop, Play, RefreshCw, Search, Server } from 'lucide-react'
import {
  errorMessage,
  getHostVMs,
  listAgents,
  listHosts,
  listJobs,
  listTargets,
  listVMs,
  runJob,
  vmSourcesOf,
} from '../api'
import type { Agent, CachedVM, Host, Job, Target } from '../api'
import { JobWizard } from '../components/JobWizard'
import { useToast } from '../components/Toast'
import {
  Button,
  Card,
  EmptyState,
  ErrorBlock,
  Input,
  PageHeader,
  Select,
  SkeletonCards,
  StatusPill,
  toneForStatus,
} from '../components/ui'
import { useAsync } from '../lib/useAsync'
import { formatBytes, formatUptime } from '../lib/format'

interface VMsData {
  vms: CachedVM[]
  hosts: Host[]
  jobs: Job[]
  targets: Target[]
  agents: Agent[]
}

/** The first enabled job that already protects this VM, else any job that does. */
function jobForVM(jobs: Job[], vm: CachedVM): Job | null {
  const matches = jobs.filter(
    (job) =>
      job.kind === 'vm' &&
      vmSourcesOf(job).some(
        (source) => String(source.hostId) === String(vm.hostId) && source.vmid === vm.vmid,
      ),
  )
  return matches.find((job) => job.enabled) ?? matches[0] ?? null
}

function VMCard({
  vm,
  job,
  onBackup,
  busy,
}: {
  vm: CachedVM
  job: Job | null
  onBackup: () => void
  busy: boolean
}) {
  return (
    <Card className="flex flex-col p-5">
      <div className="flex items-start justify-between gap-3">
        <div className="flex min-w-0 items-start gap-3">
          <div className="flex size-9 shrink-0 items-center justify-center rounded-lg border border-slate-800 bg-slate-950/60 text-accent-400">
            <Laptop className="size-4" aria-hidden />
          </div>
          <div className="min-w-0">
            <p className="truncate text-sm font-semibold text-slate-100">{vm.name}</p>
            <p className="truncate text-xs text-slate-500">
              <span className="font-mono">#{vm.vmid}</span> · {vm.hostName} · {vm.node}
            </p>
          </div>
        </div>
        <StatusPill tone={toneForStatus(vm.status)} label={vm.status} />
      </div>

      <div className="mt-5 grid grid-cols-2 gap-3">
        <div className="rounded-lg border border-slate-800/80 bg-slate-950/40 px-3 py-2.5">
          <p className="flex items-center gap-1.5 text-[11px] tracking-wide text-slate-500 uppercase">
            <HardDrive className="size-3" aria-hidden />
            Disk
          </p>
          <p className="mt-1 text-sm text-slate-200">{formatBytes(vm.maxdisk)}</p>
        </div>
        <div className="rounded-lg border border-slate-800/80 bg-slate-950/40 px-3 py-2.5">
          <p className="flex items-center gap-1.5 text-[11px] tracking-wide text-slate-500 uppercase">
            <Cpu className="size-3" aria-hidden />
            Memory
          </p>
          <p className="mt-1 text-sm text-slate-200">{formatBytes(vm.maxmem)}</p>
        </div>
      </div>

      <p className="mt-3 text-xs text-slate-500">
        {vm.status === 'running' ? `Up ${formatUptime(vm.uptime)}` : 'Not running'}
        {job ? (
          <>
            {' · '}
            <span className="text-slate-400">Protected by “{job.name}”</span>
          </>
        ) : (
          <>
            {' · '}
            <span className="text-amber-400/80">Not in any job</span>
          </>
        )}
      </p>

      <div className="mt-auto flex items-center gap-2 border-t border-slate-800 pt-4">
        <Button
          size="sm"
          variant="primary"
          className="w-full"
          loading={busy}
          icon={<Play className="size-3.5" aria-hidden />}
          onClick={onBackup}
        >
          {job ? 'Backup Now' : 'Backup Now — create job'}
        </Button>
      </div>
    </Card>
  )
}

export function VirtualMachinesPage() {
  const toast = useToast()
  const navigate = useNavigate()
  const loader = useCallback(async (): Promise<VMsData> => {
    const [vms, hosts, jobs, targets, agents] = await Promise.all([
      listVMs(),
      listHosts(),
      listJobs(),
      listTargets(),
      listAgents(),
    ])
    return { vms, hosts, jobs, targets, agents }
  }, [])
  const { data, loading, error, reload, refresh } = useAsync(loader)

  const [query, setQuery] = useState('')
  const [hostFilter, setHostFilter] = useState('all')
  const [refreshing, setRefreshing] = useState(false)
  const [busyVM, setBusyVM] = useState<string | null>(null)
  const [wizardVM, setWizardVM] = useState<CachedVM | null>(null)
  const [wizardOpen, setWizardOpen] = useState(false)

  const vms = useMemo(() => data?.vms ?? [], [data])
  const hosts = useMemo(() => data?.hosts ?? [], [data])
  const jobs = useMemo(() => data?.jobs ?? [], [data])

  const filtered = useMemo(() => {
    const needle = query.trim().toLowerCase()
    return vms.filter((vm) => {
      if (hostFilter !== 'all' && String(vm.hostId) !== hostFilter) return false
      if (!needle) return true
      return (
        vm.name.toLowerCase().includes(needle) ||
        String(vm.vmid).includes(needle) ||
        vm.node.toLowerCase().includes(needle) ||
        vm.hostName.toLowerCase().includes(needle)
      )
    })
  }, [vms, query, hostFilter])

  /** Re-poll every host’s live inventory, then reload the cached list. */
  const onRefreshInventory = async () => {
    if (hosts.length === 0) {
      toast.info('No Proxmox hosts to inventory.', 'Add a host first.')
      return
    }
    setRefreshing(true)
    try {
      const results = await Promise.allSettled(hosts.map((host) => getHostVMs(host.id)))
      const ok = results.filter((result) => result.status === 'fulfilled').length
      const found = results.reduce(
        (sum, result) => (result.status === 'fulfilled' ? sum + result.value.length : sum),
        0,
      )
      const failures = results.length - ok
      await refresh()
      if (failures === 0) {
        toast.success(
          'Inventory refreshed.',
          `${found} ${found === 1 ? 'virtual machine' : 'virtual machines'} across ${ok} ${
            ok === 1 ? 'host' : 'hosts'
          }.`,
        )
      } else {
        toast.error(
          'Some hosts could not be reached.',
          `${ok} of ${results.length} hosts answered. Check host status on the Proxmox Hosts page.`,
        )
      }
    } catch (err) {
      toast.error('Inventory refresh failed', errorMessage(err))
    } finally {
      setRefreshing(false)
    }
  }

  const onBackupNow = async (vm: CachedVM) => {
    const job = jobForVM(jobs, vm)
    if (!job) {
      setWizardVM(vm)
      setWizardOpen(true)
      return
    }

    const key = `${String(vm.hostId)}:${vm.vmid}`
    setBusyVM(key)
    try {
      await runJob(job.id)
      toast.success(`Backup of ${vm.name} started.`, `Running job “${job.name}”.`)
      navigate(`/monitor?jobId=${encodeURIComponent(String(job.id))}`)
    } catch (err) {
      toast.error(`Could not back up ${vm.name}`, errorMessage(err))
    } finally {
      setBusyVM(null)
    }
  }

  return (
    <>
      <PageHeader
        title="Virtual Machines"
        description="Cached inventory across every Proxmox host. Refresh to re-poll the hosts."
        actions={
          <>
            <Button
              icon={<RefreshCw className="size-4" aria-hidden />}
              onClick={() => void onRefreshInventory()}
              loading={refreshing}
            >
              Refresh inventory
            </Button>
          </>
        }
      />

      {loading && !data ? (
        <SkeletonCards count={6} height="h-56" />
      ) : error && !data ? (
        <ErrorBlock message={error} onRetry={() => void reload()} />
      ) : vms.length === 0 ? (
        <EmptyState
          icon={<Server className="size-5" aria-hidden />}
          title="No virtual machines in the inventory"
          description={
            hosts.length === 0
              ? 'ProxBack has no Proxmox hosts yet. Add one with an API token and its guests appear here.'
              : 'Your hosts are configured but the inventory is empty. Refresh the inventory to re-poll each host.'
          }
          action={
            hosts.length === 0 ? (
              <Link to="/hosts">
                <Button variant="primary" icon={<Server className="size-4" aria-hidden />}>
                  Add Proxmox host
                </Button>
              </Link>
            ) : (
              <Button
                variant="primary"
                icon={<RefreshCw className="size-4" aria-hidden />}
                loading={refreshing}
                onClick={() => void onRefreshInventory()}
              >
                Refresh inventory
              </Button>
            )
          }
        />
      ) : (
        <>
          <div className="mb-4 flex flex-wrap items-center gap-3">
            <div className="relative">
              <Search
                className="pointer-events-none absolute top-2.5 left-3 size-4 text-slate-600"
                aria-hidden
              />
              <Input
                value={query}
                placeholder="Search by name, VMID, or node"
                className="w-72 pl-9"
                onChange={(event) => setQuery(event.target.value)}
              />
            </div>
            <Select
              value={hostFilter}
              onChange={(event) => setHostFilter(event.target.value)}
              className="w-52"
              aria-label="Filter by Proxmox host"
            >
              <option value="all">All hosts</option>
              {hosts.map((host) => (
                <option key={String(host.id)} value={String(host.id)}>
                  {host.name}
                </option>
              ))}
            </Select>
            <p className="ml-auto text-xs text-slate-500">
              {filtered.length} of {vms.length} shown
            </p>
          </div>

          {filtered.length === 0 ? (
            <EmptyState
              icon={<Search className="size-5" aria-hidden />}
              title="Nothing matches that filter"
              description="Try a different name, VMID, node, or host."
              action={
                <Button
                  onClick={() => {
                    setQuery('')
                    setHostFilter('all')
                  }}
                >
                  Clear filters
                </Button>
              }
            />
          ) : (
            <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
              {filtered.map((vm) => {
                const key = `${String(vm.hostId)}:${vm.vmid}`
                return (
                  <VMCard
                    key={key}
                    vm={vm}
                    job={jobForVM(jobs, vm)}
                    busy={busyVM === key}
                    onBackup={() => void onBackupNow(vm)}
                  />
                )
              })}
            </div>
          )}
        </>
      )}

      <JobWizard
        open={wizardOpen}
        onClose={() => setWizardOpen(false)}
        onSaved={() => void refresh()}
        targets={data?.targets ?? []}
        vms={vms}
        agents={data?.agents ?? []}
        initialVM={wizardVM}
      />
    </>
  )
}
