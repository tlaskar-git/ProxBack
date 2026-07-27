/**
 * ProxBack REST API client.
 *
 * Mirrors the "REST API contract" section of PLAN.md exactly: one exported
 * function per endpoint, one exported interface per entity. Every network call
 * in the app goes through here — no raw `fetch` anywhere else.
 */

/* ---------------------------------------------------------------------------
 * Entities
 * ------------------------------------------------------------------------- */

/**
 * Identifier as returned by the server. The contract does not pin ids to a
 * concrete JSON type, so we accept either and treat them as opaque handles.
 */
export type ID = string | number

export interface User {
  id: ID
  username: string
}

export interface SetupStatus {
  needsSetup: boolean
  /** True while the seeded default admin/admin credentials are unchanged. */
  defaultLogin?: boolean
}

export interface AuthResponse {
  user: User
}

export interface MeResponse {
  user: User
  /** True while the seeded default admin/admin credentials are unchanged. */
  mustChangePassword?: boolean
  /** Version of the server build answering this request. */
  serverVersion?: string
}

export interface UpdateStatus {
  currentVersion: string
  latestVersion?: string
  updateAvailable: boolean
  releaseNotes?: string
  releaseUrl?: string
  publishedAt?: string
  assetName?: string
  assetAvailable: boolean
  /** Set when the release repository could not be reached. */
  checkError?: string
}

export interface UpdateApplyResult {
  ok: boolean
  version: string
  /** True when the server is about to restart itself into the new build. */
  restarting: boolean
}

/* Dashboard ---------------------------------------------------------------- */

export interface Last24h {
  succeeded: number
  failed: number
  running: number
}

export interface DashboardStats {
  vmCount: number
  agentCount: number
  hostCount: number
  targetCount: number
  jobCount: number
  last24h: Last24h
  storageBytes: number
  dedupSavedBytes: number
  recentRuns: JobRun[]
}

/* Protection posture (v0.5.0) ---------------------------------------------
 *
 * `GET /api/posture` is the console's evidence, not its opinion: the server
 * evaluates every workload against its own schedule's RPO and returns the
 * rolled-up verdict together with the reasons that produced it. The UI never
 * derives a verdict of its own — it renders `reasons` and the per-workload
 * rows underneath them.
 * ------------------------------------------------------------------------- */

export type PostureVerdict = 'protected' | 'at_risk' | 'unprotected' | 'unknown'

/** Per-workload evaluation. `unknown` is a roll-up state only. */
export type WorkloadStatus = 'protected' | 'at_risk' | 'unprotected'

export interface PostureCounts {
  protected: number
  atRisk: number
  unprotected: number
}

/** One explained contribution to the verdict, with how many workloads it covers. */
export interface PostureReason {
  code: string
  workloads: number
  detail: string
}

export interface PostureWorkload {
  kind: SourceKind
  id: ID
  name: string
  /**
   * Guest id an operator recognises. Absent for agents. Show this — never the
   * internal composite `id`, which is a storage key, not an identity.
   */
  vmid?: number
  /** Cluster this workload belongs to — half of its canonical identity. */
  hostName: string
  node: string
  /** Name of the protection policy (job) covering it, when there is one. */
  policy?: string
  enabled: boolean
  /** Recovery point objective in hours, derived from the job's schedule. */
  rpoHours?: number
  lastSuccessAt?: string | null
  /** Age of `lastSuccessAt` in hours, as the server measured it. */
  ageHours?: number
  withinRpo?: boolean
  lastFailureAt?: string | null
  /** Last time this workload's newest restore point passed integrity verification. */
  lastVerifiedAt?: string | null
  restorePoints: number
  status: WorkloadStatus
}

export interface Posture {
  verdict: PostureVerdict
  counts: PostureCounts
  reasons: PostureReason[]
  workloads: PostureWorkload[]
}

/* Proxmox hosts ----------------------------------------------------------- */

/** Server-reported health string. Unknown values render as a neutral pill. */
export type HostStatus = 'online' | 'offline' | 'error' | 'unknown' | (string & {})

export interface Host {
  id: ID
  name: string
  baseUrl: string
  tokenId: string
  insecureTLS: boolean
  status: HostStatus
  lastSeen: string | null
}

export interface HostCreate {
  name: string
  baseUrl: string
  tokenId: string
  tokenSecret: string
  insecureTLS: boolean
}

export interface HostTestResult {
  ok: boolean
  nodes: number
  error?: string
  /** Set when the host is reachable but the token cannot see any guests. */
  warning?: string
}

/* Virtual machines -------------------------------------------------------- */

export type VMStatus = 'running' | 'stopped' | 'paused' | 'suspended' | (string & {})

/** Live inventory row from `GET /api/hosts/{id}/vms`. */
export interface VM {
  vmid: number
  name: string
  node: string
  status: VMStatus
  maxdisk: number
  maxmem: number
  uptime: number
  /**
   * Proxmox tags for this guest — lower-cased and sorted by the server, and
   * possibly empty. Jobs can target a tag instead of a fixed VM list.
   */
  tags: string[]
}

/** Cached inventory row from `GET /api/vms` — same shape plus host attribution. */
export interface CachedVM extends VM {
  hostId: ID
  hostName: string
}

/* S3 targets -------------------------------------------------------------- */

export type TargetStatus = 'ok' | 'error' | 'unknown' | (string & {})

export interface Target {
  id: ID
  name: string
  endpoint: string
  bucket: string
  region: string
  pathStyle: boolean
  status: TargetStatus
}

export interface TargetCreate {
  name: string
  endpoint: string
  region: string
  bucket: string
  accessKey: string
  secretKey: string
  pathStyle: boolean
}

export interface TargetTestResult {
  ok: boolean
  error?: string
}

/* Schedules (v0.4.0) -------------------------------------------------------
 *
 * Operators pick a schedule the way they think about it; the server derives the
 * cron expression internally. `advanced` is the escape hatch and is never the
 * default. Times are `HH:MM` in the server's local timezone (`Settings.timezone`).
 * ------------------------------------------------------------------------- */

export type ScheduleKind = 'manual' | 'hourly' | 'daily' | 'weekly' | 'monthly' | 'advanced'

/** 0 = Sunday … 6 = Saturday, matching the contract. */
export type Weekday = 0 | 1 | 2 | 3 | 4 | 5 | 6

export interface ManualSchedule {
  kind: 'manual'
}

export interface HourlySchedule {
  kind: 'hourly'
  /** Minute past each hour, 0–59. */
  minute: number
}

export interface DailySchedule {
  kind: 'daily'
  /** `HH:MM`, 24-hour, server local time. */
  time: string
}

export interface WeeklySchedule {
  kind: 'weekly'
  time: string
  /** 0 = Sunday … 6 = Saturday. At least one. */
  weekdays: number[]
}

export interface MonthlySchedule {
  kind: 'monthly'
  time: string
  /** 1–31; 31 means "the last day of the month". */
  dayOfMonth: number
}

export interface AdvancedSchedule {
  kind: 'advanced'
  /** Five-field cron expression. */
  cron: string
}

export type Schedule =
  | ManualSchedule
  | HourlySchedule
  | DailySchedule
  | WeeklySchedule
  | MonthlySchedule
  | AdvancedSchedule

/**
 * What a job's `schedule` field can hold on the wire. The contract says
 * `GET /api/jobs` always returns the object, but a bare string is still
 * accepted on write and may come back from an older server — always read it
 * through `parseSchedule`.
 */
export type ScheduleValue = Schedule | string

const HHMM = /^([01]\d|2[0-3]):[0-5]\d$/

function clampInt(value: unknown, min: number, max: number, fallback: number): number {
  const n = typeof value === 'number' ? value : Number(value)
  if (!Number.isFinite(n)) return fallback
  return Math.max(min, Math.min(max, Math.round(n)))
}

function timeOf(value: unknown, fallback: string): string {
  return typeof value === 'string' && HHMM.test(value) ? value : fallback
}

/**
 * Normalises anything the server (or an older job record) may hold in
 * `schedule` into the v0.4.0 object form. A bare string is treated as
 * `manual` or `advanced`, exactly as the server does on write.
 */
export function parseSchedule(value: ScheduleValue | null | undefined): Schedule {
  if (value === null || value === undefined || value === '') return { kind: 'manual' }

  if (typeof value === 'string') {
    const text = value.trim()
    if (!text || text === 'manual') return { kind: 'manual' }
    return { kind: 'advanced', cron: text }
  }

  const raw = value as unknown as Record<string, unknown>
  switch (raw.kind) {
    case 'hourly':
      return { kind: 'hourly', minute: clampInt(raw.minute, 0, 59, 0) }
    case 'daily':
      return { kind: 'daily', time: timeOf(raw.time, '02:00') }
    case 'weekly': {
      const list = Array.isArray(raw.weekdays) ? raw.weekdays : []
      const weekdays = [...new Set(list.map((day) => clampInt(day, 0, 6, 0)))].sort((a, b) => a - b)
      return { kind: 'weekly', time: timeOf(raw.time, '03:00'), weekdays: weekdays.length ? weekdays : [0] }
    }
    case 'monthly':
      return {
        kind: 'monthly',
        time: timeOf(raw.time, '01:00'),
        dayOfMonth: clampInt(raw.dayOfMonth, 1, 31, 1),
      }
    case 'advanced':
      return { kind: 'advanced', cron: typeof raw.cron === 'string' ? raw.cron.trim() : '' }
    case 'manual':
      return { kind: 'manual' }
    default:
      return { kind: 'manual' }
  }
}

/** True when the schedule never fires on its own. */
export function isManualSchedule(schedule: Schedule): boolean {
  return schedule.kind === 'manual'
}

/* Jobs -------------------------------------------------------------------- */

export type JobKind = 'vm' | 'agent'

/** One selected VM inside a `kind: "vm"` job. */
export interface VMJobSource {
  hostId: ID
  vmid: number
  name: string
}

/** The single agent + include paths of a `kind: "agent"` job. */
export interface AgentJobSource {
  agentId: ID
  paths: string[]
}

/**
 * `sources` per the contract: an array of VM sources for VM jobs, and the
 * `{agentId, paths}` object for agent jobs. Servers that wrap the agent source
 * in a one-element array are handled too — use `vmSourcesOf`/`agentSourceOf`
 * instead of reading this field directly.
 */
export type JobSources = VMJobSource[] | AgentJobSource | AgentJobSource[]

/* Protection policy (v0.5.0) ----------------------------------------------
 *
 * Every field is optional on the wire and every default keeps the simple case
 * simple: a six-guest estate never has to open this. `parsePolicy` fills the
 * gaps so the rest of the app can read a complete object.
 * ------------------------------------------------------------------------- */

/** How the guest's filesystem is quiesced before the disk stream is read. */
export type Quiesce = 'none' | 'guest-agent'

/** Wall-clock window a run may *start* inside; `HH:MM`, server local time. */
export interface BackupWindow {
  start: string
  end: string
}

export interface JobPolicy {
  quiesce: Quiesce
  /** VM jobs: Proxmox disk identifiers to leave out, e.g. `scsi1`. */
  excludeDisks: string[]
  /** Agent jobs: path globs to leave out, e.g. `**​/node_modules`. */
  excludePaths: string[]
  /** 0–5 automatic retries of a failed workload. */
  retryCount: number
  /** 1–120 minutes between retries. */
  retryDelayMinutes: number
  /** Abandon the run after this many minutes. 0 = no limit. */
  maxDurationMinutes: number
  /** null = may start at any time. */
  window: BackupWindow | null
  /** Run on the helper/agent before and after the workload; output captured. */
  preScript: string
  postScript: string
  scriptTimeoutSeconds: number
  /** Upload ceiling for this job in Mbps. 0 = inherit the global setting. */
  uploadLimitMbpsOverride: number
}

export const DEFAULT_POLICY: JobPolicy = {
  quiesce: 'none',
  excludeDisks: [],
  excludePaths: [],
  retryCount: 0,
  retryDelayMinutes: 5,
  maxDurationMinutes: 0,
  window: null,
  preScript: '',
  postScript: '',
  scriptTimeoutSeconds: 30,
  uploadLimitMbpsOverride: 0,
}

function stringList(value: unknown): string[] {
  if (!Array.isArray(value)) return []
  return value
    .filter((item): item is string => typeof item === 'string')
    .map((item) => item.trim())
    .filter((item) => item.length > 0)
}

function windowOf(value: unknown): BackupWindow | null {
  if (typeof value !== 'object' || value === null) return null
  const raw = value as Record<string, unknown>
  const start = timeOf(raw.start, '')
  const end = timeOf(raw.end, '')
  if (!start || !end) return null
  return { start, end }
}

/** Normalises anything the server holds in `job.policy` into a full object. */
export function parsePolicy(value: unknown): JobPolicy {
  if (typeof value !== 'object' || value === null) return { ...DEFAULT_POLICY }
  const raw = value as Record<string, unknown>
  return {
    quiesce: raw.quiesce === 'guest-agent' ? 'guest-agent' : 'none',
    excludeDisks: stringList(raw.excludeDisks),
    excludePaths: stringList(raw.excludePaths),
    retryCount: clampInt(raw.retryCount, 0, 5, 0),
    retryDelayMinutes: clampInt(raw.retryDelayMinutes, 1, 120, 5),
    maxDurationMinutes: clampInt(raw.maxDurationMinutes, 0, 10_080, 0),
    window: windowOf(raw.window),
    preScript: typeof raw.preScript === 'string' ? raw.preScript : '',
    postScript: typeof raw.postScript === 'string' ? raw.postScript : '',
    scriptTimeoutSeconds: clampInt(raw.scriptTimeoutSeconds, 1, 3600, 30),
    uploadLimitMbpsOverride: clampInt(raw.uploadLimitMbpsOverride, 0, 10_000, 0),
  }
}

/** True while nothing in the policy departs from the defaults. */
export function isDefaultPolicy(policy: JobPolicy): boolean {
  return (
    policy.quiesce === DEFAULT_POLICY.quiesce &&
    policy.excludeDisks.length === 0 &&
    policy.excludePaths.length === 0 &&
    policy.retryCount === DEFAULT_POLICY.retryCount &&
    policy.maxDurationMinutes === DEFAULT_POLICY.maxDurationMinutes &&
    policy.window === null &&
    policy.preScript === '' &&
    policy.postScript === '' &&
    policy.uploadLimitMbpsOverride === DEFAULT_POLICY.uploadLimitMbpsOverride
  )
}

/**
 * The parts of a policy that differ from the defaults, phrased for an
 * operator. Empty means "standard protection" — say that, do not list ten
 * default values back at the reader.
 */
export function policyHighlights(policy: JobPolicy, kind: JobKind): string[] {
  const out: string[] = []
  if (policy.quiesce === 'guest-agent') out.push('Guest-agent quiescing')
  if (kind === 'vm' && policy.excludeDisks.length > 0) {
    out.push(`Excludes ${policy.excludeDisks.join(', ')}`)
  }
  if (kind === 'agent' && policy.excludePaths.length > 0) {
    out.push(
      policy.excludePaths.length === 1
        ? `Excludes ${policy.excludePaths[0]}`
        : `Excludes ${policy.excludePaths.length} path patterns`,
    )
  }
  if (policy.retryCount > 0) {
    out.push(
      `Retries ${policy.retryCount}× after ${policy.retryDelayMinutes} min`,
    )
  }
  if (policy.maxDurationMinutes > 0) out.push(`Stops after ${policy.maxDurationMinutes} min`)
  if (policy.window) out.push(`Starts only ${policy.window.start}–${policy.window.end}`)
  if (policy.preScript) out.push('Pre-backup script')
  if (policy.postScript) out.push('Post-backup script')
  if (policy.uploadLimitMbpsOverride > 0) {
    out.push(`Upload capped at ${policy.uploadLimitMbpsOverride} Mbps`)
  }
  return out
}

/* GFS retention (v0.5.0) ---------------------------------------------------
 *
 * `job.retention` is an object; a bare integer is still accepted and means
 * keep-last-N. A restore point survives if *any* rule retains it.
 * ------------------------------------------------------------------------- */

export interface RetentionPolicy {
  keepLast: number
  keepDaily: number
  keepWeekly: number
  keepMonthly: number
  keepYearly: number
}

export type RetentionValue = number | RetentionPolicy

export const DEFAULT_RETENTION: RetentionPolicy = {
  keepLast: 7,
  keepDaily: 0,
  keepWeekly: 0,
  keepMonthly: 0,
  keepYearly: 0,
}

/** Normalises a bare integer or a partial object into a full policy. */
export function parseRetention(value: RetentionValue | null | undefined): RetentionPolicy {
  if (typeof value === 'number') {
    return { ...DEFAULT_RETENTION, keepLast: clampInt(value, 0, 9999, 7) }
  }
  if (typeof value !== 'object' || value === null) return { ...DEFAULT_RETENTION }
  const raw = value as unknown as Record<string, unknown>
  return {
    keepLast: clampInt(raw.keepLast, 0, 9999, 0),
    keepDaily: clampInt(raw.keepDaily, 0, 9999, 0),
    keepWeekly: clampInt(raw.keepWeekly, 0, 9999, 0),
    keepMonthly: clampInt(raw.keepMonthly, 0, 9999, 0),
    keepYearly: clampInt(raw.keepYearly, 0, 9999, 0),
  }
}

/** True when only `keepLast` is in play — the simple case. */
export function isSimpleRetention(retention: RetentionPolicy): boolean {
  return (
    retention.keepDaily === 0 &&
    retention.keepWeekly === 0 &&
    retention.keepMonthly === 0 &&
    retention.keepYearly === 0
  )
}

/** Total number of rules in force — used to warn about "keeps nothing". */
export function retentionRuleCount(retention: RetentionPolicy): number {
  return (
    retention.keepLast +
    retention.keepDaily +
    retention.keepWeekly +
    retention.keepMonthly +
    retention.keepYearly
  )
}

/** One-line English summary, e.g. `Keep last 7 · 4 weekly · 6 monthly`. */
export function describeRetention(value: RetentionValue | null | undefined): string {
  const retention = parseRetention(value)
  const parts: string[] = []
  if (retention.keepLast > 0) parts.push(`Keep last ${retention.keepLast}`)
  if (retention.keepDaily > 0) parts.push(`${retention.keepDaily} daily`)
  if (retention.keepWeekly > 0) parts.push(`${retention.keepWeekly} weekly`)
  if (retention.keepMonthly > 0) parts.push(`${retention.keepMonthly} monthly`)
  if (retention.keepYearly > 0) parts.push(`${retention.keepYearly} yearly`)
  return parts.length === 0 ? 'Keeps nothing' : parts.join(' · ')
}

/** One entry of `GET /api/jobs/{id}/retention-preview`. */
export interface RetentionPreviewEntry {
  backupId: ID
  createdAt: string
  /** Which rules retained it — `last`, `daily`, `weekly`, `monthly`, `yearly`. */
  reasons: string[]
}

export interface RetentionPreview {
  keeps: RetentionPreviewEntry[]
  prunes: RetentionPreviewEntry[]
}

export interface Job {
  id: ID
  name: string
  kind: JobKind
  targetId: ID
  targetName: string
  /**
   * v0.4.0 schedule object. Older servers may still send a bare string —
   * read it through `parseSchedule`.
   */
  schedule: ScheduleValue
  /**
   * Server-rendered English summary ("Daily at 02:00", "Weekly on Sun, Sat at
   * 03:00"). Display verbatim when present; `describeSchedule` is the fallback.
   */
  scheduleLabel?: string
  /**
   * v0.5.0 GFS object. Older servers still send a bare keep-last-N integer —
   * read it through `parseRetention`.
   */
  retention: RetentionValue
  /**
   * v0.5.0 protection policy. Absent or partial on servers that predate it —
   * read it through `parsePolicy`.
   */
  policy?: Partial<JobPolicy> | null
  enabled: boolean
  sources: JobSources
  /**
   * VM jobs only. When set, membership is dynamic: at run start the job
   * resolves to every cached VM carrying this tag, so guests tagged later in
   * Proxmox are picked up automatically and `sources` may be empty.
   */
  tagFilter: string | null
  /**
   * Next scheduled fire time (RFC 3339), or null for manual schedules and
   * disabled jobs.
   */
  nextRun: string | null
  lastRun: JobRun | null
}

export interface JobCreate {
  name: string
  kind: JobKind
  targetId: ID
  /** Send the v0.4.0 object; a bare string is still accepted by the server. */
  schedule: ScheduleValue
  /** Send the v0.5.0 GFS object; a bare integer is still accepted. */
  retention: RetentionValue
  sources: JobSources
  enabled: boolean
  /** VM jobs only. Empty string clears an existing tag filter. */
  tagFilter?: string
  /** v0.5.0 protection policy. Omit to keep the server's defaults. */
  policy?: JobPolicy
}

export type JobPatch = Partial<JobCreate>

/* Job runs ---------------------------------------------------------------- */

export type RunStatus = 'running' | 'success' | 'failed' | 'canceled'

export interface JobRun {
  id: ID
  jobId: ID
  jobName: string
  status: RunStatus
  startedAt: string
  finishedAt?: string | null
  bytesProcessed: number
  bytesUploaded: number
  dedupRatio: number
  /**
   * v0.5.0 data reduction, defined once by the contract:
   * `reductionPct = 1 - uploaded/processed` (0 when processed is 0).
   */
  reductionPct?: number
  /**
   * `processed/uploaded`, and **omitted when uploaded is 0** — an infinite
   * ratio is never rendered as `1.0×`. Read it through `reductionOf`.
   */
  reductionRatio?: number
  error?: string | null
  progressPct: number
  currentStep: string
}

/* ---------------------------------------------------------------------------
 * Data reduction
 *
 * The contract defines these once so the console cannot invent a second
 * definition. A run that read 32 MiB and uploaded nothing is "100% avoided",
 * never "1.0×".
 * ------------------------------------------------------------------------- */

export interface DataReduction {
  /** 0–100. How much of the source data did not have to travel. */
  pct: number
  /** processed ÷ uploaded, or null when nothing was uploaded. */
  ratio: number | null
}

/**
 * Reads the server's `reductionPct` / `reductionRatio` when present and
 * otherwise derives them from the byte counters with the same formulas.
 */
export function reductionOf(run: {
  bytesProcessed: number
  bytesUploaded: number
  reductionPct?: number
  reductionRatio?: number
}): DataReduction {
  const processed = Number.isFinite(run.bytesProcessed) ? Math.max(0, run.bytesProcessed) : 0
  const uploaded = Number.isFinite(run.bytesUploaded) ? Math.max(0, run.bytesUploaded) : 0

  const pct =
    typeof run.reductionPct === 'number' && Number.isFinite(run.reductionPct)
      ? Math.max(0, Math.min(100, run.reductionPct))
      : processed === 0
        ? 0
        : Math.max(0, Math.min(100, (1 - uploaded / processed) * 100))

  // Only the server may assert a ratio; when it omits one and nothing was
  // uploaded, there is no ratio to show.
  const ratio =
    typeof run.reductionRatio === 'number' &&
    Number.isFinite(run.reductionRatio) &&
    run.reductionRatio > 0
      ? run.reductionRatio
      : uploaded > 0 && processed > 0
        ? processed / uploaded
        : null

  return { pct, ratio }
}

/** Per-object state inside a run — the "objects in this session" breakdown. */
export type RunSourceStatus = 'pending' | 'running' | 'success' | 'failed' | 'skipped'

export interface RunSource {
  /** Position in the run's source list; also the row's stable key. */
  seq: number
  name: string
  kind: SourceKind
  /** Proxmox node for VM sources. */
  node?: string
  /**
   * v0.5.0 identity, when the server attributes the source to a cluster. A
   * run can span clusters, and two of them can hold the same guest name.
   */
  hostName?: string
  vmid?: number
  status: RunSourceStatus
  bytesProcessed: number
  bytesUploaded: number
  /** Expected total size, used for this row's progress denominator. */
  sizeBytes: number
  progressPct: number
  startedAt?: string | null
  finishedAt?: string | null
  error?: string | null
}

/**
 * `GET /api/runs/{id}` — a run plus the v0.4.0 monitor payload. `sources` is
 * always an array; `throughputBps` is 0 unless the run is in flight.
 */
export interface RunDetail extends JobRun {
  sources: RunSource[]
  /** Bytes per second over the server's last sample window. */
  throughputBps: number
  /**
   * v0.5.0: where a restore run actually put the data. Persisted with the run
   * so the destination is a record, not a log line. Absent on backup and
   * verification runs.
   */
  destination?: RestoreDestination | null
}

export interface RunRef {
  runId: ID
}

/** One line of a run's persisted activity log. */
export interface RunLogLine {
  ts: string
  line: string
}

export interface RunLogResponse {
  lines: RunLogLine[]
}

/** Which terminal runs `POST /api/runs/clear` removes. */
export type ClearRunsScope = 'finished' | 'failed'

export interface ClearRunsResult {
  deleted: number
}

/* Restore points ---------------------------------------------------------- */

export type SourceKind = 'vm' | 'agent'
export type BackupKind = 'full' | 'incremental'

export interface BackupDisk {
  name: string
  sizeBytes: number
}

/** Outcome of the last integrity verification of a restore point. */
export type VerifyResult = 'passed' | 'failed'

export interface Backup {
  id: ID
  jobId: ID
  sourceKind: SourceKind
  sourceId: ID
  sourceName: string
  /**
   * v0.5.0 identity. Two clusters can hold identically named guests, so a
   * restore point carries the cluster it came from.
   */
  hostId?: ID
  hostName?: string
  /** Proxmox node the source lived on, when the server records it. */
  node?: string
  targetId: ID
  createdAt: string
  sizeBytes: number
  uploadedBytes: number
  kind: BackupKind
  parentId?: ID | null
  disks: BackupDisk[]
  /**
   * v0.5.0 verification evidence, attached to the point itself rather than
   * buried in run history. This is **integrity verified** — every chunk
   * re-read and re-hashed — and never a claim that a restore was tested.
   */
  lastVerifiedAt?: string | null
  lastVerifyResult?: VerifyResult | null
  verifiedBytes?: number
}

export interface BackupQuery {
  sourceKind?: SourceKind
  sourceId?: ID
  targetId?: ID
}

export interface RestoreVMTarget {
  hostId: ID
  node: string
  vmid: number
  /**
   * Proxmox storage the restored disks land on (vzdump-based `vma` restore
   * points only). Empty lets qmrestore use the storage recorded in the backup.
   */
  storage?: string
}

export interface RestoreAgentTarget {
  agentId: ID
  destPath: string
}

/**
 * v0.5.0 recovery mode.
 *
 * - `alongside` — the default and the recommendation. Requires a VMID that
 *   does not exist yet; the server suggests a free one.
 * - `overwrite` — destroys what is on the destination. Requires
 *   `confirmName` to match the destination VM's current name, and is refused
 *   with 409 otherwise. Never preselected.
 */
export type RestoreMode = 'alongside' | 'overwrite'

export interface RestoreRequest {
  backupId: ID
  mode: RestoreMode
  vm?: RestoreVMTarget
  agent?: RestoreAgentTarget
  /** Required for `overwrite`: the destination VM's current name, typed out. */
  confirmName?: string
}

/** Where a restore run put the data, persisted with the run. */
export interface RestoreDestination {
  host?: string
  node?: string
  vmid?: number
  storage?: string
  mode?: RestoreMode
  /** Agent restores record the unpack directory instead of a VMID. */
  destPath?: string
}

/** `GET /api/hosts/{id}/free-vmid` — the next VMID nothing is using. */
export interface FreeVMID {
  vmid: number
}

/* Agents ------------------------------------------------------------------ */

export type AgentStatus = 'online' | 'offline'

export interface Agent {
  id: ID
  hostname: string
  os: string
  arch: string
  version: string
  status: AgentStatus
  lastSeen: string
  registeredAt: string
}

export interface EnrollToken {
  token: string
  expiresAt: string
}

/* Node helpers ------------------------------------------------------------- */

/**
 * A ProxBack node helper: a root service on a Proxmox node that streams
 * `vzdump --stdout` / `qmrestore` so agentless VM image backup works on real
 * hosts (which have no disk-export API).
 */
/**
 * `unassigned` is not a health state: it means the helper predates v0.5.0 and
 * carries no host, so it is **not used for routing** and never guessed at.
 */
export type HelperStatus = 'online' | 'offline' | 'unassigned'

export interface Helper {
  id: ID
  /**
   * v0.5.0: helpers are keyed by (hostId, node), because two clusters can
   * each contain a node called `pve1`. Empty means the helper is unassigned.
   */
  hostId?: ID | null
  hostName?: string | null
  /** Proxmox node name this helper serves, within its host. */
  node: string
  address: string
  port: number
  version: string
  status: HelperStatus
  lastSeen: string
  registeredAt: string
}

/**
 * True while a helper has no host and therefore routes nothing. Covers both
 * the explicit status and the empty-hostId migration state.
 */
export function isHelperUnassigned(helper: Helper): boolean {
  if (helper.status === 'unassigned') return true
  const hostId = helper.hostId
  return hostId === undefined || hostId === null || String(hostId) === ''
}

/* Settings ---------------------------------------------------------------- */

/** When the server POSTs a run summary to `webhookUrl`. */
export type NotifyOn = 'off' | 'failures' | 'all'

/** Per-chunk compression applied before upload. */
export type Compression = 'zstd' | 'off'

export interface Settings {
  serverName: string
  concurrency: number
  /** Empty disables notifications entirely. */
  webhookUrl: string
  notifyOn: NotifyOn
  /** Chunk uploads in flight per stream, 1–16. */
  uploadConcurrency: number
  /** Compress each chunk before upload. Chunk boundaries stay on raw data, so
   * this never harms incremental deduplication. */
  compression: Compression
  /** Upload ceiling in Mbps shared by all runs. 0 = unlimited. */
  uploadLimitMbps: number
  /**
   * Read-only IANA name of the server's local timezone (v0.4.0). Every job
   * schedule fires in this zone, so the scheduling UI says so out loud.
   */
  timezone?: string
}

export interface WebhookTestResult {
  ok: boolean
  error?: string
}

/* ---------------------------------------------------------------------------
 * Transport
 * ------------------------------------------------------------------------- */

/** Error shape returned by the server for every failed request. */
export interface ApiErrorBody {
  error: string
}

/** Thrown by every client function when the server responds with a non-2xx. */
export class ApiError extends Error {
  readonly status: number
  readonly url: string
  /** Parsed JSON error body when the server sent one — some endpoints attach
   * structured fields beyond `error` (e.g. the SSH host-key `fingerprint`). */
  readonly body?: unknown

  constructor(status: number, message: string, url: string, body?: unknown) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.url = url
    this.body = body
  }

  /** True when the session cookie is missing or expired. */
  get isUnauthorized(): boolean {
    return this.status === 401
  }

  /** True when the request conflicted with current state (e.g. job running). */
  get isConflict(): boolean {
    return this.status === 409
  }
}

/** Narrowing helper so callers never need `instanceof` on an `unknown`. */
export function isApiError(err: unknown): err is ApiError {
  return err instanceof ApiError
}

/** Best-effort human message for anything thrown by the client. */
export function errorMessage(err: unknown): string {
  if (isApiError(err)) return err.message
  if (err instanceof Error) return err.message
  return String(err)
}

type Json = Record<string, unknown> | unknown[]

type QueryValue = string | number | boolean | ID | undefined | null

interface RequestOptions {
  method?: 'GET' | 'POST' | 'PATCH' | 'PUT' | 'DELETE'
  body?: Json
  query?: Record<string, QueryValue>
  signal?: AbortSignal
}

function buildUrl(path: string, query?: Record<string, QueryValue>): string {
  if (!query) return path
  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(query)) {
    if (value === undefined || value === null || value === '') continue
    params.set(key, String(value))
  }
  const qs = params.toString()
  return qs ? `${path}?${qs}` : path
}

async function parseError(res: Response, url: string): Promise<ApiError> {
  let message = ''
  let body: unknown
  try {
    const text = await res.text()
    if (text) {
      try {
        const parsed = JSON.parse(text) as Partial<ApiErrorBody>
        body = parsed
        if (typeof parsed.error === 'string' && parsed.error) message = parsed.error
        else message = text
      } catch {
        message = text
      }
    }
  } catch {
    /* body unreadable — fall through to the generic message */
  }
  if (!message) {
    message = res.status === 401 ? 'Not signed in.' : `Request failed with status ${res.status}.`
  }
  return new ApiError(res.status, message, url, body)
}

async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { method = 'GET', body, query, signal } = options
  const url = buildUrl(path, query)

  let res: Response
  try {
    res = await fetch(url, {
      method,
      credentials: 'include',
      signal,
      headers: {
        Accept: 'application/json',
        ...(body === undefined ? {} : { 'Content-Type': 'application/json' }),
      },
      body: body === undefined ? undefined : JSON.stringify(body),
    })
  } catch (err) {
    if (err instanceof DOMException && err.name === 'AbortError') throw err
    throw new ApiError(0, 'Cannot reach the ProxBack server.', url)
  }

  if (!res.ok) throw await parseError(res, url)

  if (res.status === 204) return undefined as T
  const text = await res.text()
  if (!text) return undefined as T
  return JSON.parse(text) as T
}

/* ---------------------------------------------------------------------------
 * Setup / auth
 * ------------------------------------------------------------------------- */

export function getSetupStatus(): Promise<SetupStatus> {
  return request<SetupStatus>('/api/setup/status')
}

export function setup(username: string, password: string): Promise<AuthResponse> {
  return request<AuthResponse>('/api/setup', {
    method: 'POST',
    body: { username, password },
  })
}

export function login(username: string, password: string): Promise<AuthResponse> {
  return request<AuthResponse>('/api/login', {
    method: 'POST',
    body: { username, password },
  })
}

export function logout(): Promise<void> {
  return request<void>('/api/logout', { method: 'POST' })
}

export function getMe(): Promise<MeResponse> {
  return request<MeResponse>('/api/me')
}

export function changePassword(currentPassword: string, newPassword: string): Promise<void> {
  return request<void>('/api/me/password', {
    method: 'POST',
    body: { currentPassword, newPassword },
  })
}

/* ---------------------------------------------------------------------------
 * Software update
 * ------------------------------------------------------------------------- */

export function getUpdateStatus(): Promise<UpdateStatus> {
  return request<UpdateStatus>('/api/update/status')
}

/**
 * Installs the latest release. Refused with 409 while any run is in flight,
 * because the restart would cancel it — pass force to override.
 */
export function applyUpdate(force = false): Promise<UpdateApplyResult> {
  return request<UpdateApplyResult>('/api/update/apply', {
    method: 'POST',
    query: force ? { force: 1 } : undefined,
  })
}

/* ---------------------------------------------------------------------------
 * Dashboard
 * ------------------------------------------------------------------------- */

export function getDashboard(): Promise<DashboardStats> {
  return request<DashboardStats>('/api/dashboard')
}

/* ---------------------------------------------------------------------------
 * Protection posture
 * ------------------------------------------------------------------------- */

/**
 * The estate's evaluated posture. Arrays are normalised so callers can always
 * map over `reasons` and `workloads`, and an estate the server cannot judge
 * comes back as `unknown` rather than as a green light.
 */
export async function getPosture(signal?: AbortSignal): Promise<Posture> {
  const posture = await request<Posture>('/api/posture', { signal })
  const counts = posture.counts ?? { protected: 0, atRisk: 0, unprotected: 0 }
  return {
    verdict: posture.verdict ?? 'unknown',
    counts: {
      protected: Number(counts.protected) || 0,
      atRisk: Number(counts.atRisk) || 0,
      unprotected: Number(counts.unprotected) || 0,
    },
    reasons: Array.isArray(posture.reasons) ? posture.reasons : [],
    workloads: Array.isArray(posture.workloads) ? posture.workloads : [],
  }
}

/* ---------------------------------------------------------------------------
 * Proxmox hosts
 * ------------------------------------------------------------------------- */

export function listHosts(): Promise<Host[]> {
  return request<Host[]>('/api/hosts')
}

export function createHost(input: HostCreate): Promise<Host> {
  return request<Host>('/api/hosts', { method: 'POST', body: { ...input } })
}

export function testHost(id: ID): Promise<HostTestResult> {
  return request<HostTestResult>(`/api/hosts/${encodeURIComponent(String(id))}/test`, {
    method: 'POST',
  })
}

export function deleteHost(id: ID): Promise<void> {
  return request<void>(`/api/hosts/${encodeURIComponent(String(id))}`, { method: 'DELETE' })
}

/** Live inventory straight from the host; also refreshes the server-side cache. */
export function getHostVMs(id: ID): Promise<VM[]> {
  return request<VM[]>(`/api/hosts/${encodeURIComponent(String(id))}/vms`)
}

/** Cached inventory across every host. */
export function listVMs(): Promise<CachedVM[]> {
  return request<CachedVM[]>('/api/vms')
}

/**
 * The next VMID free on this host, used to prefill a restore-alongside
 * destination so the operator never has to guess one.
 */
export function getFreeVMID(hostId: ID, signal?: AbortSignal): Promise<FreeVMID> {
  return request<FreeVMID>(`/api/hosts/${encodeURIComponent(String(hostId))}/free-vmid`, { signal })
}

/* ---------------------------------------------------------------------------
 * S3 targets
 * ------------------------------------------------------------------------- */

export function listTargets(): Promise<Target[]> {
  return request<Target[]>('/api/targets')
}

export function createTarget(input: TargetCreate): Promise<Target> {
  return request<Target>('/api/targets', { method: 'POST', body: { ...input } })
}

export function testTarget(id: ID): Promise<TargetTestResult> {
  return request<TargetTestResult>(`/api/targets/${encodeURIComponent(String(id))}/test`, {
    method: 'POST',
  })
}

export function deleteTarget(id: ID): Promise<void> {
  return request<void>(`/api/targets/${encodeURIComponent(String(id))}`, { method: 'DELETE' })
}

/* ---------------------------------------------------------------------------
 * Jobs
 * ------------------------------------------------------------------------- */

export function listJobs(): Promise<Job[]> {
  return request<Job[]>('/api/jobs')
}

export function createJob(input: JobCreate): Promise<Job> {
  return request<Job>('/api/jobs', { method: 'POST', body: { ...input } })
}

export function patchJob(id: ID, input: JobPatch): Promise<Job> {
  return request<Job>(`/api/jobs/${encodeURIComponent(String(id))}`, {
    method: 'PATCH',
    body: { ...input },
  })
}

export function deleteJob(id: ID): Promise<void> {
  return request<void>(`/api/jobs/${encodeURIComponent(String(id))}`, { method: 'DELETE' })
}

/** Starts the job now. Rejects with a 409 `ApiError` when it is already running. */
export function runJob(id: ID): Promise<RunRef> {
  return request<RunRef>(`/api/jobs/${encodeURIComponent(String(id))}/run`, { method: 'POST' })
}

/**
 * What a retention policy would keep and what it would prune, evaluated
 * against the restore points this job actually holds.
 *
 * The candidate policy is sent as query parameters so the operator can see the
 * effect of an edit *before* saving it; a server that only evaluates the
 * stored policy simply ignores them and the answer is still correct for what
 * is on disk today.
 */
export async function getRetentionPreview(
  jobId: ID,
  candidate?: RetentionPolicy,
  signal?: AbortSignal,
): Promise<RetentionPreview> {
  const preview = await request<RetentionPreview>(
    `/api/jobs/${encodeURIComponent(String(jobId))}/retention-preview`,
    {
      query: candidate
        ? {
            keepLast: candidate.keepLast,
            keepDaily: candidate.keepDaily,
            keepWeekly: candidate.keepWeekly,
            keepMonthly: candidate.keepMonthly,
            keepYearly: candidate.keepYearly,
          }
        : undefined,
      signal,
    },
  )
  return {
    keeps: Array.isArray(preview.keeps) ? preview.keeps : [],
    prunes: Array.isArray(preview.prunes) ? preview.prunes : [],
  }
}

/* ---------------------------------------------------------------------------
 * Job runs
 * ------------------------------------------------------------------------- */

export interface RunQuery {
  jobId?: ID
  limit?: number
}

export function listRuns(query: RunQuery = {}, signal?: AbortSignal): Promise<JobRun[]> {
  return request<JobRun[]>('/api/runs', {
    query: { jobId: query.jobId, limit: query.limit },
    signal,
  })
}

/**
 * One run with its per-source breakdown and live throughput. Servers that
 * predate v0.4.0 omit both fields, so they are normalised here — callers can
 * always read `sources.length` and `throughputBps`.
 */
export async function getRun(id: ID, signal?: AbortSignal): Promise<RunDetail> {
  const detail = await request<RunDetail>(`/api/runs/${encodeURIComponent(String(id))}`, { signal })
  // The restore destination has been carried under both names during the
  // v0.5.0 rollout; accept either and expose one.
  const raw = detail as unknown as Record<string, unknown>
  const destination = (detail.destination ?? raw.restore ?? null) as RestoreDestination | null
  return {
    ...detail,
    sources: Array.isArray(detail.sources) ? detail.sources : [],
    throughputBps: Number.isFinite(detail.throughputBps) ? detail.throughputBps : 0,
    destination: destination && typeof destination === 'object' ? destination : null,
  }
}

export function cancelRun(id: ID): Promise<void> {
  return request<void>(`/api/runs/${encodeURIComponent(String(id))}/cancel`, { method: 'POST' })
}

/**
 * Re-runs the job behind a finished run. Rejects with 409 when that job
 * already has a run in flight, and 404 for restore/verify runs — those have no
 * job to re-run.
 */
export function retryRun(id: ID): Promise<RunRef> {
  return request<RunRef>(`/api/runs/${encodeURIComponent(String(id))}/retry`, { method: 'POST' })
}

export function getRunLog(id: ID, signal?: AbortSignal): Promise<RunLogResponse> {
  return request<RunLogResponse>(`/api/runs/${encodeURIComponent(String(id))}/log`, { signal })
}

export function deleteRun(id: ID): Promise<void> {
  return request<void>(`/api/runs/${encodeURIComponent(String(id))}`, { method: 'DELETE' })
}

export function clearRuns(scope: ClearRunsScope, jobId?: ID): Promise<ClearRunsResult> {
  return request<ClearRunsResult>('/api/runs/clear', {
    method: 'POST',
    body: { scope, ...(jobId === undefined ? {} : { jobId }) },
  })
}

/* ---------------------------------------------------------------------------
 * Restore points & restore
 * ------------------------------------------------------------------------- */

export function listBackups(query: BackupQuery = {}): Promise<Backup[]> {
  return request<Backup[]>('/api/backups', {
    query: {
      sourceKind: query.sourceKind,
      sourceId: query.sourceId,
      targetId: query.targetId,
    },
  })
}

/**
 * Starts a health-check run that re-downloads every chunk of the restore point
 * and validates its SHA-256. Rejects with a 409 `ApiError` when a verification
 * of the same restore point is already in flight.
 */
export function verifyBackup(id: ID): Promise<RunRef> {
  return request<RunRef>(`/api/backups/${encodeURIComponent(String(id))}/verify`, {
    method: 'POST',
  })
}

export function deleteBackup(id: ID): Promise<void> {
  return request<void>(`/api/backups/${encodeURIComponent(String(id))}`, { method: 'DELETE' })
}

/**
 * Starts a restore. `mode` is always explicit — the server refuses an
 * `overwrite` whose `confirmName` does not match the destination VM's current
 * name with a 409, and `alongside` needs a VMID that does not exist yet.
 */
export function createRestore(input: RestoreRequest): Promise<RunRef> {
  return request<RunRef>('/api/restores', { method: 'POST', body: { ...input } })
}

/* ---------------------------------------------------------------------------
 * Agents
 * ------------------------------------------------------------------------- */

export function listAgents(): Promise<Agent[]> {
  return request<Agent[]>('/api/agents')
}

export function createEnrollToken(): Promise<EnrollToken> {
  return request<EnrollToken>('/api/agents/enroll-token', { method: 'POST' })
}

/* ---------------------------------------------------------------------------
 * Node helpers
 * ------------------------------------------------------------------------- */

export interface HelperDeployRequest {
  /**
   * v0.5.0: the host the node belongs to. Helpers are keyed by (hostId, node)
   * because two clusters can each contain a `pve1`.
   */
  hostId: ID
  node: string
  address: string
  /** SSH port. Defaults to 22 on the server. */
  port?: number
  username: string
  password: string
  /** The origin the helper should enroll against — pass window.location.origin. */
  serverUrl: string
  /** Port the helper will listen on. Defaults to 8007 on the server. */
  helperPort?: number
  /**
   * SHA256 host-key fingerprint the operator confirmed. Empty on the first
   * attempt: the server refuses with 409 + the observed fingerprint, the UI
   * shows it for confirmation, and the retry carries it.
   */
  hostKeyFingerprint?: string
}

export interface HelperDeployResult {
  ok: boolean
  /** Human-readable step lines from the deployment. */
  log: string[]
  helperOnline?: boolean
}

/** Fingerprint carried by the 409 "confirm the host key" deploy response. */
export function deployFingerprintOf(err: unknown): string | null {
  if (!isApiError(err) || err.status !== 409) return null
  const body = err.body as { fingerprint?: unknown } | undefined
  return typeof body?.fingerprint === 'string' && body.fingerprint ? body.fingerprint : null
}

export function deployHelper(req: HelperDeployRequest): Promise<HelperDeployResult> {
  return request<HelperDeployResult>('/api/helpers/deploy', { method: 'POST', body: { ...req } })
}

export function listHelpers(): Promise<Helper[]> {
  return request<Helper[]>('/api/helpers')
}

/**
 * Mints a helper enrollment token for one specific host. The helper inherits
 * the host from the token, so a Proxmox node never has to know its own
 * cluster identity.
 */
export function createHelperEnrollToken(hostId: ID): Promise<EnrollToken> {
  return request<EnrollToken>('/api/helpers/enroll-token', {
    method: 'POST',
    body: { hostId },
  })
}

/**
 * Binds an unassigned helper to a host. Until this happens the helper is not
 * used for routing — ProxBack never guesses which cluster a bare node name
 * belongs to.
 */
export function assignHelper(id: ID, hostId: ID): Promise<Helper> {
  return request<Helper>(`/api/helpers/${encodeURIComponent(String(id))}/assign`, {
    method: 'POST',
    body: { hostId },
  })
}

export function deleteHelper(id: ID): Promise<void> {
  return request<void>(`/api/helpers/${encodeURIComponent(String(id))}`, { method: 'DELETE' })
}

export function deleteAgent(id: ID): Promise<void> {
  return request<void>(`/api/agents/${encodeURIComponent(String(id))}`, { method: 'DELETE' })
}

/* ---------------------------------------------------------------------------
 * Settings
 * ------------------------------------------------------------------------- */

export function getSettings(): Promise<Settings> {
  return request<Settings>('/api/settings')
}

export function putSettings(input: Settings): Promise<Settings> {
  // `timezone` is read-only — never echo it back on the write.
  const { timezone: _timezone, ...writable } = input
  return request<Settings>('/api/settings', { method: 'PUT', body: { ...writable } })
}

/** Sends a sample payload to the saved webhook URL. Never throws on a 200. */
export function testWebhook(): Promise<WebhookTestResult> {
  return request<WebhookTestResult>('/api/settings/test-webhook', { method: 'POST' })
}

/* ---------------------------------------------------------------------------
 * Download paths for the agent binaries (served by the Go server).
 * ------------------------------------------------------------------------- */

export const AGENT_DOWNLOADS = {
  linux: '/downloads/proxback-agent-linux-amd64',
  windows: '/downloads/proxback-agent-windows-amd64.exe',
} as const

export const HELPER_DOWNLOAD = '/downloads/proxback-helper-linux-amd64' as const

/* ---------------------------------------------------------------------------
 * Job source helpers
 * ------------------------------------------------------------------------- */

function isAgentJobSource(value: unknown): value is AgentJobSource {
  return (
    typeof value === 'object' &&
    value !== null &&
    'agentId' in value &&
    Array.isArray((value as AgentJobSource).paths)
  )
}

function isVMJobSource(value: unknown): value is VMJobSource {
  return typeof value === 'object' && value !== null && 'vmid' in value && 'hostId' in value
}

/** The VM sources of a job, or `[]` for agent jobs. */
export function vmSourcesOf(job: Pick<Job, 'sources'>): VMJobSource[] {
  const sources: unknown = job.sources
  if (!Array.isArray(sources)) return []
  return (sources as unknown[]).filter(isVMJobSource)
}

/**
 * The Proxmox tags of an inventory row, tolerant of servers that omit the
 * field entirely.
 */
export function tagsOf(vm: Pick<VM, 'tags'>): string[] {
  const tags: unknown = vm.tags
  if (!Array.isArray(tags)) return []
  return tags.filter((tag): tag is string => typeof tag === 'string' && tag.length > 0)
}

/** Sorted union of every tag present in an inventory list. */
export function allTagsOf(vms: Pick<VM, 'tags'>[]): string[] {
  const seen = new Set<string>()
  for (const vm of vms) for (const tag of tagsOf(vm)) seen.add(tag)
  return [...seen].sort((a, b) => a.localeCompare(b))
}

/** VMs currently carrying `tag` — what a tag-filtered job resolves to today. */
export function vmsWithTag<T extends Pick<VM, 'tags'>>(vms: T[], tag: string): T[] {
  return vms.filter((vm) => tagsOf(vm).includes(tag))
}

/** The agent source of a job, or `null` for VM jobs. */
export function agentSourceOf(job: Pick<Job, 'sources'>): AgentJobSource | null {
  const sources: unknown = job.sources
  if (isAgentJobSource(sources)) return sources
  if (Array.isArray(sources)) {
    return (sources as unknown[]).find(isAgentJobSource) ?? null
  }
  return null
}
