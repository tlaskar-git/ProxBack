# ProxBack — Architecture & Build Plan

ProxBack is a Veeam-style backup platform for Proxmox VE. Single Go server binary with an
embedded React web control panel, optional in-guest agents for Windows/Linux, S3-compatible
backup targets (Backblaze B2, MinIO, AWS), and incremental-forever chunk-based backups.

Decisions (confirmed with owner):
- **Backup method:** Hybrid — agentless VM-image backup via Proxmox API + optional in-guest
  agents for file-level backup.
- **Stack:** Go backend (single binary), React + Vite + Tailwind frontend.
- **Deployment:** Linux systemd service inside a Proxmox VM/LXC (built here on Windows,
  cross-compiled for linux/amd64).
- **Testing:** Fully automated E2E against simulators (mock Proxmox API + embedded fake S3).
  No real infrastructure is touched.

## Repo layout

```
proxback/
├── PLAN.md                  (this file)
├── go.mod                   module proxback
├── cmd/
│   ├── proxback-server/     server entrypoint
│   ├── proxback-agent/      in-guest agent entrypoint (Windows + Linux)
│   ├── pve-sim/             Proxmox VE API simulator
│   └── s3-sim/              embedded S3 simulator (gofakes3 or equivalent)
├── internal/
│   ├── store/               SQLite persistence (modernc.org/sqlite — pure Go, NO CGO)
│   ├── auth/                users, bcrypt, session cookies, agent API keys
│   ├── pve/                 Proxmox VE API client (token auth)
│   ├── s3target/            S3 client (aws-sdk-go-v2, custom endpoint + path-style for B2/MinIO)
│   ├── engine/              chunking, manifests, incremental logic, restore
│   ├── sched/               cron scheduler + retention enforcement
│   ├── agentmgr/            agent registration, heartbeat, job dispatch (poll-based)
│   └── api/                 HTTP router (net/http or chi), all REST handlers, SPA serving
├── web/                     React + Vite + TS + Tailwind SPA (separate npm project)
├── deploy/                  install.sh, proxback.service, proxback-agent.service, README
└── e2e/                     Go E2E test(s) driving server+sims end to end
```

## Backup engine design (core of the product)

- Fixed-size chunking: 4 MiB chunks, SHA-256 per chunk.
- S3 object layout per target:
  - `chunks/<sha256>` — deduplicated chunk data (shared across all backups on the target)
  - `manifests/<sourceKind>/<sourceID>/<backupID>.json` — ordered chunk list per disk/file,
    sizes, timestamps, job info. sourceKind = `vm` | `agent`.
- **Full backup:** read source stream → chunk → hash → upload chunks not already present →
  write manifest.

### Throughput (v0.3.2)

Measured against a real install: a serial read→hash→upload loop used only ~52% of the
available uplink, because nothing was read or hashed while a chunk was in flight.

- **Parallel chunk uploads.** Chunks are read sequentially (cheap, preserves boundaries)
  and uploaded through a bounded worker pool (`uploadConcurrency`, default 4, 1–16).
  Manifest chunk **order must be preserved regardless of completion order** — workers
  return results tagged with the chunk's sequence number and the manifest is assembled in
  sequence. Any worker error fails the whole stream and cancels the rest.
- **Per-chunk compression** (`compression`: `"zstd"` default, `"off"`). Compression happens
  **after** chunking the raw stream, never before: chunk boundaries and the content address
  stay on *plaintext*, so a change early in a disk does not shift every later chunk and
  incremental dedup keeps working. Compressing the vzdump stream itself would cascade and
  turn every incremental into a near-full — the helper therefore keeps `--compress 0`.
  - The chunk key remains `chunks/<sha256-of-raw-chunk>`, so the dedup index, existing
    chunks, and existing manifests are all unaffected.
  - Stored object layout: compressed chunks carry a 4-byte magic `PBZ1` followed by the
    zstd frame. Reads sniff the magic; anything else is treated as raw (which is what
    pre-v0.3.2 chunks are). If a raw chunk coincidentally starts with `PBZ1`,
    decompression fails and the reader falls back to raw — the SHA-256 verification after
    reassembly is the final arbiter either way.
  - A chunk already present on the target is never re-uploaded, whatever form it is in.
  - `bytesUploaded` counts bytes actually sent (post-compression), so the run's figure
    reflects real bandwidth; `bytesProcessed` stays raw. Restore/verify decompress
    transparently.
- **Upload rate limit** (`uploadLimitMbps`, 0 = unlimited): a token-bucket shared by all
  concurrent uploads of the server, so scheduled backups can be kept from saturating the
  link.
- Settings gain `uploadConcurrency`, `compression`, `uploadLimitMbps` (validated on PUT);
  all three are safe to change between runs.
- **Incremental:** identical pipeline; the dedup check (server-side SQLite chunk index per
  target, with S3 HEAD fallback on cache miss) means unchanged chunks are never re-uploaded.
  A backup whose parent exists records `parentId` in the manifest for the UI chain display.
- **Restore:** read manifest → download chunks in order → reassemble → verify SHA-256 of every
  chunk + total size. VM restore streams the image back to the PVE host (sim accepts upload);
  agent restore streams files back to the agent.
- **Retention:** keep-last-N per job; deleting a backup point deletes its manifest, then a
  garbage-collect pass removes chunks referenced by no manifest on that target.
- Progress: engine reports bytes processed/uploaded/deduped via a callback; stored on the job
  run row so the UI can poll progress.

## Proxmox integration (agentless path)

`internal/pve` client implements against the real PVE JSON API shape (`/api2/json/...`,
`Authorization: PVEAPIToken=user@realm!tokenid=secret`, `verify-tls` toggle):
- `GET /api2/json/nodes` — list nodes
- `GET /api2/json/nodes/{node}/qemu` — list VMs (vmid, name, status, maxdisk, maxmem, uptime)
- `GET /api2/json/nodes/{node}/qemu/{vmid}/config` — disk list (scsi0/virtio0/ide0… entries)
- `POST /api2/json/nodes/{node}/qemu/{vmid}/snapshot` — create snapshot (`snapname`)
- `DELETE .../snapshot/{snapname}` — remove snapshot
- Task polling: `GET /api2/json/nodes/{node}/tasks/{upid}/status`
- Disk export (ProxBack extension endpoint, implemented by pve-sim and documented for real
  deployments): `GET /api2/json/nodes/{node}/qemu/{vmid}/proxback-export/{disk}` returns the
  raw disk stream of the snapshot; `POST .../proxback-import/{disk}` accepts a restore stream.

Backup flow (agentless): snapshot VM → export each disk stream through the engine → delete
snapshot → write manifest per disk (one backupID covers all disks of the VM).

## v0.5.0 — trust, policy depth, and identity

Driven by an external product review. Three classes of work: correctness defects that
undermine trust, protection-policy depth, and the product shell.

### Identity (correctness — highest severity)

A helper is currently keyed by **node name alone**, so two clusters that each contain a
`pve1` collide: registering the second deletes the first, and backup/restore traffic can
be routed to the wrong physical node. Helpers must be keyed by **(hostID, node)**.

- `helpers` gains `host_id`; uniqueness becomes `(host_id, node)`.
- Registration carries the host the helper belongs to. The enrollment token is minted for a
  specific host (`POST /api/helpers/enroll-token {"hostId"}`), so the helper inherits it —
  the node never has to know its own cluster identity.
- Lookup is `HelperFor(hostID, node)`. No global by-node resolution anywhere.
- Existing helpers migrate to `host_id = ''` and are reported as `"unassigned"`; they are
  **not** used for routing. The UI asks the operator to bind or re-deploy them. Never guess
  when more than one host could match.
- Workload identity is `cluster / name (vmid) / node` everywhere a source is chosen or
  displayed: VM inventory, job sources, restore points, run sources.
  `Backup` gains `hostId` + `hostName`; `vmDTO` already carries them.

### Protection posture (replaces the dashboard's optimistic verdict)

`GET /api/posture` → per-workload evaluation, rolled up:
```
{"verdict":"protected"|"at_risk"|"unprotected"|"unknown",
 "counts":{"protected":N,"atRisk":N,"unprotected":N},
 "reasons":[{"code","workloads":N,"detail"}],
 "workloads":[{"kind","id","name","hostName","node","policy"?,"enabled",
   "rpoHours"?,"lastSuccessAt"?,"ageHours"?,"withinRpo"?,"lastFailureAt"?,
   "lastVerifiedAt"?,"restorePoints","status":"protected"|"at_risk"|"unprotected"}]}
```
- A workload is `at_risk` when its own last success is older than its schedule's RPO plus a
  grace window, or its last run failed. Never derived from "the newest success anywhere".
- Empty estate reports `unknown`, not `0/0 — all guests in a job`.
- **Data-reduction metrics are defined once**: `reductionPct = 1 - uploaded/processed`
  (0 when processed is 0) and `reductionRatio = processed/uploaded` (omitted when uploaded
  is 0 — an infinite ratio is not displayed as `1.0×`). A run that read 32 MiB and uploaded
  nothing is "100% avoided", never "1.0×".

### Verification evidence

Verification results attach to the restore point, not just to run history.
`Backup` gains `lastVerifiedAt`, `lastVerifyResult` (`"passed"|"failed"`), `verifiedBytes`.
The UI distinguishes **integrity verified** (chunks re-hashed) from **restore tested**
(not yet implemented) and never claims the latter.

### Recovery safety

`POST /api/restores` gains an explicit `mode`: `"alongside"` (default) | `"overwrite"`.
- `alongside` requires a target VMID that does not exist; the server suggests a free one via
  `GET /api/hosts/{id}/free-vmid` → `{"vmid":N}`.
- `overwrite` requires `"confirmName"` matching the destination VM's current name, and is
  refused otherwise (409). It is never the default and never preselected.
- Restore run metadata persists the destination (`host`, `node`, `vmid`, `storage`, `mode`)
  and is shown in run detail, not only in a log line.

### Protection policy (`job.policy`, all fields optional with safe defaults)

```
{"quiesce":"none"|"guest-agent",           // freeze via qemu-guest-agent
 "excludeDisks":["scsi1"],                 // vm jobs
 "excludePaths":["**/node_modules"],       // agent jobs
 "retryCount":0-5,"retryDelayMinutes":1-120,
 "maxDurationMinutes":0=unlimited,
 "window":{"start":"22:00","end":"06:00"}|null,   // may only start inside this window
 "preScript":"","postScript":"",           // run on the helper/agent, output captured
 "scriptTimeoutSeconds":30,
 "uploadLimitMbpsOverride":0}              // 0 = inherit global
```
Presented as an optional **Advanced protection** step; defaults keep the six-VM case simple.

### GFS retention

`job.retention` becomes an object (a bare integer still accepted = keep-last-N):
```
{"keepLast":7,"keepDaily":0,"keepWeekly":4,"keepMonthly":6,"keepYearly":1}
```
A restore point survives if any rule retains it. `GET /api/jobs/{id}/retention-preview`
→ `{"keeps":[{"backupId","createdAt","reasons":["last","weekly"]}],"prunes":[...]}` so the
UI can show what a policy would keep before saving.

## Node helper (real-PVE agentless backup; v0.3.0)

Real Proxmox has no disk-export API, so agentless image backup of a real host runs through
a **node helper**: a single root binary (`cmd/proxback-helper`) installed as a systemd
service on each PVE node. It wraps PVE's own tooling — export streams
`vzdump <vmid> --mode snapshot --compress 0 --stdout` (a VMA archive; snapshot consistency,
all storage types, qemu-agent freeze handled by PVE itself) and import pipes the archive
into `qmrestore - <vmid>`. The simulator's `proxback-export`/`proxback-import` extension
remains as the test/dev path; when a VM's node has no registered helper and the extension
answers 404/501, the run fails with an actionable "install the node helper" error.

- Enrollment mirrors agents: UI generates a single-use token (24 h); the install one-liner
  downloads `/downloads/proxback-helper-linux-amd64` from the server and runs
  `proxback-helper --server <url> --token <t> --install`. On registration the helper
  generates its own access secret; the server stores it encrypted and authenticates to the
  helper with `Authorization: Bearer <secret>`.
- Helper HTTP API (listens on :8007 by default, plain HTTP on the management network):
  - `GET  /healthz` → `{"node","version"}` (unauthenticated)
  - `GET  /export/{vmid}` → VMA stream (vzdump --stdout)
  - `POST /import/{vmid}?storage=<s>` ← VMA stream (qmrestore from stdin)
- Automated deployment (v0.3.1): `POST /api/helpers/deploy` (session auth) connects to the
  node over SSH from the ProxBack server, uploads the staged helper binary, and runs the
  installer — no shell access needed by the operator. Body:
  `{"node","address","port":22,"username":"root","password","serverUrl","helperPort":8007,
  "hostKeyFingerprint":""}`.
  - Trust-on-first-use: when `hostKeyFingerprint` is empty or does not match, the server
    does NOT run anything and answers `409 {"error":...,"fingerprint":"SHA256:..."}`; the
    UI shows the fingerprint for confirmation and retries with it set. A matching
    fingerprint proceeds.
  - On success: `{"ok":true,"log":["...step lines..."]}` — the helper registers itself
    during `--install` (token minted server-side, passed on the remote command line only).
  - The password is used for this one connection, never persisted, never logged. Binary
    source is `<data>/downloads/proxback-helper-linux-amd64` (400-class error with a clear
    message when it is not staged).
  - `serverUrl` is supplied by the UI (`window.location.origin`) so the helper enrolls
    against the address the operator actually uses.
- Server REST additions (session auth unless noted):
  - `POST /api/helpers/enroll-token` → `{"token","expiresAt"}`
  - `GET  /api/helpers` → `[{"id","node","address","port","version","status":"online"|
    "offline","lastSeen","registeredAt"}]`
  - `DELETE /api/helpers/{id}`
  - Helper-facing: `POST /api/helpers/register` `{token,node,port,version,accessSecret}` →
    `{"helperId","apiKey"}` (address learned from the connection's remote IP);
    `POST /api/helpers/heartbeat` (Bearer apiKey, 30 s cadence; online = seen within 90 s)
- Backup via helper: matched by PVE node name; the whole VM streams as ONE disk entry
  named `vma` in the manifest (`disks:[{"name":"vma",...}]`). No explicit PVE snapshot
  calls — vzdump owns consistency. Restore via helper streams the `vma` entry into
  `/import/{vmid}`. Sim-backed VMs (no helper registered, extension present) keep the
  per-disk path unchanged.
- E2E: a fake in-process helper (httptest) registers via the real enrollment flow, serves
  a deterministic VMA-stand-in stream for export and captures imports; the suite proves
  helper-path backup, dedup on re-run, and byte-identical helper restore.

`cmd/pve-sim` serves this API subset with 2 nodes / 4 configurable fake VMs whose disk
content is deterministic pseudo-random data (seeded per VM, a few MiB each) that can be
**mutated** via a sim-only endpoint (`POST /sim/mutate/{vmid}`) so E2E can prove incrementals
upload fewer chunks. Auth: accepts any PVEAPIToken header (records it for assertions).
Sim guests carry PVE-style tags (semicolon-separated `tags` field in the qemu listing):
web-01 `prod;web`, db-01 `prod;db`, app-01 `dev`, mail-01 `dev;mail`.

## Agent design (file-level path)

- Single static Go binary, cross-compiled windows/amd64 + linux/amd64.
- Registration: UI generates a one-time enrollment token; agent runs with
  `--server https://host:8443 --token XYZ`, exchanges it for a permanent agent API key,
  registers hostname/OS.
- Poll loop: heartbeat every 15s (`POST /api/agents/heartbeat`), fetch pending jobs.
- File backup job: walk configured include paths, tar-stream them, pipe through the same
  chunk engine **via the server** (agent uploads chunks to server endpoints; server owns S3
  creds — agents never see them). Restore downloads the tar stream back and unpacks to a
  target directory.
- Config file + `--install` flag that prints (Linux) a systemd unit or (Windows) `sc.exe`
  service registration instructions.

## Persistence (SQLite, pure-Go driver)

Tables: `users`, `sessions`, `pve_hosts`, `vms_cache`, `s3_targets` (secrets AES-GCM encrypted
with a key file next to the DB), `agents`, `enroll_tokens`, `jobs`, `job_runs`, `backups`
(restore points), `chunk_index` (target_id, sha256, size), `settings`.

## REST API contract (server ⇄ web UI — both build agents follow this exactly)

All under `/api`, JSON. Auth: session cookie (`proxback_session`); 401 when missing.
Errors: `{"error":"message"}` with proper status codes.

Setup/auth
- `GET  /api/setup/status` → `{"needsSetup":bool}`
- `POST /api/setup` `{username,password}` (only when needsSetup; creates admin, logs in)
- `POST /api/login` `{username,password}` → sets cookie, returns `{"user":{"id","username"}}`
- `POST /api/logout`
- `GET  /api/me` → `{"user":{...}}` or 401

Dashboard
- `GET /api/dashboard` → `{"vmCount","agentCount","hostCount","targetCount",
  "jobCount","last24h":{"succeeded","failed","running"},"storageBytes","dedupSavedBytes",
  "recentRuns":[JobRun...]}`

Proxmox hosts
- `GET  /api/hosts` → `[{"id","name","baseUrl","tokenId","insecureTLS","status","lastSeen"}]`
- `POST /api/hosts` `{name,baseUrl,tokenId,tokenSecret,insecureTLS}` (validates by listing nodes)
- `POST /api/hosts/{id}/test` → `{"ok":bool,"nodes":int,"error"?}`
- `DELETE /api/hosts/{id}`
- `GET  /api/hosts/{id}/vms` → live inventory
  `[{"vmid","name","node","status","maxdisk","maxmem","uptime","tags":["prod","web"]}]`
  (also refreshes vms_cache; `tags` parsed from PVE's semicolon-separated `tags` field,
  lower-cased, sorted, may be empty)
- `GET  /api/vms` → cached inventory across all hosts, same shape + `hostId`,`hostName`

S3 targets
- `GET  /api/targets` → `[{"id","name","endpoint","bucket","region","pathStyle","status"}]`
  (never returns secretKey)
- `POST /api/targets` `{name,endpoint,region,bucket,accessKey,secretKey,pathStyle}`
- `POST /api/targets/{id}/test` → `{"ok":bool,"error"?}` (put+get+delete probe object)
- `DELETE /api/targets/{id}`

### Schedules (v0.4.0) — no cron in the product surface

Operators pick a schedule the way they think about it, not in cron syntax. A job's
`schedule` is an object; the server derives the cron expression internally.

```
{"kind":"manual"}
{"kind":"hourly","minute":30}                         // every hour at :30
{"kind":"daily","time":"02:00"}
{"kind":"weekly","time":"03:00","weekdays":[0,6]}     // 0=Sunday … 6=Saturday
{"kind":"monthly","time":"01:00","dayOfMonth":1}      // 1–31; 31 runs on the last day
{"kind":"advanced","cron":"*/15 * * * *"}             // escape hatch, never the default
```

- `POST/PATCH /api/jobs` accept this object. A bare string is still accepted and treated
  as `advanced` (or `manual`), so existing automation keeps working.
- `GET /api/jobs` always returns the object plus `"scheduleLabel"` — a rendered English
  summary ("Daily at 02:00", "Weekly on Sun, Sat at 03:00") the UI displays verbatim.
- Existing jobs migrate on upgrade: a stored cron that matches a preset becomes that
  preset; anything else becomes `advanced` with the cron preserved.
- All times are the server's local timezone; `GET /api/settings` gains read-only
  `"timezone"` so the UI can say so.

Jobs
- `GET  /api/jobs` → `[{"id","name","kind":"vm"|"agent","targetId","targetName","schedule",
  "scheduleLabel","retention","enabled","sources":[...],"tagFilter":string|null,
  "nextRun":RFC3339|null,"lastRun":JobRun|null}]`
  - vm job sources: `[{"hostId","vmid","name"}]`; agent job: `{"agentId","paths":[...]}`
  - `tagFilter` (vm jobs only): when set, membership is dynamic — at run start the job
    resolves to every cached VM carrying that tag (sources array may be empty); VMs added
    to Proxmox later with the tag are picked up automatically. A run with zero matching
    VMs fails with a clear error.
  - `nextRun`: next scheduled fire time computed from the cron schedule; null for
    "manual" schedules and disabled jobs.
- `POST /api/jobs` `{name,kind,targetId,schedule("manual"|cron5),retention,sources,
  tagFilter?,enabled}`
- `PATCH /api/jobs/{id}` (same fields, partial; setting tagFilter:"" clears it)
- `DELETE /api/jobs/{id}`
- `POST /api/jobs/{id}/run` → `{"runId"}` (409 if already running)
- `GET  /api/runs?jobId=&limit=` → `[JobRun]`
  JobRun = `{"id","jobId","jobName","status":"running"|"success"|"failed"|"canceled",
  "startedAt","finishedAt"?,"bytesProcessed","bytesUploaded","dedupRatio","error"?,
  "progressPct","currentStep"}`
- `GET  /api/runs/{id}` → JobRun **plus `"sources"`** (v0.4.0): the per-object breakdown
  that drives the visual monitor —
  `[{"seq","name","kind":"vm"|"agent","node"?,"status":"pending"|"running"|"success"|
  "failed"|"skipped","bytesProcessed","bytesUploaded","sizeBytes","progressPct",
  "startedAt"?,"finishedAt"?,"error"?}]`.
  Written as the run walks its sources, so a run of 8 VMs shows 8 rows advancing
  independently — one finished, one at 40%, six pending. Always an array.
- `GET /api/runs/{id}` also returns `"throughputBps"` (bytes/second over the last sample
  window, 0 when not running) so the monitor can plot live speed.
- `POST /api/runs/{id}/cancel`
- `POST /api/runs/{id}/retry` (v0.4.0) → `{"runId"}` — re-runs the same job; 409 when the
  job already has a run in flight, 404 for restore/verify runs (no job to re-run).
- Run history detail & cleanup (v0.3.1):
  - `GET /api/runs/{id}/log` → `{"lines":[{"ts":RFC3339,"line":string}]}` — persisted
    per-run activity log (run started, each step transition, per-source completions with
    byte counts, warnings, the final error/summary), capped at 500 lines per run, deleted
    with the run.
  - `DELETE /api/runs/{id}` — remove a finished run from history (409 while running).
    Deleting a run never touches restore points.
  - `POST /api/runs/clear` `{"scope":"finished"|"failed","jobId"?}` → `{"deleted":N}` —
    bulk-remove terminal runs (finished = success+failed+canceled), optionally for one job.

Restore points & restore
- `GET  /api/backups?sourceKind=&sourceId=&targetId=` → `[{"id","jobId","sourceKind",
  "sourceId","sourceName","targetId","createdAt","sizeBytes","uploadedBytes","kind":"full"|
  "incremental","parentId"?,"disks":[{"name","sizeBytes"}]}]`
- `POST /api/backups/{id}/verify` → `{"runId"}` — health-check run (jobName
  "Verify <sourceName>") that downloads every chunk of the restore point and validates its
  SHA-256 + sizes without writing anywhere; success means the point is restorable.
  409 when a verify for the same backup is already running.
- `DELETE /api/backups/{id}`
- `POST /api/restores` `{backupId, vm?:{hostId,node,vmid}, agent?:{agentId,destPath}}`
  → `{"runId"}` (restore runs appear in /api/runs with jobName "Restore …")

Agents
- `GET  /api/agents` → `[{"id","hostname","os","arch","version","status":"online"|"offline",
  "lastSeen","registeredAt"}]`
- `POST /api/agents/enroll-token` → `{"token","expiresAt"}` (single use, 24 h)
- `DELETE /api/agents/{id}`
- Agent-facing (API-key auth via `Authorization: Bearer <agentKey>`):
  - `POST /api/agents/register` `{token,hostname,os,arch,version}` → `{"agentId","apiKey"}`
  - `POST /api/agents/heartbeat` → `{"jobs":[pending agent job descriptors]}`
  - `POST /api/agents/runs/{runId}/chunks` (raw chunk body, `X-Chunk-Sha256` header)
  - `POST /api/agents/runs/{runId}/complete` `{manifest...}` / `.../fail` `{error}`
  - `GET  /api/agents/restores/{runId}/stream` → tar stream for restore

Settings
- `GET/PUT /api/settings` → `{"serverName","concurrency","webhookUrl","notifyOn",
  "uploadConcurrency","compression","uploadLimitMbps"}`
  - `webhookUrl`: empty disables notifications. `notifyOn`: `"off"|"failures"|"all"`.
  - `uploadConcurrency`: 1–16, default 4. `compression`: `"zstd"|"off"`, default `"zstd"`.
    `uploadLimitMbps`: 0–10000, 0 = unlimited.
- `POST /api/settings/test-webhook` → `{"ok":bool,"error"?}` — sends a sample payload to
  the saved webhook URL.

Notifications: when a backup/restore/verify run finishes and notifyOn matches, the server
POSTs JSON to webhookUrl (10 s timeout; failures are logged, never block runs):
`{"event":"run.finished","server":serverName,"job":jobName,"kind":"vm"|"agent"|"restore"|
"verify","status","bytesProcessed","bytesUploaded","dedupRatio","error"?,"startedAt",
"finishedAt","durationSec"}`. The payload is plain JSON usable by ntfy, Gotify, Discord
(via webhook proxy), or any automation endpoint.

Software update (session auth; release source is the GitHub repo, overridable via
`PROXBACK_UPDATE_REPO` / `PROXBACK_UPDATE_API` env vars)
- `GET  /api/update/status` → `{"currentVersion","latestVersion"?,"updateAvailable",
  "releaseNotes"?,"releaseUrl"?,"publishedAt"?,"assetName"?,"assetAvailable","checkError"?}`
- `POST /api/update/apply` → `{"ok","version","restarting"}` — downloads the platform's
  server asset from the latest release, verifies it against the release's `checksums.txt`,
  swaps the running binary (previous kept as `.old`), then exits gracefully so systemd
  (`Restart=always`) boots the new build. 409 when already latest / no releases.
- `GET /api/me` additionally returns `"serverVersion"`.

Static: everything not under `/api` serves the embedded SPA (fallback to index.html).

## Web UI (React + Vite + TS + Tailwind, visual/button-first UX)

Dark, modern dashboard aesthetic (Veeam-like): sidebar nav with icons, big action buttons,
status pills (green/amber/red), progress bars on running jobs, wizards as modal step flows.
Poll running state every 2 s. Libraries: react-router, lucide-react icons, recharts for the
dashboard charts. No component library needed — Tailwind components.

Pages: Setup (first run) · Login · Dashboard · Proxmox Hosts (+add-host modal w/ Test
Connection) · Virtual Machines (card/grid, per-VM **Backup Now** button + backup-job wizard)
· Backup Jobs (list + create wizard: sources → target → schedule → retention → review) ·
Job Runs / Monitor (live progress) · Restore Points (per source, chain view full→inc, Restore
wizard + Delete) · Storage Targets (+add w/ Test) · Agents (list + **Deploy Agent** page:
generate token button, copyable one-line install commands for Windows PowerShell & Linux
bash, download links `/downloads/proxback-agent-{windows,linux}-amd64[.exe]`) · Settings.

`vite build` outputs `web/dist`; server embeds it via `go:embed` (build copies dist into
`internal/api/webdist`). Dev proxy: vite proxies `/api` → `localhost:8443`.

## E2E (Go test in e2e/, run with real built binaries or in-process servers)

1. Start s3-sim + pve-sim + server (in-process, ephemeral ports, temp dirs).
2. Setup admin → login → add PVE host (sim) → add S3 target (sim) → verify both Test OK.
3. Create VM backup job for 2 VMs → run → assert success, manifests exist, chunk count > 0.
4. Run again with no changes → assert near-zero uploaded bytes (dedup works).
5. Mutate VM disk via sim → run → assert only changed chunks uploaded, backup marked
   incremental with parentId.
6. Restore VM backup to sim → sim exposes imported bytes → assert byte-identical to source.
7. Agent flow: enroll token → start agent (in-process) pointing at temp dir with test files →
   agent job → run → mutate files → incremental → restore to second dir → diff equal.
8. Retention: set keep-last-2, run 3rd backup, assert oldest pruned + orphan chunks GC'd.
9. Auth: unauthenticated /api/jobs → 401; wrong password → 401.

## Build & verification protocol

- Phase 1: build the entire Go side (server, agent, sims, e2e) and the entire web/ SPA
  against the contract above, independently.
- Phase 2: embed the web build, `go vet`, `go build` all binaries (windows+linux),
  `go test ./...`, run the full E2E, launch server+sims locally, click through every page
  in the browser, fix all bugs found, re-verify.
- Phase 3: deploy/ artifacts (install.sh, systemd units, README with real-PVE setup notes).

Toolchain: Go 1.26+ and Node 22+. No Docker — the sims are plain Go binaries. The SQLite
driver must be modernc.org/sqlite so every binary builds with CGO_ENABLED=0.
