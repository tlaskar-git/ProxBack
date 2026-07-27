import { useEffect, useState } from 'react'
import type { FormEvent } from 'react'
import { Fingerprint, ServerCog, ShieldQuestion, Terminal } from 'lucide-react'
import { deployFingerprintOf, deployHelper, errorMessage } from '../api'
import type { Host } from '../api'
import { Modal } from './Modal'
import { useToast } from './Toast'
import { Button, Field, Input, SectionNote, Select } from './ui'

/**
 * Installs a node helper over SSH from the ProxBack server, so the operator
 * never has to open a shell on the Proxmox node.
 *
 * Credentials are used for the single deployment connection and are never
 * stored. The first attempt deliberately fails with the node's SSH host-key
 * fingerprint, which the operator confirms before anything is executed.
 */
export function DeployHelperModal({
  open,
  onClose,
  onDeployed,
  hosts,
  nodesByHost,
  defaultAddress,
}: {
  open: boolean
  onClose: () => void
  onDeployed: () => void
  /** Clusters this helper could belong to. A helper is keyed by (host, node). */
  hosts: Host[]
  /** Node names discovered from the inventory, per host id. */
  nodesByHost: Record<string, string[]>
  /** Best-guess address (the Proxmox host's own address) to prefill. */
  defaultAddress?: string
}) {
  const toast = useToast()
  const [hostId, setHostId] = useState<string>('')
  const [node, setNode] = useState('')
  const [address, setAddress] = useState('')
  const [port, setPort] = useState('22')
  const [username, setUsername] = useState('root')
  const [password, setPassword] = useState('')
  const [helperPort, setHelperPort] = useState('8007')
  const [fingerprint, setFingerprint] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [log, setLog] = useState<string[]>([])

  useEffect(() => {
    if (!open) return
    const firstHost = String(hosts[0]?.id ?? '')
    setHostId(firstHost)
    setNode(nodesByHost[firstHost]?.[0] ?? '')
    setAddress(defaultAddress ?? '')
    setPort('22')
    setUsername('root')
    setPassword('')
    setHelperPort('8007')
    setFingerprint(null)
    setError(null)
    setLog([])
    setSubmitting(false)
  }, [open, hosts, nodesByHost, defaultAddress])

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    setError(null)

    if (!hostId) return setError('Choose which cluster this node belongs to.')
    if (!node.trim()) return setError('Choose which Proxmox node to deploy to.')
    if (!address.trim()) return setError("Enter the node's SSH address.")
    if (!username.trim()) return setError('Enter the SSH username (usually root).')
    if (!password) return setError('Enter the SSH password.')

    setSubmitting(true)
    try {
      const result = await deployHelper({
        hostId,
        node: node.trim(),
        address: address.trim(),
        port: Number(port) || 22,
        username: username.trim(),
        password,
        serverUrl: window.location.origin,
        helperPort: Number(helperPort) || 8007,
        ...(fingerprint ? { hostKeyFingerprint: fingerprint } : {}),
      })
      setLog(result.log ?? [])
      setPassword('')
      toast.success(
        `Node helper installed on ${node}.`,
        result.helperOnline
          ? 'It has registered and is online.'
          : 'It should register within a few seconds.',
      )
      onDeployed()
      onClose()
    } catch (err) {
      const fp = deployFingerprintOf(err)
      if (fp) {
        // First contact: show the fingerprint and let the operator confirm.
        setFingerprint(fp)
        setError(null)
      } else {
        setError(errorMessage(err))
      }
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Modal
      open={open}
      onClose={onClose}
      width="lg"
      title="Deploy node helper"
      subtitle="ProxBack installs the helper on the Proxmox node over SSH — no shell commands to paste."
      footer={
        <>
          <Button onClick={onClose} disabled={submitting}>
            Cancel
          </Button>
          <Button
            variant="primary"
            form="deploy-helper-form"
            type="submit"
            loading={submitting}
            icon={<ServerCog className="size-4" aria-hidden />}
          >
            {fingerprint ? 'Trust and install' : 'Install helper'}
          </Button>
        </>
      }
    >
      <form id="deploy-helper-form" className="space-y-4" onSubmit={(e) => void submit(e)} noValidate>
        {/* Cluster first: a helper is identified by (cluster, node), because
            two clusters can each contain a node called pve1. */}
        <Field label="Cluster">
          {({ id }) => (
            <Select
              id={id}
              value={hostId}
              onChange={(event) => {
                const next = event.target.value
                setHostId(next)
                setNode(nodesByHost[next]?.[0] ?? '')
              }}
            >
              <option value="">Select a host…</option>
              {hosts.map((host) => (
                <option key={String(host.id)} value={String(host.id)}>
                  {host.name}
                </option>
              ))}
            </Select>
          )}
        </Field>

        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Proxmox node">
            {({ id }) =>
              (nodesByHost[hostId] ?? []).length > 0 ? (
                <Select id={id} value={node} onChange={(event) => setNode(event.target.value)}>
                  {(nodesByHost[hostId] ?? []).map((name) => (
                    <option key={name} value={name}>
                      {name}
                    </option>
                  ))}
                </Select>
              ) : (
                <Input
                  id={id}
                  value={node}
                  placeholder="pve1"
                  onChange={(event) => setNode(event.target.value)}
                />
              )
            }
          </Field>
          <Field label="SSH address" hint="Hostname or IP of that node.">
            {({ id }) => (
              <Input
                id={id}
                value={address}
                placeholder="10.0.0.5"
                onChange={(event) => setAddress(event.target.value)}
              />
            )}
          </Field>
        </div>

        <div className="grid gap-4 sm:grid-cols-3">
          <Field label="SSH port">
            {({ id }) => (
              <Input
                id={id}
                type="number"
                value={port}
                onChange={(event) => setPort(event.target.value)}
              />
            )}
          </Field>
          <Field label="Username">
            {({ id }) => (
              <Input
                id={id}
                value={username}
                autoComplete="off"
                onChange={(event) => setUsername(event.target.value)}
              />
            )}
          </Field>
          <Field label="Helper port" hint="Port the helper listens on.">
            {({ id }) => (
              <Input
                id={id}
                type="number"
                value={helperPort}
                onChange={(event) => setHelperPort(event.target.value)}
              />
            )}
          </Field>
        </div>

        <Field label="Password" hint="Used for this deployment only — never stored.">
          {({ id }) => (
            <Input
              id={id}
              type="password"
              value={password}
              autoComplete="new-password"
              onChange={(event) => setPassword(event.target.value)}
            />
          )}
        </Field>

        {fingerprint ? (
          <div className="space-y-2 rounded-lg border border-warn-500/30 bg-warn-500/10 px-4 py-3">
            <p className="flex items-center gap-2 text-sm font-medium text-warn-200">
              <ShieldQuestion className="size-4" aria-hidden />
              Confirm this node’s SSH host key
            </p>
            <p className="flex items-center gap-2 font-mono text-xs break-all text-warn-100">
              <Fingerprint className="size-3.5 shrink-0" aria-hidden />
              {fingerprint}
            </p>
            <p className="text-xs text-warn-300/90">
              Compare it with the node’s own key (run{' '}
              <code className="font-mono">ssh-keyscan -t ed25519 {address || 'node'}</code> or check
              the Proxmox console), then press Trust and install. Nothing has been executed yet.
            </p>
          </div>
        ) : (
          <SectionNote>
            ProxBack connects to the node, uploads the helper binary to{' '}
            <code className="font-mono">/usr/local/bin/proxback-helper</code>, installs its systemd
            unit, and enrolls it automatically. The node needs{' '}
            <code className="font-mono">vzdump</code> and <code className="font-mono">qmrestore</code>{' '}
            — which every Proxmox node has.
          </SectionNote>
        )}

        {error ? (
          <p className="rounded-lg border border-fail-500/30 bg-fail-500/10 px-3.5 py-2.5 text-xs text-fail-300">
            {error}
          </p>
        ) : null}

        {log.length > 0 ? (
          <div className="space-y-1 rounded-lg border border-slate-800 bg-slate-950/60 px-4 py-3">
            <p className="eyebrow flex items-center gap-2">
              <Terminal className="size-3.5" aria-hidden />
              Deployment log
            </p>
            {log.map((line, i) => (
              <p key={i} className="font-mono text-xs break-all text-slate-400">
                {line}
              </p>
            ))}
          </div>
        ) : null}
      </form>
    </Modal>
  )
}
