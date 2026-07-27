/**
 * Restore wizard.
 *
 * Restoring is the most destructive thing this product does, so it is a short
 * wizard rather than one form with a button at the bottom:
 *
 *   1. Mode        — restore alongside (default) or overwrite the original.
 *   2. Destination — host, node, VMID, storage. The VMID is prefilled from the
 *                    server's free-VMID suggestion so nobody has to guess one.
 *   3. Review      — the source point, when it was last verified, the exact
 *                    destination, and how much will move.
 *
 * Overwrite is never preselected, is styled destructive throughout, and cannot
 * be started until the operator types the destination machine's current name.
 * Agent restores skip step 1: there is no VM to overwrite, so they always run
 * in `alongside` mode and the contract's confirmation does not apply.
 */

import { useCallback, useEffect, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import {
  AlertTriangle,
  Check,
  ChevronLeft,
  ChevronRight,
  CopyPlus,
  RefreshCw,
  RotateCcw,
  ShieldAlert,
} from 'lucide-react'
import { createRestore, errorMessage, getFreeVMID } from '../api'
import type { Agent, Backup, CachedVM, Host, RestoreMode, RestoreRequest } from '../api'
import { cn } from '../lib/cn'
import { formatBytes, formatDateTime, formatRelative } from '../lib/format'
import { identityText, WorkloadIdentity } from './Identity'
import { Modal } from './Modal'
import { useToast } from './Toast'
import {
  Button,
  DefinitionList,
  DefinitionRow,
  Field,
  Hint,
  Input,
  Num,
  Select,
  StatusPill,
} from './ui'

interface ModeOption {
  value: RestoreMode
  title: string
  description: string
  icon: typeof CopyPlus
}

const MODES: ModeOption[] = [
  {
    value: 'alongside',
    title: 'Restore alongside',
    description:
      'Creates a new machine on a free VMID. Nothing that exists today is touched. Recommended.',
    icon: CopyPlus,
  },
  {
    value: 'overwrite',
    title: 'Overwrite original',
    description:
      'Destroys the disks of the destination machine and replaces them with this restore point.',
    icon: ShieldAlert,
  },
]

function ModeTile({
  option,
  selected,
  onSelect,
}: {
  option: ModeOption
  selected: boolean
  onSelect: () => void
}) {
  const destructive = option.value === 'overwrite'
  return (
    <button
      type="button"
      role="radio"
      aria-checked={selected}
      onClick={onSelect}
      className={cn(
        'flex w-full items-start gap-3 rounded-xl border px-4 py-3.5 text-left transition-colors duration-150',
        selected
          ? destructive
            ? 'border-fail-500/60 bg-fail-500/10'
            : 'border-accent-500/50 bg-accent-500/10'
          : 'border-slate-800 bg-slate-950/40 hover:border-slate-700 hover:bg-slate-900/60',
      )}
    >
      <span
        className={cn(
          'mt-0.5 flex size-8 shrink-0 items-center justify-center rounded-lg border',
          selected
            ? destructive
              ? 'border-fail-500/50 bg-fail-500/15 text-fail-300'
              : 'border-accent-500/40 bg-accent-500/15 text-accent-300'
            : 'border-slate-800 bg-slate-900 text-slate-500',
        )}
      >
        <option.icon className="size-4" aria-hidden />
      </span>
      <span className="min-w-0">
        <span className="flex flex-wrap items-center gap-2">
          <span
            className={cn(
              'text-sm font-medium',
              selected ? (destructive ? 'text-fail-200' : 'text-slate-50') : 'text-slate-200',
            )}
          >
            {option.title}
          </span>
          {destructive ? (
            <StatusPill tone="fail" label="Destructive" />
          ) : (
            <StatusPill tone="neutral" label="Recommended" />
          )}
        </span>
        <span className="mt-0.5 block text-xs leading-relaxed text-slate-500">
          {option.description}
        </span>
      </span>
    </button>
  )
}

export function RestoreWizard({
  backup,
  hosts,
  agents,
  vms,
  onClose,
  onStarted,
}: {
  backup: Backup | null
  hosts: Host[]
  agents: Agent[]
  vms: CachedVM[]
  onClose: () => void
  onStarted: () => void
}) {
  const toast = useToast()
  const navigate = useNavigate()

  const isVM = backup?.sourceKind === 'vm'
  /* vzdump-based points (one `vma` stream, restored through the node helper)
     can be pointed at a specific Proxmox storage. */
  const isVma =
    isVM && backup !== null && backup.disks.length === 1 && backup.disks[0]?.name === 'vma'

  const steps = isVM ? (['Mode', 'Destination', 'Review'] as const) : (['Destination', 'Review'] as const)

  const [step, setStep] = useState(0)
  const [mode, setMode] = useState<RestoreMode>('alongside')
  const [hostId, setHostId] = useState('')
  const [node, setNode] = useState('')
  const [vmid, setVmid] = useState('')
  const [storage, setStorage] = useState('')
  const [agentId, setAgentId] = useState('')
  const [destPath, setDestPath] = useState('')
  const [confirmName, setConfirmName] = useState('')
  const [suggesting, setSuggesting] = useState(false)
  const [suggestError, setSuggestError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  /** The workload this restore point came from, resolved through the cache. */
  const source = useMemo(() => {
    if (!backup || backup.sourceKind !== 'vm') return null
    return (
      vms.find(
        (vm) =>
          String(vm.vmid) === String(backup.sourceId) &&
          (backup.hostId === undefined || String(vm.hostId) === String(backup.hostId)),
      ) ??
      vms.find((vm) => String(vm.vmid) === String(backup.sourceId)) ??
      null
    )
  }, [backup, vms])

  /* Reset on every open: a restore dialog must never inherit the previous
     restore's destination, and least of all its mode. */
  useEffect(() => {
    if (!backup) return
    setStep(0)
    setMode('alongside')
    setError(null)
    setSuggestError(null)
    setSubmitting(false)
    setConfirmName('')
    setStorage('')

    if (backup.sourceKind === 'vm') {
      const host =
        backup.hostId !== undefined && backup.hostId !== null
          ? String(backup.hostId)
          : String(source?.hostId ?? hosts[0]?.id ?? '')
      setHostId(host)
      setNode(backup.node ?? source?.node ?? '')
      setVmid('')
    } else {
      const match = agents.find((agent) => String(agent.id) === String(backup.sourceId))
      setAgentId(String(match?.id ?? agents[0]?.id ?? ''))
      setDestPath('')
    }
    // Only re-seed when a different restore point is opened.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [backup])

  /** The machine that would be destroyed, when overwriting. */
  const destinationVM = useMemo(() => {
    if (!isVM) return null
    const target = Number(vmid)
    if (!Number.isInteger(target)) return null
    return (
      vms.find((vm) => String(vm.hostId) === String(hostId) && vm.vmid === target) ?? null
    )
  }, [isVM, vms, hostId, vmid])

  const suggestVmid = useCallback(async () => {
    if (!hostId) return
    setSuggesting(true)
    setSuggestError(null)
    try {
      const free = await getFreeVMID(hostId)
      setVmid(String(free.vmid))
    } catch (err) {
      setSuggestError(errorMessage(err))
    } finally {
      setSuggesting(false)
    }
  }, [hostId])

  /* Alongside needs a VMID nothing is using, so the server suggests one.
     Overwrite deliberately targets the original instead. */
  useEffect(() => {
    if (!backup || !isVM) return
    if (mode === 'overwrite') {
      setVmid(String(source?.vmid ?? backup.sourceId ?? ''))
      setNode(backup.node ?? source?.node ?? '')
      return
    }
    setConfirmName('')
    void suggestVmid()
  }, [backup, isVM, mode, source, suggestVmid])

  const overwrite = isVM && mode === 'overwrite'
  const expectedName = destinationVM?.name ?? ''
  const confirmSatisfied =
    !overwrite || (confirmName.trim().length > 0 && (!expectedName || confirmName.trim() === expectedName))

  const destinationValid = isVM
    ? Boolean(hostId) && node.trim().length > 0 && Number.isInteger(Number(vmid)) && Number(vmid) > 0
    : Boolean(agentId) && destPath.trim().length > 0

  /* The confirmation lives on the Destination step, so that step is what it
     gates — an operator should not be able to walk past an unconfirmed
     overwrite and discover the block at the last screen. */
  const stepValid = (index: number): boolean => {
    const name = steps[index]
    if (name === 'Mode') return true
    return destinationValid && confirmSatisfied
  }

  const hostName = hosts.find((host) => String(host.id) === hostId)?.name ?? ''
  const agent = agents.find((item) => String(item.id) === agentId) ?? null

  const onSubmit = async () => {
    if (!backup) return
    setError(null)

    if (!destinationValid) {
      setError(
        isVM
          ? 'Choose a host, a node, and a numeric VMID for the restored machine.'
          : 'Choose an agent and a destination path.',
      )
      return
    }
    if (overwrite && !confirmSatisfied) {
      setError('Type the destination machine’s current name to confirm the overwrite.')
      return
    }

    const payload: RestoreRequest = isVM
      ? {
          backupId: backup.id,
          mode,
          vm: {
            hostId,
            node: node.trim(),
            vmid: Number(vmid),
            ...(isVma && storage.trim() ? { storage: storage.trim() } : {}),
          },
          ...(overwrite ? { confirmName: confirmName.trim() } : {}),
        }
      : {
          backupId: backup.id,
          mode: 'alongside',
          agent: { agentId, destPath: destPath.trim() },
        }

    setSubmitting(true)
    try {
      const result = await createRestore(payload)
      toast.success(
        `Restore of ${backup.sourceName} started.`,
        `Run ${String(result.runId)} — follow it on the Monitor page.`,
      )
      onStarted()
      onClose()
      navigate('/monitor')
    } catch (err) {
      const message = errorMessage(err)
      setError(message)
      toast.error('Could not start restore', message)
    } finally {
      setSubmitting(false)
    }
  }

  const isLast = step === steps.length - 1
  const currentStep = steps[step]

  return (
    <Modal
      open={backup !== null}
      onClose={onClose}
      width="lg"
      title="Restore"
      subtitle={
        backup
          ? `${identityText({
              hostName: backup.hostName,
              name: backup.sourceName,
              vmid: backup.sourceKind === 'vm' ? backup.sourceId : null,
              node: backup.node,
            })} — ${backup.kind} point from ${formatDateTime(backup.createdAt)}`
          : undefined
      }
      footer={
        <>
          <Button
            onClick={() => setStep((current) => Math.max(0, current - 1))}
            disabled={step === 0 || submitting}
            icon={<ChevronLeft className="size-4" aria-hidden />}
          >
            Back
          </Button>
          <div className="flex-1" />
          <Button onClick={onClose} disabled={submitting}>
            Cancel
          </Button>
          {isLast ? (
            <Button
              variant={overwrite ? 'danger' : 'primary'}
              loading={submitting}
              disabled={!stepValid(step)}
              onClick={() => void onSubmit()}
              icon={<RotateCcw className="size-4" aria-hidden />}
            >
              {overwrite ? 'Overwrite and restore' : 'Start restore'}
            </Button>
          ) : (
            <Button
              variant="primary"
              disabled={!stepValid(step)}
              onClick={() => setStep((current) => Math.min(steps.length - 1, current + 1))}
              icon={<ChevronRight className="size-4" aria-hidden />}
            >
              Continue
            </Button>
          )}
        </>
      }
    >
      {!backup ? null : (
        <div className="space-y-5">
          <ol className="flex flex-wrap items-center gap-x-2 gap-y-2 text-xs">
            {steps.map((label, index) => (
              <li key={label} className="flex items-center gap-2">
                <span
                  className={cn(
                    'flex size-5 items-center justify-center rounded-full border text-[10px] font-semibold',
                    index < step
                      ? 'border-accent-500/40 bg-accent-500/15 text-accent-300'
                      : index === step
                        ? 'border-accent-500/50 bg-accent-500/15 text-accent-300'
                        : 'border-slate-700 bg-slate-900 text-slate-500',
                  )}
                >
                  {index < step ? <Check className="size-3" aria-hidden /> : index + 1}
                </span>
                <span className={cn(index === step ? 'font-medium text-slate-200' : 'text-slate-500')}>
                  {label}
                </span>
                {index < steps.length - 1 ? (
                  <ChevronRight className="size-3.5 text-slate-700" aria-hidden />
                ) : null}
              </li>
            ))}
          </ol>

          {/* Step — mode */}
          {currentStep === 'Mode' ? (
            <div className="space-y-3" role="radiogroup" aria-label="Restore mode">
              {MODES.map((option) => (
                <ModeTile
                  key={option.value}
                  option={option}
                  selected={mode === option.value}
                  onSelect={() => setMode(option.value)}
                />
              ))}
            </div>
          ) : null}

          {/* Step — destination */}
          {currentStep === 'Destination' && isVM ? (
            <div className="space-y-4">
              <Field label="Cluster">
                {({ id }) => (
                  <Select id={id} value={hostId} onChange={(event) => setHostId(event.target.value)}>
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
                <Field label="Node">
                  {({ id }) => (
                    <Input
                      id={id}
                      value={node}
                      placeholder="pve1"
                      onChange={(event) => setNode(event.target.value)}
                    />
                  )}
                </Field>

                <Field
                  label="VMID"
                  error={
                    overwrite || !suggestError
                      ? undefined
                      : `Could not ask the server for a free VMID: ${suggestError}`
                  }
                >
                  {({ id }) => (
                    <div className="flex gap-2">
                      <Input
                        id={id}
                        type="number"
                        min={100}
                        value={vmid}
                        onChange={(event) => setVmid(event.target.value)}
                      />
                      {overwrite ? null : (
                        <Button
                          aria-label="Suggest a free VMID"
                          title="Ask the server for the next free VMID"
                          loading={suggesting}
                          onClick={() => void suggestVmid()}
                          icon={<RefreshCw className="size-3.5" aria-hidden />}
                        />
                      )}
                    </div>
                  )}
                </Field>
              </div>

              {overwrite ? (
                <Hint>
                  Overwriting targets VMID {vmid || '—'} on {node || 'the selected node'}.
                </Hint>
              ) : (
                <Hint>
                  Prefilled with the next free VMID on this cluster, so the restored machine cannot
                  collide with a running one.
                </Hint>
              )}

              {isVma ? (
                <Field label="Target storage (optional)">
                  {({ id }) => (
                    <Input
                      id={id}
                      value={storage}
                      placeholder="local-lvm"
                      onChange={(event) => setStorage(event.target.value)}
                    />
                  )}
                </Field>
              ) : null}

              {overwrite ? (
                <div className="space-y-3 rounded-xl border border-fail-500/40 bg-fail-500/[0.07] px-4 py-3.5">
                  <p className="flex items-center gap-2 text-[13px] font-medium text-fail-200">
                    <AlertTriangle className="size-4 shrink-0" aria-hidden />
                    This destroys the destination machine’s disks
                  </p>
                  {destinationVM ? (
                    <p className="text-xs leading-relaxed text-slate-300">
                      VMID <Num>{destinationVM.vmid}</Num> on {hostName || 'this cluster'} is
                      currently <span className="font-medium text-slate-100">{destinationVM.name}</span>
                      . Type that name to confirm.
                    </p>
                  ) : (
                    <p className="text-xs leading-relaxed text-slate-300">
                      This VMID is not in the cached inventory. Type the destination machine’s
                      current name exactly — the server refuses the restore if it does not match.
                    </p>
                  )}
                  <Field label="Destination machine name">
                    {({ id }) => (
                      <Input
                        id={id}
                        value={confirmName}
                        placeholder={destinationVM?.name ?? 'current name'}
                        autoComplete="off"
                        aria-invalid={confirmName.length > 0 && !confirmSatisfied}
                        onChange={(event) => setConfirmName(event.target.value)}
                      />
                    )}
                  </Field>
                </div>
              ) : null}
            </div>
          ) : null}

          {currentStep === 'Destination' && !isVM ? (
            <div className="space-y-4">
              <Field label="Restore to agent">
                {({ id }) => (
                  <Select
                    id={id}
                    value={agentId}
                    onChange={(event) => setAgentId(event.target.value)}
                  >
                    <option value="">Select an agent…</option>
                    {agents.map((item) => (
                      <option key={String(item.id)} value={String(item.id)}>
                        {item.hostname} — {item.os}/{item.arch} ({item.status})
                      </option>
                    ))}
                  </Select>
                )}
              </Field>

              <Field label="Destination path">
                {({ id }) => (
                  <Input
                    id={id}
                    value={destPath}
                    placeholder="/restore/2026-07-27  or  C:\restore"
                    onChange={(event) => setDestPath(event.target.value)}
                  />
                )}
              </Field>
              <Hint>
                Files are unpacked into this directory. Anything already there under the same name
                is replaced, so restoring into a fresh directory is the safe choice.
              </Hint>
            </div>
          ) : null}

          {/* Step — review */}
          {currentStep === 'Review' ? (
            <div className="space-y-4">
              <DefinitionList>
                <DefinitionRow label="Restore point">
                  <span className="block">
                    {formatDateTime(backup.createdAt)} · {backup.kind}
                  </span>
                  <span className="mt-0.5 block text-meta text-slate-500">
                    {formatRelative(backup.createdAt)}
                  </span>
                </DefinitionRow>

                <DefinitionRow label="Source">
                  <WorkloadIdentity
                    inline
                    workload={{
                      hostName: backup.hostName,
                      name: backup.sourceName,
                      vmid: backup.sourceKind === 'vm' ? backup.sourceId : null,
                      node: backup.node,
                    }}
                  />
                </DefinitionRow>

                <DefinitionRow
                  label="Last verification"
                  tone={backup.lastVerifyResult === 'failed' ? 'warn' : 'normal'}
                >
                  {backup.lastVerifiedAt ? (
                    <span className="inline-flex flex-wrap items-center gap-2">
                      <StatusPill
                        tone={backup.lastVerifyResult === 'failed' ? 'fail' : 'ok'}
                        label={
                          backup.lastVerifyResult === 'failed'
                            ? 'Integrity check failed'
                            : 'Integrity verified'
                        }
                      />
                      <span className="text-meta text-slate-500">
                        {formatRelative(backup.lastVerifiedAt)}
                      </span>
                    </span>
                  ) : (
                    <span className="text-warn-300">
                      Never verified — this point has not been read back from the target.
                    </span>
                  )}
                </DefinitionRow>

                <DefinitionRow label="Mode" tone={overwrite ? 'warn' : 'normal'}>
                  {overwrite
                    ? 'Overwrite original — the destination machine’s disks are destroyed'
                    : 'Restore alongside — nothing existing is touched'}
                </DefinitionRow>

                <DefinitionRow label="Destination">
                  {isVM ? (
                    <>
                      <span className="block">
                        {hostName || '—'} / VMID <Num>{vmid || '—'}</Num> / {node || '—'}
                      </span>
                      {storage.trim() ? (
                        <span className="mt-0.5 block text-meta text-slate-500">
                          storage {storage.trim()}
                        </span>
                      ) : null}
                    </>
                  ) : (
                    <>
                      <span className="block">{agent?.hostname ?? '—'}</span>
                      <span className="mt-0.5 block font-mono text-meta text-slate-500">
                        {destPath || '—'}
                      </span>
                    </>
                  )}
                </DefinitionRow>

                <DefinitionRow label="Estimated transfer">
                  <Num>{formatBytes(backup.sizeBytes)}</Num>
                  <span className="ml-2 text-meta text-slate-500">
                    full size of this point, read back from the target
                  </span>
                </DefinitionRow>
              </DefinitionList>

              {overwrite ? (
                <div className="rounded-xl border border-fail-500/40 bg-fail-500/[0.07] px-4 py-3.5">
                  <p className="flex items-center gap-2 text-[13px] font-medium text-fail-200">
                    <AlertTriangle className="size-4 shrink-0" aria-hidden />
                    {confirmSatisfied
                      ? `Confirmed: ${confirmName.trim()} will be overwritten.`
                      : 'Type the destination machine’s current name on the previous step to continue.'}
                  </p>
                </div>
              ) : null}

              {error ? (
                <p className="rounded-lg border border-fail-500/30 bg-fail-500/10 px-3.5 py-2.5 text-xs text-fail-300">
                  {error}
                </p>
              ) : null}
            </div>
          ) : null}
        </div>
      )}
    </Modal>
  )
}
