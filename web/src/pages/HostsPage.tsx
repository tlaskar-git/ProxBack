import { useCallback, useState } from 'react'
import type { FormEvent } from 'react'
import { KeyRound, Plug, Plus, RefreshCw, Server, Trash2 } from 'lucide-react'
import {
  createHelperEnrollToken,
  createHost,
  deleteHelper,
  deleteHost,
  errorMessage,
  HELPER_DOWNLOAD,
  listHelpers,
  listHosts,
  testHost,
} from '../api'
import type { EnrollToken, Helper, Host, HostCreate, ID } from '../api'
import { CopyField } from '../components/CopyField'
import { Modal } from '../components/Modal'
import { useConfirm } from '../components/Confirm'
import { useToast } from '../components/Toast'
import {
  Button,
  Card,
  CardHeader,
  Checkbox,
  EmptyState,
  ErrorBlock,
  Field,
  IconButton,
  Input,
  Metric,
  Num,
  PageHeader,
  SectionNote,
  SkeletonCards,
  StatusPill,
  toneForStatus,
} from '../components/ui'
import { useAsync } from '../lib/useAsync'
import { formatRelative } from '../lib/format'

const EMPTY_FORM: HostCreate = {
  name: '',
  baseUrl: '',
  tokenId: '',
  tokenSecret: '',
  insecureTLS: false,
}

function AddHostModal({
  open,
  onClose,
  onCreated,
}: {
  open: boolean
  onClose: () => void
  onCreated: (host: Host) => void
}) {
  const toast = useToast()
  const [form, setForm] = useState<HostCreate>(EMPTY_FORM)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const patch = (next: Partial<HostCreate>) => setForm((current) => ({ ...current, ...next }))

  const close = () => {
    setForm(EMPTY_FORM)
    setError(null)
    onClose()
  }

  const onSubmit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    setError(null)

    if (!form.name.trim()) return setError('Give the host a display name.')
    if (!/^https?:\/\//i.test(form.baseUrl.trim())) {
      return setError('The base URL must start with https:// or http://.')
    }
    if (!form.tokenId.trim()) return setError('Enter the API token ID.')
    if (!form.tokenSecret.trim()) return setError('Enter the API token secret.')

    setSubmitting(true)
    try {
      const host = await createHost({
        name: form.name.trim(),
        baseUrl: form.baseUrl.trim().replace(/\/+$/, ''),
        tokenId: form.tokenId.trim(),
        tokenSecret: form.tokenSecret,
        insecureTLS: form.insecureTLS,
      })
      toast.success(`Host “${host.name}” added.`, 'Verifying node access…')
      onCreated(host)
      close()

      // Immediately probe the new host so the card shows a real status.
      try {
        const test = await testHost(host.id)
        if (test.ok && test.warning) {
          toast.error(`${host.name}: connected, but no guests are visible`, test.warning)
        } else if (test.ok) {
          toast.success(
            `${host.name} is reachable.`,
            `${test.nodes} ${test.nodes === 1 ? 'node' : 'nodes'} visible through the API.`,
          )
        } else {
          toast.error(
            `${host.name} is not reachable`,
            test.error ?? 'The host rejected the API token.',
          )
        }
      } catch (err) {
        toast.error('Test Connection failed', errorMessage(err))
      } finally {
        onCreated(host)
      }
    } catch (err) {
      const message = errorMessage(err)
      setError(message)
      toast.error('Could not add host', message)
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Modal
      open={open}
      onClose={close}
      title="Add Proxmox host"
      subtitle="ProxBack talks to the Proxmox VE API with an API token — no root password required."
      width="lg"
      footer={
        <>
          <Button onClick={close}>Cancel</Button>
          <Button
            variant="primary"
            form="add-host-form"
            type="submit"
            loading={submitting}
            icon={<Plus className="size-4" aria-hidden />}
          >
            Add host
          </Button>
        </>
      }
    >
      <form id="add-host-form" className="space-y-4" onSubmit={(event) => void onSubmit(event)} noValidate>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Display name">
            {({ id }) => (
              <Input
                id={id}
                value={form.name}
                autoFocus
                placeholder="pve-cluster-01"
                onChange={(event) => patch({ name: event.target.value })}
              />
            )}
          </Field>

          <Field label="Base URL" hint="Include the port, e.g. 8006.">
            {({ id }) => (
              <Input
                id={id}
                value={form.baseUrl}
                placeholder="https://pve.example.com:8006"
                onChange={(event) => patch({ baseUrl: event.target.value })}
              />
            )}
          </Field>
        </div>

        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Token ID" hint="Format: user@realm!tokenname">
            {({ id }) => (
              <Input
                id={id}
                value={form.tokenId}
                placeholder="proxback@pve!backup"
                onChange={(event) => patch({ tokenId: event.target.value })}
              />
            )}
          </Field>

          <Field label="Token secret" hint="Stored encrypted; never returned by the API.">
            {({ id }) => (
              <Input
                id={id}
                type="password"
                value={form.tokenSecret}
                placeholder="••••••••-••••-••••-••••-••••••••••••"
                onChange={(event) => patch({ tokenSecret: event.target.value })}
              />
            )}
          </Field>
        </div>

        <Checkbox
          label="Allow self-signed certificates"
          hint="Skip TLS verification — typical for a stock Proxmox installation."
          checked={form.insecureTLS}
          onChange={(checked) => patch({ insecureTLS: checked })}
        />

        {error ? (
          <p className="rounded-lg border border-red-500/30 bg-red-500/10 px-3.5 py-2.5 text-xs text-red-300">
            {error}
          </p>
        ) : (
          <SectionNote>
            The server validates these credentials by listing cluster nodes as soon as you submit.
            Once the host is added, use Test Connection on its card to re-check reachability at any
            time.
          </SectionNote>
        )}
      </form>
    </Modal>
  )
}

function HostCard({
  host,
  onDeleted,
  onTested,
}: {
  host: Host
  onDeleted: () => void
  onTested: () => void
}) {
  const toast = useToast()
  const confirm = useConfirm()
  const [testing, setTesting] = useState(false)
  const [deleting, setDeleting] = useState(false)

  const onTest = async () => {
    setTesting(true)
    try {
      const result = await testHost(host.id)
      if (result.ok && result.warning) {
        toast.error(`${host.name}: connected, but no guests are visible`, result.warning)
      } else if (result.ok) {
        toast.success(
          `${host.name} is reachable.`,
          `${result.nodes} ${result.nodes === 1 ? 'node' : 'nodes'} visible through the API.`,
        )
      } else {
        toast.error(`${host.name} is not reachable`, result.error ?? 'The host rejected the token.')
      }
      onTested()
    } catch (err) {
      toast.error('Test failed', errorMessage(err))
    } finally {
      setTesting(false)
    }
  }

  const onDelete = async () => {
    const ok = await confirm({
      title: 'Remove Proxmox host',
      message: (
        <>
          Remove <span className="font-medium text-slate-100">{host.name}</span>? Cached VM inventory
          for this host is dropped. Existing restore points on your storage targets are not touched.
        </>
      ),
      confirmLabel: 'Remove host',
    })
    if (!ok) return

    setDeleting(true)
    try {
      await deleteHost(host.id)
      toast.success(`Host “${host.name}” removed.`)
      onDeleted()
    } catch (err) {
      toast.error('Could not remove host', errorMessage(err))
    } finally {
      setDeleting(false)
    }
  }

  return (
    <Card className="flex flex-col p-5">
      <div className="flex items-start justify-between gap-3">
        <div className="flex min-w-0 items-start gap-3">
          <div className="flex size-9 shrink-0 items-center justify-center rounded-lg border border-slate-800 bg-slate-950/60 text-accent-400">
            <Server className="size-4" aria-hidden />
          </div>
          <div className="min-w-0">
            <p className="truncate text-sm font-semibold text-slate-100">{host.name}</p>
            <p className="truncate text-xs text-slate-500">{host.baseUrl}</p>
          </div>
        </div>
        <StatusPill tone={toneForStatus(host.status)} label={host.status || 'unknown'} />
      </div>

      <dl className="mt-5 grid grid-cols-2 gap-4">
        <Metric label="Token ID" value={<span className="font-mono text-xs">{host.tokenId}</span>} />
        <Metric label="Last seen" value={<Num>{formatRelative(host.lastSeen)}</Num>} />
        <Metric label="TLS" value={host.insecureTLS ? 'Self-signed allowed' : 'Verified'} />
      </dl>

      {host.status === 'limited' ? (
        <p className="mt-4 rounded-lg border border-amber-500/30 bg-amber-500/10 px-3.5 py-2.5 text-xs leading-relaxed text-amber-300">
          The token connects but sees no guests — it likely has “Privilege Separation” enabled with
          no permissions granted. On the Proxmox host, grant the token a role with VM.Audit,
          VM.Snapshot, VM.Backup and Datastore.Audit on path <span className="font-mono">/</span>{' '}
          (Datacenter → Permissions), or recreate it with privilege separation off, then press Test
          Connection.
        </p>
      ) : null}

      <div className="mt-5 flex items-center gap-2 border-t border-slate-800 pt-4">
        <Button
          size="sm"
          onClick={() => void onTest()}
          loading={testing}
          icon={<Plug className="size-3.5" aria-hidden />}
        >
          Test Connection
        </Button>
        <IconButton
          variant="dangerQuiet"
          aria-label={`Remove ${host.name}`}
          title="Remove host"
          className="ml-auto"
          loading={deleting}
          onClick={() => void onDelete()}
        >
          <Trash2 className="size-4" aria-hidden />
        </IconButton>
      </div>
    </Card>
  )
}

export function HostsPage() {
  const loader = useCallback(() => listHosts(), [])
  const { data, loading, error, reload, refresh } = useAsync(loader)
  const [addOpen, setAddOpen] = useState(false)

  const hosts: Host[] = data ?? []
  const seen = new Set<ID>()
  const unique = hosts.filter((host) => {
    if (seen.has(host.id)) return false
    seen.add(host.id)
    return true
  })

  return (
    <>
      <PageHeader
        title="Proxmox Hosts"
        description="Clusters and standalone nodes ProxBack can inventory and snapshot."
        actions={
          <>
            <Button
              icon={<RefreshCw className="size-4" aria-hidden />}
              onClick={() => void reload()}
              loading={loading}
            >
              Refresh
            </Button>
            <Button
              variant="primary"
              icon={<Plus className="size-4" aria-hidden />}
              onClick={() => setAddOpen(true)}
            >
              Add Host
            </Button>
          </>
        }
      />

      {loading && !data ? (
        <SkeletonCards count={3} height="h-56" />
      ) : error && !data ? (
        <ErrorBlock message={error} onRetry={() => void reload()} />
      ) : unique.length === 0 ? (
        <EmptyState
          icon={<Server className="size-5" aria-hidden />}
          title="No Proxmox hosts yet"
          description="Add a host with a Proxmox VE API token so ProxBack can list its virtual machines, take snapshots, and stream disks to your storage targets."
          action={
            <Button
              variant="primary"
              icon={<Plus className="size-4" aria-hidden />}
              onClick={() => setAddOpen(true)}
            >
              Add Host
            </Button>
          }
        />
      ) : (
        <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
          {unique.map((host) => (
            <HostCard
              key={String(host.id)}
              host={host}
              onDeleted={() => void refresh()}
              onTested={() => void refresh()}
            />
          ))}
        </div>
      )}

      <HelpersSection />

      <AddHostModal
        open={addOpen}
        onClose={() => setAddOpen(false)}
        onCreated={() => void refresh()}
      />
    </>
  )
}

/* ---------------------------------------------------------------------------
 * Node helpers
 *
 * Agentless VM image backup on a real Proxmox host runs through a helper
 * service on each node (real PVE has no disk-export API). Deployment mirrors
 * the agent flow: single-use token, one command pasted on the node.
 * ------------------------------------------------------------------------- */

function helperInstallCommand(origin: string, token: string): string {
  return [
    `curl -fsSL ${origin}${HELPER_DOWNLOAD} -o /usr/local/bin/proxback-helper`,
    'chmod +x /usr/local/bin/proxback-helper',
    `/usr/local/bin/proxback-helper --server ${origin} --token ${token} --install`,
  ].join(' && ')
}

function HelpersSection() {
  const toast = useToast()
  const confirm = useConfirm()
  const loader = useCallback(() => listHelpers(), [])
  const { data, loading, error, reload, refresh } = useAsync(loader)
  const helpers = data ?? []

  const [token, setToken] = useState<EnrollToken | null>(null)
  const [generating, setGenerating] = useState(false)
  const [deleting, setDeleting] = useState<string | null>(null)
  const origin = typeof window === 'undefined' ? '' : window.location.origin

  const onGenerate = async () => {
    setGenerating(true)
    try {
      const next = await createHelperEnrollToken()
      setToken(next)
      toast.success('Helper enrollment token generated.', 'Single use — expires after 24 hours.')
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
          Remove the helper for node{' '}
          <span className="font-medium text-slate-100">{helper.node}</span>? Agentless image backups
          and restores for VMs on that node stop working until a helper is deployed again.
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
        subtitle="One per Proxmox node — streams VM image backups through vzdump and restores through qmrestore. Required for agentless backup of real hosts."
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
              loading={generating}
              icon={<KeyRound className="size-3.5" aria-hidden />}
              onClick={() => void onGenerate()}
            >
              {token ? 'Generate new token' : 'Deploy helper'}
            </Button>
          </>
        }
      />

      {error && !data ? (
        <div className="px-5 py-5">
          <ErrorBlock message={error} onRetry={() => void reload()} />
        </div>
      ) : helpers.length > 0 ? (
        <div className="overflow-x-auto">
          <table className="w-full min-w-[42rem] text-sm">
            <thead>
              <tr className="border-b border-slate-800 text-left text-[11px] tracking-wide text-slate-500 uppercase">
                <th className="px-5 py-2.5 font-medium">Node</th>
                <th className="px-5 py-2.5 font-medium">Address</th>
                <th className="px-5 py-2.5 font-medium">Version</th>
                <th className="px-5 py-2.5 font-medium">Status</th>
                <th className="px-5 py-2.5 font-medium">Last seen</th>
                <th className="px-5 py-2.5" />
              </tr>
            </thead>
            <tbody className="divide-y divide-slate-800/70">
              {helpers.map((helper) => (
                <tr
                  key={String(helper.id)}
                  className="transition-colors duration-150 hover:bg-slate-800/30"
                >
                  <td className="px-5 py-3 font-medium text-slate-100">{helper.node}</td>
                  <td className="px-5 py-3 font-mono text-xs whitespace-nowrap text-slate-400">
                    {helper.address}:{helper.port}
                  </td>
                  <td className="px-5 py-3 font-mono text-xs whitespace-nowrap text-slate-400">
                    {helper.version}
                  </td>
                  <td className="px-5 py-3">
                    <StatusPill tone={toneForStatus(helper.status)} label={helper.status} />
                  </td>
                  <td className="px-5 py-3 whitespace-nowrap">
                    <Num className="text-slate-400">{formatRelative(helper.lastSeen)}</Num>
                  </td>
                  <td className="px-5 py-3 text-right">
                    <IconButton
                      variant="dangerQuiet"
                      aria-label={`Remove helper for ${helper.node}`}
                      title="Remove helper"
                      loading={deleting === String(helper.id)}
                      onClick={() => void onDelete(helper)}
                    >
                      <Trash2 className="size-4" aria-hidden />
                    </IconButton>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : !token ? (
        <div className="px-5 py-5">
          <SectionNote>
            No node helpers yet — agentless VM backups will fail with an “install the node helper”
            error until each node runs one. Press Deploy helper, then paste the command into a root
            shell on every Proxmox node.
          </SectionNote>
        </div>
      ) : null}

      {token ? (
        <div className="space-y-4 border-t border-slate-800 px-5 py-5">
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
            caption="Downloads the helper, installs the systemd unit, and registers this node. Run on the node itself (needs vzdump and qmrestore)."
          />
        </div>
      ) : null}
    </Card>
  )
}
