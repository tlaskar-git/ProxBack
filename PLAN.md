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

Jobs
- `GET  /api/jobs` → `[{"id","name","kind":"vm"|"agent","targetId","targetName","schedule",
  "retention","enabled","sources":[...],"tagFilter":string|null,"nextRun":RFC3339|null,
  "lastRun":JobRun|null}]`
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
- `GET  /api/runs/{id}` → JobRun
- `POST /api/runs/{id}/cancel`

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
- `GET/PUT /api/settings` → `{"serverName","concurrency","webhookUrl","notifyOn"}`
  - `webhookUrl`: empty disables notifications. `notifyOn`: `"off"|"failures"|"all"`.
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
