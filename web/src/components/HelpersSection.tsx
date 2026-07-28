/**
 * Node helpers.
 *
 * A helper is a root service on one Proxmox node that streams `vzdump` out and
 * `qmrestore` in. From v0.5.0 it is identified by **(cluster, node)**, not by
 * node name alone: two clusters can each contain a `pve1`, and resolving by
 * name alone could route a backup — or worse, a restore — to the wrong
 * physical machine.
 *
 * A helper that predates that change carries no cluster. It is shown as
 * **unassigned**, it is stated plainly that it routes nothing, and the only
 * remedy offered is to bind it to a cluster or re-deploy it. Nothing here ever
 * guesses which cluster a bare node name belongs to.
 */

import { useCallback, useMemo, useState } from 'react'
import { KeyRound, Link2, RefreshCw, ServerCog, Trash2 } from 'lucide-react'
import {
  assignHelper,
  createHelperEnrollToken,
  deleteHelper,
  errorMessage,
  HELPER_DOWNLOAD,
  isHelperUnassigned,
  listHelpers,
  listVMs,
} from '../api'
import type { EnrollToken, Helper, Host, ID } from '../api'
import { useConfirm } from './Confirm'
import { CopyField } from './CopyField'
import { DeployHelperModal } from './DeployHelperModal'
import { useToast } from './Toast'
import {
  Button,
  Card,
  CardHeader,
  Disclosure,
  ErrorBlock,
  Hint,
  IconButton,
  Num,
  SectionNote,
  Select,
  StatusPill,
  toneForStatus,
} from './ui'
import { roleDeniedReason, useSession } from '../session'
import { useAsync } from '../lib/useAsync'
import { formatRelative } from '../lib/format'

function helperInstallCommand(origin: string, token: string): string {
  return [
    `curl -fsSL ${origin}${HELPER_DOWNLOAD} -o /usr/local/bin/proxback-helper`,
    'chmod +x /usr/local/bin/proxback-helper',
    `/usr/local/bin/proxback-helper --server ${origin} --token ${token} --install`,
  ].join(' && ')
}

/** Inline "bind this helper to a cluster" control for an unassigned helper. */
function AssignControl({
  helper,
  hosts,
  onAssigned,
}: {
  helper: Helper
  hosts: Host[]
  onAssigned: () => void
}) {
  const toast = useToast()
  const [hostId, setHostId] = useState<string>('')
  const [busy, setBusy] = useState(false)

  const submit = async () => {
    if (!hostId) return
    setBusy(true)
    try {
      await assignHelper(helper.id, hostId as ID)
      const host = hosts.find((item) => String(item.id) === hostId)
      toast.success(
        `Helper for ${helper.node} bound to ${host?.name ?? 'the selected cluster'}.`,
        'It can now be used to route backups and restores for that node.',
      )
      onAssigned()
    } catch (err) {
      toast.error('Could not bind the helper', errorMessage(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="flex flex-wrap items-center gap-2">
      <Select
        value={hostId}
        onChange={(event) => setHostId(event.target.value)}
        className="w-48"
        aria-label={`Cluster for the helper on ${helper.node}`}
      >
        <option value="">Choose a cluster…</option>
        {hosts.map((host) => (
          <option key={String(host.id)} value={String(host.id)}>
            {host.name}
          </option>
        ))}
      </Select>
      <Button
        size="sm"
        variant="primary"
        disabled={!hostId}
        loading={busy}
        icon={<Link2 className="size-3.5" aria-hidden />}
        onClick={() => void submit()}
      >
        Bind
      </Button>
    </div>
  )
}

export function HelpersSection({ hosts }: { hosts: Host[] }) {
  const toast = useToast()
  const confirm = useConfirm()
  const { can, role } = useSession()
  const denied = can.manageInfrastructure
    ? undefined
    : roleDeniedReason(role, 'deploy or remove node helpers')
  const loader = useCallback(() => listHelpers(), [])
  const { data, loading, error, reload, refresh } = useAsync(loader)
  const helpers = useMemo(() => data ?? [], [data])

  const vmLoader = useCallback(() => listVMs(), [])
  const { data: vmData } = useAsync(vmLoader)

  const [token, setToken] = useState<EnrollToken | null>(null)
  const [tokenHostId, setTokenHostId] = useState<string>('')
  const [generating, setGenerating] = useState(false)
  const [deleting, setDeleting] = useState<string | null>(null)
  const [deployOpen, setDeployOpen] = useState(false)
  const origin = typeof window === 'undefined' ? '' : window.location.origin

  /** Node names per cluster, from the cached inventory. */
  const nodesByHost = useMemo(() => {
    const map: Record<string, string[]> = {}
    for (const vm of vmData ?? []) {
      const key = String(vm.hostId)
      const list = map[key] ?? (map[key] = [])
      if (!list.includes(vm.node)) list.push(vm.node)
    }
    for (const key of Object.keys(map)) map[key]?.sort()
    return map
  }, [vmData])

  const unassigned = helpers.filter(isHelperUnassigned)

  /** (cluster, node) pairs in the inventory with no helper bound to them. */
  const uncovered = useMemo(() => {
    const covered = new Set(
      helpers
        .filter((helper) => !isHelperUnassigned(helper))
        .map((helper) => `${String(helper.hostId)}:${helper.node}`),
    )
    const gaps: { hostId: string; hostName: string; node: string }[] = []
    for (const [hostId, nodes] of Object.entries(nodesByHost)) {
      const hostName = hosts.find((host) => String(host.id) === hostId)?.name ?? hostId
      for (const node of nodes) {
        if (!covered.has(`${hostId}:${node}`)) gaps.push({ hostId, hostName, node })
      }
    }
    return gaps
  }, [helpers, nodesByHost, hosts])

  // Prefill the SSH address from the configured host URL when it is an IP/host.
  const defaultAddress = (() => {
    const raw = hosts[0]?.baseUrl ?? ''
    try {
      return raw ? new URL(raw).hostname : ''
    } catch {
      return ''
    }
  })()

  const onGenerate = async () => {
    if (!tokenHostId) return
    setGenerating(true)
    try {
      const next = await createHelperEnrollToken(tokenHostId as ID)
      setToken(next)
      toast.success(
        'Helper enrollment token generated.',
        'Single use, expires in 24 hours, and carries the cluster — the node never has to know its own.',
      )
    } catch (err) {
      toast.error('Could not generate token', errorMessage(err))
    } finally {
      setGenerating(false)
    }
  }

  const onDelete = async (helper: Helper) => {
    const ok = await confirm({
      title: 'Remove node helper',
      message: (
        <>
          Remove the helper for{' '}
          <span className="font-medium text-slate-100">
            {helper.hostName ? `${helper.hostName} / ${helper.node}` : helper.node}
          </span>
          ? Agentless image backups and restores for workloads on that node stop working until a
          helper is deployed again.
        </>
      ),
      confirmLabel: 'Remove helper',
    })
    if (!ok) return
    setDeleting(String(helper.id))
    try {
      await deleteHelper(helper.id)
      toast.success(`Helper for “${helper.node}” removed.`)
      void refresh()
    } catch (err) {
      toast.error('Could not remove helper', errorMessage(err))
    } finally {
      setDeleting(null)
    }
  }

  return (
    <Card className="mt-6">
      <CardHeader
        title="Node helpers"
        subtitle="One per Proxmox node. Required for agentless image backup of a real cluster."
        actions={
          <>
            <Button
              size="sm"
              icon={<RefreshCw className="size-3.5" aria-hidden />}
              onClick={() => void reload()}
              loading={loading}
            >
              Refresh
            </Button>
            <Button
              size="sm"
              variant="primary"
              icon={<ServerCog className="size-3.5" aria-hidden />}
              onClick={() => setDeployOpen(true)}
              disabled={!can.manageInfrastructure}
              title={denied}
              aria-label={denied ? `Deploy helper — ${denied}` : undefined}
            >
              Deploy helper
            </Button>
          </>
        }
      />

      {unassigned.length > 0 ? (
        <div className="border-b border-slate-800 bg-warn-500/[0.07] px-5 py-3.5">
          <p className="text-[13px] font-medium text-warn-200">
            {unassigned.length === 1
              ? '1 helper is not bound to a cluster and is not being used.'
              : `${unassigned.length} helpers are not bound to a cluster and are not being used.`}
          </p>
          <p className="mt-1 text-xs leading-relaxed text-slate-400">
            Helpers are identified by cluster and node together. These registered before that was
            true, so ProxBack cannot tell which cluster they belong to — and will not guess. Bind
            each one below, or re-deploy it.
          </p>
        </div>
      ) : null}

      {error && !data ? (
        <div className="px-5 py-5">
          <ErrorBlock message={error} onRetry={() => void reload()} />
        </div>
      ) : helpers.length > 0 ? (
        <div className="overflow-x-auto">
          <table className="w-full min-w-[52rem] text-sm">
            <thead>
              <tr className="border-b border-slate-800 text-left text-micro font-semibold tracking-wide text-slate-500 uppercase">
                <th scope="col" className="px-5 py-2.5">
                  Cluster
                </th>
                <th scope="col" className="px-5 py-2.5">
                  Node
                </th>
                <th scope="col" className="px-5 py-2.5">
                  Address
                </th>
                <th scope="col" className="px-5 py-2.5">
                  Version
                </th>
                <th scope="col" className="px-5 py-2.5">
                  Status
                </th>
                <th scope="col" className="px-5 py-2.5">
                  Last seen
                </th>
                <th scope="col" className="relative px-5 py-2.5">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody className="divide-y divide-slate-800/70">
              {helpers.map((helper) => {
                const orphan = isHelperUnassigned(helper)
                return (
                  <tr
                    key={String(helper.id)}
                    className="align-top transition-colors duration-150 hover:bg-slate-800/30"
                  >
                    <td className="px-5 py-3">
                      {orphan ? (
                        <div className="space-y-2">
                          <StatusPill tone="warn" label="Unassigned" />
                          <AssignControl
                            helper={helper}
                            hosts={hosts}
                            onAssigned={() => void refresh()}
                          />
                        </div>
                      ) : (
                        <span className="text-slate-200">{helper.hostName || '—'}</span>
                      )}
                    </td>
                    <td className="px-5 py-3 font-mono text-xs whitespace-nowrap text-slate-300">
                      {helper.node}
                    </td>
                    <td className="px-5 py-3 font-mono text-xs whitespace-nowrap text-slate-400">
                      {helper.address}:{helper.port}
                    </td>
                    <td className="px-5 py-3 font-mono text-xs whitespace-nowrap text-slate-400">
                      {helper.version}
                    </td>
                    <td className="px-5 py-3">
                      {orphan ? (
                        <span className="text-xs text-warn-300">Not used for routing</span>
                      ) : (
                        <StatusPill tone={toneForStatus(helper.status)} label={helper.status} />
                      )}
                    </td>
                    <td className="px-5 py-3 whitespace-nowrap">
                      <Num className="text-slate-400">{formatRelative(helper.lastSeen)}</Num>
                    </td>
                    <td className="px-5 py-3 text-right">
                      <IconButton
                        variant="dangerQuiet"
                        aria-label={
                          denied
                            ? `Remove the helper for ${helper.node} — ${denied}`
                            : `Remove the helper for ${helper.node}`
                        }
                        title={denied ?? 'Remove helper'}
                        disabled={!can.manageInfrastructure}
                        loading={deleting === String(helper.id)}
                        onClick={() => void onDelete(helper)}
                      >
                        <Trash2 className="size-4" aria-hidden />
                      </IconButton>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      ) : (
        <div className="px-5 py-5">
          <SectionNote>
            No node helpers yet — agentless image backups fail until each node has one. Deploy
            helper installs one over SSH for you.
          </SectionNote>
        </div>
      )}

      {uncovered.length > 0 && helpers.length > 0 ? (
        <div className="border-t border-slate-800 px-5 py-3.5">
          <p className="text-xs text-warn-300">
            {uncovered.length === 1
              ? `${uncovered[0]?.hostName} / ${uncovered[0]?.node} has no helper — workloads on it cannot be image-backed-up yet.`
              : `${uncovered.length} nodes have no helper: ${uncovered
                  .map((gap) => `${gap.hostName} / ${gap.node}`)
                  .join(', ')}.`}
          </p>
        </div>
      ) : null}

      <div className="border-t border-slate-800 px-5 py-4">
        <Disclosure summary="Install manually" hint="for nodes this server cannot reach by SSH">
          <div className="space-y-4">
            <div className="flex flex-wrap items-end gap-2">
              <label className="flex-1">
                <span className="mb-1.5 block text-xs font-medium text-slate-400">
                  Cluster this node belongs to
                </span>
                <Select
                  value={tokenHostId}
                  onChange={(event) => setTokenHostId(event.target.value)}
                  aria-label="Cluster for the enrollment token"
                >
                  <option value="">Choose a cluster…</option>
                  {hosts.map((host) => (
                    <option key={String(host.id)} value={String(host.id)}>
                      {host.name}
                    </option>
                  ))}
                </Select>
              </label>
              <Button
                loading={generating}
                disabled={!tokenHostId || !can.manageInfrastructure}
                title={denied}
                icon={<KeyRound className="size-4" aria-hidden />}
                onClick={() => void onGenerate()}
              >
                {token ? 'Generate another token' : 'Generate token'}
              </Button>
            </div>

            <Hint>
              The token carries the cluster, so the helper enrolls against the right one without the
              node knowing anything about its own identity.
            </Hint>

            {token ? (
              <>
                <CopyField
                  label="Enrollment token"
                  value={token.token}
                  copyLabel="Token"
                  caption={`Single use — expires ${formatRelative(token.expiresAt)}. Generate one token per node.`}
                />
                <CopyField
                  label="Proxmox node — root shell"
                  value={helperInstallCommand(origin, token.token)}
                  copyLabel="Helper install command"
                  caption="Downloads the helper, installs its service, and registers this node."
                />
              </>
            ) : null}
          </div>
        </Disclosure>
      </div>

      <DeployHelperModal
        open={deployOpen}
        onClose={() => setDeployOpen(false)}
        onDeployed={() => void refresh()}
        hosts={hosts}
        nodesByHost={nodesByHost}
        defaultAddress={defaultAddress}
      />
    </Card>
  )
}
