import { useEffect, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import {
  Check,
  ChevronLeft,
  ChevronRight,
  Database,
  FolderPlus,
  Info,
  Laptop,
  Search,
  ShieldCheck,
  X,
} from 'lucide-react'
import {
  agentSourceOf,
  allTagsOf,
  createJob,
  errorMessage,
  patchJob,
  tagsOf,
  vmSourcesOf,
  vmsWithTag,
} from '../api'
import type { Agent, CachedVM, ID, Job, JobCreate, JobKind, JobSources, Target } from '../api'
import { cn } from '../lib/cn'
import { describeSchedule, formatBytes, formatCount, isValidCron } from '../lib/format'
import { Modal } from './Modal'
import { useToast } from './Toast'
import {
  Button,
  Chip,
  ChipButton,
  Field,
  IconButton,
  Input,
  Num,
  SectionNote,
  Segmented,
  StatusPill,
  Toggle,
  toneForStatus,
} from './ui'

type ScheduleMode = 'manual' | 'hourly' | 'daily' | 'weekly' | 'custom'

const CRON_PRESETS: Record<Exclude<ScheduleMode, 'manual' | 'custom'>, string> = {
  hourly: '0 * * * *',
  daily: '0 2 * * *',
  weekly: '0 3 * * 0',
}

const STEPS = ['Sources', 'Target', 'Schedule', 'Retention', 'Review'] as const

/** How a VM job picks its members: a fixed list, or every VM carrying a tag. */
type VMSourceMode = 'manual' | 'tag'

const SOURCE_MODES: { value: VMSourceMode; label: string }[] = [
  { value: 'manual', label: 'Pick VMs manually' },
  { value: 'tag', label: 'By Proxmox tag' },
]

interface WizardState {
  name: string
  kind: JobKind
  sourceMode: VMSourceMode
  selectedVMs: CachedVM[]
  tagFilter: string
  agentId: ID | ''
  paths: string[]
  targetId: ID | ''
  scheduleMode: ScheduleMode
  cron: string
  retention: number
  enabled: boolean
}

function vmKey(hostId: ID, vmid: number): string {
  return `${String(hostId)}:${vmid}`
}

function scheduleModeOf(schedule: string): ScheduleMode {
  if (!schedule || schedule === 'manual') return 'manual'
  const entry = Object.entries(CRON_PRESETS).find(([, cron]) => cron === schedule.trim())
  if (entry) return entry[0] as ScheduleMode
  return 'custom'
}

function initialState(
  editJob: Job | null | undefined,
  initialVM: CachedVM | null | undefined,
  vms: CachedVM[],
): WizardState {
  if (editJob) {
    const agentSource = agentSourceOf(editJob)
    const wanted = new Set(vmSourcesOf(editJob).map((source) => vmKey(source.hostId, source.vmid)))
    const tagFilter = editJob.tagFilter ?? ''
    return {
      name: editJob.name,
      kind: editJob.kind,
      sourceMode: tagFilter ? 'tag' : 'manual',
      selectedVMs: vms.filter((vm) => wanted.has(vmKey(vm.hostId, vm.vmid))),
      tagFilter,
      agentId: agentSource?.agentId ?? '',
      paths: agentSource?.paths ?? [],
      targetId: editJob.targetId,
      scheduleMode: scheduleModeOf(editJob.schedule),
      cron: scheduleModeOf(editJob.schedule) === 'custom' ? editJob.schedule : '',
      retention: editJob.retention,
      enabled: editJob.enabled,
    }
  }

  return {
    name: initialVM ? `Backup — ${initialVM.name}` : '',
    kind: 'vm',
    sourceMode: 'manual',
    selectedVMs: initialVM ? [initialVM] : [],
    tagFilter: '',
    agentId: '',
    paths: [],
    targetId: '',
    scheduleMode: 'daily',
    cron: '',
    retention: 7,
    enabled: true,
  }
}

function StepRail({ step }: { step: number }) {
  return (
    <ol className="mb-6 flex flex-wrap items-center gap-x-2 gap-y-2 text-xs">
      {STEPS.map((label, index) => {
        const done = index < step
        const active = index === step
        return (
          <li key={label} className="flex items-center gap-2">
            <span
              className={cn(
                'flex size-5 items-center justify-center rounded-full border text-[10px] font-semibold',
                done
                  ? 'border-emerald-500/40 bg-emerald-500/15 text-emerald-300'
                  : active
                    ? 'border-accent-500/50 bg-accent-500/15 text-accent-300'
                    : 'border-slate-700 bg-slate-900 text-slate-500',
              )}
            >
              {done ? <Check className="size-3" aria-hidden /> : index + 1}
            </span>
            <span className={cn(active ? 'font-medium text-slate-200' : 'text-slate-500')}>
              {label}
            </span>
            {index < STEPS.length - 1 ? (
              <ChevronRight className="size-3.5 text-slate-700" aria-hidden />
            ) : null}
          </li>
        )
      })}
    </ol>
  )
}

function SelectTile({
  selected,
  onClick,
  title,
  description,
  icon,
  disabled,
}: {
  selected: boolean
  onClick: () => void
  title: string
  description: string
  icon: ReactNode
  disabled?: boolean
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      aria-pressed={selected}
      className={cn(
        'flex w-full items-start gap-3 rounded-xl border px-4 py-3.5 text-left transition-colors duration-150',
        selected
          ? 'border-accent-500/50 bg-accent-500/10'
          : 'border-slate-800 bg-slate-950/40 hover:border-slate-700 hover:bg-slate-900/60',
        disabled && 'cursor-not-allowed opacity-50',
      )}
    >
      <span
        className={cn(
          'mt-0.5 flex size-8 shrink-0 items-center justify-center rounded-lg border',
          selected
            ? 'border-accent-500/40 bg-accent-500/15 text-accent-300'
            : 'border-slate-800 bg-slate-900 text-slate-500',
        )}
      >
        {icon}
      </span>
      <span className="min-w-0">
        <span className={cn('block text-sm font-medium', selected ? 'text-white' : 'text-slate-200')}>
          {title}
        </span>
        <span className="mt-0.5 block text-xs leading-relaxed text-slate-500">{description}</span>
      </span>
    </button>
  )
}

export function JobWizard({
  open,
  onClose,
  onSaved,
  targets,
  vms,
  agents,
  initialVM,
  editJob,
}: {
  open: boolean
  onClose: () => void
  onSaved: () => void
  targets: Target[]
  vms: CachedVM[]
  agents: Agent[]
  initialVM?: CachedVM | null
  editJob?: Job | null
}) {
  const toast = useToast()
  const [step, setStep] = useState(0)
  const [state, setState] = useState<WizardState>(() => initialState(editJob, initialVM, vms))
  const [pathDraft, setPathDraft] = useState('')
  const [query, setQuery] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Reset every time the wizard is opened so it never shows stale selections.
  useEffect(() => {
    if (!open) return
    setStep(0)
    setState(initialState(editJob, initialVM, vms))
    setPathDraft('')
    setQuery('')
    setError(null)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open])

  const patch = (next: Partial<WizardState>) => setState((current) => ({ ...current, ...next }))

  const schedule =
    state.scheduleMode === 'manual'
      ? 'manual'
      : state.scheduleMode === 'custom'
        ? state.cron.trim()
        : CRON_PRESETS[state.scheduleMode]

  const selectedKeys = useMemo(
    () => new Set(state.selectedVMs.map((vm) => vmKey(vm.hostId, vm.vmid))),
    [state.selectedVMs],
  )

  const filteredVMs = useMemo(() => {
    const needle = query.trim().toLowerCase()
    if (!needle) return vms
    return vms.filter(
      (vm) =>
        vm.name.toLowerCase().includes(needle) ||
        String(vm.vmid).includes(needle) ||
        vm.node.toLowerCase().includes(needle) ||
        vm.hostName.toLowerCase().includes(needle),
    )
  }, [vms, query])

  const selectedTarget = targets.find((target) => String(target.id) === String(state.targetId))
  const selectedAgent = agents.find((agent) => String(agent.id) === String(state.agentId))

  const availableTags = useMemo(() => allTagsOf(vms), [vms])
  const tagMatches = useMemo(
    () => (state.tagFilter ? vmsWithTag(vms, state.tagFilter) : []),
    [vms, state.tagFilter],
  )
  const byTag = state.kind === 'vm' && state.sourceMode === 'tag'

  const stepValid = (index: number): boolean => {
    switch (index) {
      case 0:
        if (!state.name.trim()) return false
        if (state.kind === 'agent') return state.agentId !== '' && state.paths.length > 0
        return byTag ? state.tagFilter !== '' : state.selectedVMs.length > 0
      case 1:
        return state.targetId !== ''
      case 2:
        if (state.scheduleMode === 'custom') return isValidCron(state.cron)
        return true
      case 3:
        return Number.isFinite(state.retention) && state.retention >= 1
      default:
        return true
    }
  }

  const toggleVM = (vm: CachedVM) => {
    const key = vmKey(vm.hostId, vm.vmid)
    setState((current) => {
      const exists = current.selectedVMs.some((item) => vmKey(item.hostId, item.vmid) === key)
      return {
        ...current,
        selectedVMs: exists
          ? current.selectedVMs.filter((item) => vmKey(item.hostId, item.vmid) !== key)
          : [...current.selectedVMs, vm],
      }
    })
  }

  const addPath = () => {
    const value = pathDraft.trim()
    if (!value) return
    if (state.paths.includes(value)) {
      setPathDraft('')
      return
    }
    patch({ paths: [...state.paths, value] })
    setPathDraft('')
  }

  const onSubmit = async () => {
    setError(null)
    const sources: JobSources =
      state.kind === 'vm'
        ? byTag
          ? // Tag-filtered jobs resolve their members at run start.
            []
          : state.selectedVMs.map((vm) => ({ hostId: vm.hostId, vmid: vm.vmid, name: vm.name }))
        : { agentId: state.agentId as ID, paths: state.paths }

    const payload: JobCreate = {
      name: state.name.trim(),
      kind: state.kind,
      targetId: state.targetId as ID,
      schedule,
      retention: state.retention,
      sources,
      enabled: state.enabled,
      // Only VM jobs carry a tag filter; "" clears one that was set before.
      ...(state.kind === 'vm' ? { tagFilter: byTag ? state.tagFilter : '' } : {}),
    }

    setSubmitting(true)
    try {
      if (editJob) {
        await patchJob(editJob.id, payload)
        toast.success(`Job “${payload.name}” updated.`)
      } else {
        await createJob(payload)
        toast.success(
          `Job “${payload.name}” created.`,
          schedule === 'manual' ? 'Run it from the Backup Jobs page.' : describeSchedule(schedule),
        )
      }
      onSaved()
      onClose()
    } catch (err) {
      const message = errorMessage(err)
      setError(message)
      toast.error(editJob ? 'Could not update job' : 'Could not create job', message)
    } finally {
      setSubmitting(false)
    }
  }

  const isLast = step === STEPS.length - 1

  return (
    <Modal
      open={open}
      onClose={onClose}
      width="xl"
      title={editJob ? `Edit job — ${editJob.name}` : 'Create backup job'}
      subtitle="Pick what to protect, where it goes, and how often it runs."
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
              variant="primary"
              loading={submitting}
              onClick={() => void onSubmit()}
              icon={<Check className="size-4" aria-hidden />}
            >
              {editJob ? 'Save changes' : 'Create job'}
            </Button>
          ) : (
            <Button
              variant="primary"
              disabled={!stepValid(step)}
              onClick={() => setStep((current) => Math.min(STEPS.length - 1, current + 1))}
              icon={<ChevronRight className="size-4" aria-hidden />}
            >
              Continue
            </Button>
          )}
        </>
      }
    >
      <StepRail step={step} />

      {/* Step 1 — kind + sources */}
      {step === 0 ? (
        <div className="space-y-5">
          <Field label="Job name">
            {({ id }) => (
              <Input
                id={id}
                value={state.name}
                placeholder="Nightly — production VMs"
                onChange={(event) => patch({ name: event.target.value })}
              />
            )}
          </Field>

          <div className="grid gap-3 sm:grid-cols-2">
            <SelectTile
              selected={state.kind === 'vm'}
              onClick={() => patch({ kind: 'vm' })}
              disabled={Boolean(editJob)}
              icon={<Laptop className="size-4" aria-hidden />}
              title="Virtual machines"
              description="Agentless image backup through the Proxmox API — snapshot, export each disk, done."
            />
            <SelectTile
              selected={state.kind === 'agent'}
              onClick={() => patch({ kind: 'agent' })}
              disabled={Boolean(editJob)}
              icon={<ShieldCheck className="size-4" aria-hidden />}
              title="Agent — file level"
              description="An in-guest agent tars the paths you choose and streams them through the server."
            />
          </div>

          {editJob ? (
            <SectionNote>The job kind cannot be changed after creation.</SectionNote>
          ) : null}

          {state.kind === 'vm' ? (
            <div className="space-y-3">
              <Segmented
                label="How this job picks virtual machines"
                value={state.sourceMode}
                options={SOURCE_MODES}
                onChange={(mode) => patch({ sourceMode: mode })}
              />

              {byTag ? (
                <div className="space-y-3">
                  {availableTags.length === 0 ? (
                    <p className="rounded-lg border border-dashed border-slate-800 bg-slate-950/40 px-4 py-8 text-center text-xs text-slate-500">
                      No Proxmox tags in the inventory yet. Set the Tags field on a guest in Proxmox,
                      refresh the Virtual Machines page, then come back.
                    </p>
                  ) : (
                    <>
                      <p className="text-xs font-medium text-slate-400">
                        Pick one tag
                        {state.tagFilter ? (
                          <span className="ml-2 text-slate-600">{state.tagFilter}</span>
                        ) : null}
                      </p>
                      <div className="flex flex-wrap gap-1.5">
                        {availableTags.map((tag) => (
                          <ChipButton
                            key={tag}
                            selected={state.tagFilter === tag}
                            onClick={() =>
                              patch({ tagFilter: state.tagFilter === tag ? '' : tag })
                            }
                          >
                            {tag}
                            <span className="ml-1 text-slate-600">
                              {vmsWithTag(vms, tag).length}
                            </span>
                          </ChipButton>
                        ))}
                      </div>
                    </>
                  )}

                  <SectionNote>
                    Membership is dynamic. Every VM carrying this tag when the job runs is included
                    automatically — guests you tag later join on their own, and a guest drops out as
                    soon as the tag comes off. The job never needs editing.
                  </SectionNote>

                  {state.tagFilter ? (
                    <div className="rounded-lg border border-slate-800 bg-slate-950/40 p-2">
                      <p className="px-1 pb-1.5 text-xs text-slate-500">
                        <Num>{formatCount(tagMatches.length)}</Num>{' '}
                        {tagMatches.length === 1 ? 'VM carries' : 'VMs carry'} this tag right now
                      </p>
                      <ul className="max-h-44 space-y-0.5 overflow-y-auto">
                        {tagMatches.map((vm) => (
                          <li
                            key={vmKey(vm.hostId, vm.vmid)}
                            className="flex items-center gap-2 px-1 py-1 text-xs"
                          >
                            <Laptop className="size-3.5 shrink-0 text-slate-600" aria-hidden />
                            <span className="truncate text-slate-300">{vm.name}</span>
                            <Num className="shrink-0 text-slate-500">#{vm.vmid}</Num>
                            <span className="ml-auto flex shrink-0 items-center gap-1">
                              {tagsOf(vm)
                                .filter((tag) => tag !== state.tagFilter)
                                .slice(0, 3)
                                .map((tag) => (
                                  <Chip key={tag}>{tag}</Chip>
                                ))}
                            </span>
                          </li>
                        ))}
                        {tagMatches.length === 0 ? (
                          <li className="px-1 py-2 text-xs text-amber-400/80">
                            Nothing carries this tag yet — a run that resolves to zero VMs fails.
                          </li>
                        ) : null}
                      </ul>
                    </div>
                  ) : null}
                </div>
              ) : (
                <>
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <p className="text-xs font-medium text-slate-400">
                      Select virtual machines
                      <span className="ml-2 text-slate-600">
                        {state.selectedVMs.length} selected
                      </span>
                    </p>
                    <div className="relative">
                      <Search
                        className="pointer-events-none absolute top-2.5 left-2.5 size-3.5 text-slate-600"
                        aria-hidden
                      />
                      <Input
                        value={query}
                        placeholder="Filter by name, VMID, or node"
                        className="w-64 py-1.5 pl-8 text-xs"
                        onChange={(event) => setQuery(event.target.value)}
                      />
                    </div>
                  </div>

                  {vms.length === 0 ? (
                    <p className="rounded-lg border border-dashed border-slate-800 bg-slate-950/40 px-4 py-8 text-center text-xs text-slate-500">
                      No virtual machines in the inventory yet. Add a Proxmox host, then refresh the
                      Virtual Machines page.
                    </p>
                  ) : (
                    <div className="max-h-72 space-y-2 overflow-y-auto rounded-lg border border-slate-800 bg-slate-950/40 p-2">
                      {filteredVMs.map((vm) => {
                        const key = vmKey(vm.hostId, vm.vmid)
                        const selected = selectedKeys.has(key)
                        return (
                          <button
                            type="button"
                            key={key}
                            onClick={() => toggleVM(vm)}
                            aria-pressed={selected}
                            className={cn(
                              'flex w-full items-center gap-3 rounded-lg border px-3 py-2.5 text-left transition-colors duration-150',
                              selected
                                ? 'border-accent-500/50 bg-accent-500/10'
                                : 'border-transparent hover:bg-slate-800/50',
                            )}
                          >
                            <span
                              className={cn(
                                'flex size-4 shrink-0 items-center justify-center rounded border',
                                selected
                                  ? 'border-accent-400 bg-accent-500 text-white'
                                  : 'border-slate-600',
                              )}
                            >
                              {selected ? <Check className="size-3" aria-hidden /> : null}
                            </span>
                            <span className="min-w-0 flex-1">
                              <span className="block truncate text-sm text-slate-200">
                                {vm.name}
                                <span className="ml-2 font-mono text-xs text-slate-500">
                                  #{vm.vmid}
                                </span>
                              </span>
                              <span className="block truncate text-xs text-slate-500">
                                {vm.hostName} · {vm.node} · {formatBytes(vm.maxdisk)} disk
                              </span>
                            </span>
                            {tagsOf(vm).length > 0 ? (
                              <span className="hidden shrink-0 items-center gap-1 sm:flex">
                                {tagsOf(vm)
                                  .slice(0, 3)
                                  .map((tag) => (
                                    <Chip key={tag}>{tag}</Chip>
                                  ))}
                              </span>
                            ) : null}
                            <StatusPill tone={toneForStatus(vm.status)} label={vm.status} />
                          </button>
                        )
                      })}
                      {filteredVMs.length === 0 ? (
                        <p className="px-3 py-6 text-center text-xs text-slate-500">
                          No virtual machine matches “{query}”.
                        </p>
                      ) : null}
                    </div>
                  )}
                </>
              )}
            </div>
          ) : (
            <div className="space-y-4">
              <div className="space-y-2">
                <p className="text-xs font-medium text-slate-400">Select agent</p>
                {agents.length === 0 ? (
                  <p className="rounded-lg border border-dashed border-slate-800 bg-slate-950/40 px-4 py-8 text-center text-xs text-slate-500">
                    No agents are enrolled yet. Generate an enrollment token on the Agents page and
                    install the agent inside the guest.
                  </p>
                ) : (
                  <div className="grid gap-2 sm:grid-cols-2">
                    {agents.map((agent) => (
                      <SelectTile
                        key={String(agent.id)}
                        selected={String(state.agentId) === String(agent.id)}
                        onClick={() => patch({ agentId: agent.id })}
                        icon={<ShieldCheck className="size-4" aria-hidden />}
                        title={agent.hostname}
                        description={`${agent.os}/${agent.arch} · agent ${agent.version} · ${agent.status}`}
                      />
                    ))}
                  </div>
                )}
              </div>

              <div className="space-y-2">
                <p className="text-xs font-medium text-slate-400">Include paths</p>
                <div className="flex gap-2">
                  <Input
                    value={pathDraft}
                    placeholder="/var/www  or  C:\\Users\\finance"
                    onChange={(event) => setPathDraft(event.target.value)}
                    onKeyDown={(event) => {
                      if (event.key === 'Enter') {
                        event.preventDefault()
                        addPath()
                      }
                    }}
                  />
                  <Button
                    onClick={addPath}
                    disabled={!pathDraft.trim()}
                    icon={<FolderPlus className="size-4" aria-hidden />}
                  >
                    Add
                  </Button>
                </div>

                {state.paths.length === 0 ? (
                  <p className="text-xs text-slate-500">
                    Add at least one directory or file to include in the backup.
                  </p>
                ) : (
                  <ul className="divide-y divide-slate-800 rounded-lg border border-slate-800 bg-slate-950/40">
                    {state.paths.map((path) => (
                      <li key={path} className="flex items-center gap-3 px-3 py-2">
                        <code className="min-w-0 flex-1 truncate font-mono text-xs text-slate-300">
                          {path}
                        </code>
                        <IconButton
                          aria-label={`Remove ${path}`}
                          onClick={() =>
                            patch({ paths: state.paths.filter((item) => item !== path) })
                          }
                        >
                          <X className="size-3.5" aria-hidden />
                        </IconButton>
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            </div>
          )}
        </div>
      ) : null}

      {/* Step 2 — target */}
      {step === 1 ? (
        <div className="space-y-3">
          <p className="text-xs font-medium text-slate-400">Where should the backups be written?</p>
          {targets.length === 0 ? (
            <p className="rounded-lg border border-dashed border-slate-800 bg-slate-950/40 px-4 py-8 text-center text-xs text-slate-500">
              No storage targets yet. Add an S3-compatible bucket on the Storage Targets page first.
            </p>
          ) : (
            <div className="grid gap-2 sm:grid-cols-2">
              {targets.map((target) => (
                <SelectTile
                  key={String(target.id)}
                  selected={String(state.targetId) === String(target.id)}
                  onClick={() => patch({ targetId: target.id })}
                  icon={<Database className="size-4" aria-hidden />}
                  title={target.name}
                  description={`${target.bucket} · ${target.endpoint.replace(/^https?:\/\//, '')}`}
                />
              ))}
            </div>
          )}
          <SectionNote>
            Chunks are deduplicated per target, so pointing several jobs at the same bucket shrinks
            total storage.
          </SectionNote>
        </div>
      ) : null}

      {/* Step 3 — schedule */}
      {step === 2 ? (
        <div className="space-y-3">
          <p className="text-xs font-medium text-slate-400">When should this job run?</p>
          <div className="grid gap-2 sm:grid-cols-2">
            <SelectTile
              selected={state.scheduleMode === 'manual'}
              onClick={() => patch({ scheduleMode: 'manual' })}
              icon={<Check className="size-4" aria-hidden />}
              title="Manual only"
              description="Never runs on its own — you press Run Now."
            />
            <SelectTile
              selected={state.scheduleMode === 'hourly'}
              onClick={() => patch({ scheduleMode: 'hourly' })}
              icon={<Check className="size-4" aria-hidden />}
              title="Hourly"
              description="Every hour, on the hour — cron 0 * * * *"
            />
            <SelectTile
              selected={state.scheduleMode === 'daily'}
              onClick={() => patch({ scheduleMode: 'daily' })}
              icon={<Check className="size-4" aria-hidden />}
              title="Daily at 02:00"
              description="Overnight window — cron 0 2 * * *"
            />
            <SelectTile
              selected={state.scheduleMode === 'weekly'}
              onClick={() => patch({ scheduleMode: 'weekly' })}
              icon={<Check className="size-4" aria-hidden />}
              title="Weekly, Sunday 03:00"
              description="Once a week — cron 0 3 * * 0"
            />
          </div>

          <SelectTile
            selected={state.scheduleMode === 'custom'}
            onClick={() => patch({ scheduleMode: 'custom' })}
            icon={<Check className="size-4" aria-hidden />}
            title="Custom cron expression"
            description="Five fields: minute, hour, day of month, month, day of week."
          />

          {state.scheduleMode === 'custom' ? (
            <Field
              label="Cron expression"
              error={
                state.cron.trim() && !isValidCron(state.cron)
                  ? 'Enter five space-separated cron fields.'
                  : undefined
              }
              hint="Example: 30 1 * * 1-5 — 01:30, Monday through Friday."
            >
              {({ id }) => (
                <Input
                  id={id}
                  value={state.cron}
                  placeholder="0 2 * * *"
                  className="font-mono"
                  onChange={(event) => patch({ cron: event.target.value })}
                />
              )}
            </Field>
          ) : null}
        </div>
      ) : null}

      {/* Step 4 — retention */}
      {step === 3 ? (
        <div className="space-y-4">
          <Field
            label="Keep last N restore points"
            hint="Older restore points are pruned after each successful run, and chunks referenced by no remaining manifest are garbage-collected."
          >
            {({ id }) => (
              <Input
                id={id}
                type="number"
                min={1}
                max={9999}
                value={String(state.retention)}
                className="w-32"
                onChange={(event) => patch({ retention: Number(event.target.value) })}
              />
            )}
          </Field>

          <div className="flex flex-wrap gap-2">
            {[3, 7, 14, 30].map((value) => (
              <Button
                key={value}
                size="sm"
                variant={state.retention === value ? 'primary' : 'secondary'}
                onClick={() => patch({ retention: value })}
              >
                Keep {value}
              </Button>
            ))}
          </div>

          {state.retention < 1 ? (
            <p className="text-xs text-red-400">Keep at least one restore point.</p>
          ) : null}
        </div>
      ) : null}

      {/* Step 5 — review */}
      {step === 4 ? (
        <div className="space-y-4">
          <dl className="divide-y divide-slate-800 overflow-hidden rounded-lg border border-slate-800 bg-slate-950/40">
            {[
              { label: 'Name', value: state.name.trim() || '—' },
              {
                label: 'Type',
                value: state.kind === 'vm' ? 'Virtual machines (agentless)' : 'Agent (file level)',
              },
              {
                label: 'Sources',
                value: byTag ? (
                  <span className="inline-flex flex-wrap items-center gap-x-2 gap-y-1">
                    Tag: <Chip tone="accent">{state.tagFilter || '—'}</Chip>
                    <span className="text-slate-500">
                      (currently <Num>{formatCount(tagMatches.length)}</Num>{' '}
                      {tagMatches.length === 1 ? 'VM' : 'VMs'})
                    </span>
                  </span>
                ) : state.kind === 'vm' ? (
                  state.selectedVMs.length === 0 ? (
                    '—'
                  ) : (
                    state.selectedVMs.map((vm) => `${vm.name} (#${vm.vmid})`).join(', ')
                  )
                ) : (
                  `${selectedAgent?.hostname ?? '—'} — ${state.paths.length} ${
                    state.paths.length === 1 ? 'path' : 'paths'
                  }`
                ),
              },
              { label: 'Target', value: selectedTarget?.name ?? '—' },
              { label: 'Schedule', value: describeSchedule(schedule) },
              { label: 'Retention', value: `Keep last ${state.retention}` },
            ].map((row) => (
              <div key={row.label} className="flex gap-4 px-4 py-2.5">
                <dt className="w-28 shrink-0 text-xs text-slate-500">{row.label}</dt>
                <dd className="min-w-0 flex-1 text-sm break-words text-slate-200">{row.value}</dd>
              </div>
            ))}
          </dl>

          {state.kind === 'agent' && state.paths.length > 0 ? (
            <ul className="space-y-1">
              {state.paths.map((path) => (
                <li key={path} className="font-mono text-xs text-slate-400">
                  {path}
                </li>
              ))}
            </ul>
          ) : null}

          <div className="flex items-center justify-between rounded-lg border border-slate-800 bg-slate-950/40 px-4 py-3">
            <div>
              <p className="text-sm text-slate-200">Enable this job</p>
              <p className="mt-0.5 text-xs text-slate-500">
                Disabled jobs keep their schedule but never fire.
              </p>
            </div>
            <Toggle
              checked={state.enabled}
              onChange={(checked) => patch({ enabled: checked })}
              label="Enable this job"
            />
          </div>

          {error ? (
            <p className="rounded-lg border border-red-500/30 bg-red-500/10 px-3.5 py-2.5 text-xs text-red-300">
              {error}
            </p>
          ) : null}
        </div>
      ) : null}

      {!isLast && !stepValid(step) ? (
        <p className="mt-5 flex items-center gap-2 text-xs text-slate-500">
          <Info className="size-3.5" aria-hidden />
          Complete this step to continue.
        </p>
      ) : null}
    </Modal>
  )
}
