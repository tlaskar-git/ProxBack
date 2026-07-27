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
  /** Keep-last-N restore points. */
  retention: number
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
  retention: number
  sources: JobSources
  enabled: boolean
  /** VM jobs only. Empty string clears an existing tag filter. */
  tagFilter?: string
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
  error?: string | null
  progressPct: number
  currentStep: string
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

export interface Backup {
  id: ID
  jobId: ID
  sourceKind: SourceKind
  sourceId: ID
  sourceName: string
  targetId: ID
  createdAt: string
  sizeBytes: number
  uploadedBytes: number
  kind: BackupKind
  parentId?: ID | null
  disks: BackupDisk[]
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

export interface RestoreRequest {
  backupId: ID
  vm?: RestoreVMTarget
  agent?: RestoreAgentTarget
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
export interface Helper {
  id: ID
  /** Proxmox node name this helper serves (matched against VM inventory). */
  node: string
  address: string
  port: number
  version: string
  status: 'online' | 'offline'
  lastSeen: string
  registeredAt: string
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
  return {
    ...detail,
    sources: Array.isArray(detail.sources) ? detail.sources : [],
    throughputBps: Number.isFinite(detail.throughputBps) ? detail.throughputBps : 0,
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

export function createHelperEnrollToken(): Promise<EnrollToken> {
  return request<EnrollToken>('/api/helpers/enroll-token', { method: 'POST' })
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
