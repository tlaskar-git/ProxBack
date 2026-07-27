<div align="center">

# ProxBack

**Backup and recovery for Proxmox VE.**

Image-level backup of your virtual machines, deduplicated and compressed to
object storage, managed from one web console.

[![Release](https://img.shields.io/github/v/release/tlaskar-git/ProxBack?color=10b981)](https://github.com/tlaskar-git/ProxBack/releases)
[![License](https://img.shields.io/badge/license-MIT-blue)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8)](https://go.dev)

</div>

---

## Overview

ProxBack protects Proxmox VE estates without agents in every guest. It snapshots
virtual machines through Proxmox's own tooling, splits the stream into content-addressed
chunks, and stores only the chunks your target does not already hold. The result is a
first backup that costs one full copy and subsequent backups that cost only what changed.

It runs as a single binary with the web console built in. There is no database server to
install, no message queue, and no agent requirement for image-level protection.

## Capabilities

| | |
|---|---|
| **Agentless VM backup** | Snapshot-consistent image backup of Windows and Linux guests through a lightweight node service. No software inside the guest. |
| **File-level agents** | Optional in-guest agents for protecting individual directories on Windows and Linux. |
| **Incremental forever** | 4 MiB content-addressed chunks with SHA-256 deduplication. Unchanged data is never uploaded twice; every restore point remains independently restorable. |
| **Object storage targets** | Backblaze B2, Amazon S3, MinIO, or any S3-compatible endpoint. Credentials are encrypted at rest. |
| **Parallel, compressed transfer** | Chunks upload concurrently and are compressed individually, after chunking, so compression never degrades deduplication. Optional bandwidth ceiling. |
| **Policy-based grouping** | Target virtual machines by Proxmox tag. Guests tagged later join the job automatically at the next run. |
| **Verification** | On-demand integrity checks re-read every chunk of a restore point and validate its hash, proving recoverability before you need it. |
| **Operational visibility** | Live per-object progress, throughput, activity logs per run, and webhook notifications to any JSON endpoint. |
| **In-place updates** | The console checks for releases and updates itself, with checksum verification and automatic rollback of the previous binary. |

## Architecture

```
                    ┌──────────────────────────────┐
   Proxmox nodes    │        ProxBack server       │      Object storage
 ┌──────────────┐   │                              │   ┌──────────────────┐
 │ node helper  │──▶│  chunker → dedup → compress  │──▶│  Backblaze B2    │
 │ (vzdump)     │   │        ▲                     │   │  S3 / MinIO      │
 └──────────────┘   │        │                     │   └──────────────────┘
 ┌──────────────┐   │   scheduler · retention      │
 │ guest agent  │──▶│   web console · REST API     │
 │ (file level) │   │   SQLite state               │
 └──────────────┘   └──────────────────────────────┘
```

The **node helper** is a small service on each Proxmox node. It streams
`vzdump --stdout` for backup and pipes `qmrestore` for recovery, so consistency,
storage-type support, and guest-agent quiescing are handled by Proxmox itself rather than
reimplemented. The **server** owns chunking, deduplication, scheduling, retention, and all
credentials — helpers and agents never see your storage keys.

## Requirements

- Proxmox VE 7 or later, with an API token
- A Linux VM or container for the server (1 vCPU, 1 GB RAM, 10 GB disk is sufficient)
- An S3-compatible bucket
- Root SSH access to each Proxmox node, for automated helper deployment

## Installation

Download the latest release, then on the machine that will run the server:

```bash
curl -fsSLO https://github.com/tlaskar-git/ProxBack/releases/latest/download/proxback-server-linux-amd64
curl -fsSLO https://github.com/tlaskar-git/ProxBack/releases/latest/download/proxback-helper-linux-amd64
curl -fsSLO https://github.com/tlaskar-git/ProxBack/releases/latest/download/install.sh
chmod +x install.sh && sudo ./install.sh
```

The installer creates a service account, installs to `/opt/proxback`, stores state in
`/var/lib/proxback`, and enables the `proxback` systemd unit.

Open `http://<server>:8443` and sign in with **admin** / **admin**. The console will
require you to change the password before it stops warning you.

> The server speaks plain HTTP by design. Terminate TLS with a reverse proxy, or keep it on
> a trusted management network.

## First-run configuration

**1. Connect Proxmox.** In the Proxmox UI, create an API token under
*Datacenter → Permissions → API Tokens*, then grant it a role carrying `VM.Audit`,
`VM.Snapshot`, `VM.Backup`, and `Datastore.Audit` on path `/`:

```bash
pveum acl modify / -token 'root@pam!proxback' -role PVEVMAdmin
```

Add the host in *Proxmox Hosts → Add Host*. If the token cannot see any guests, ProxBack
says so explicitly rather than showing an empty list.

**2. Deploy node helpers.** In *Proxmox Hosts → Node helpers → Deploy helper*, choose a
node and supply its SSH credentials. ProxBack installs and enrols the helper itself. The
node's SSH host key fingerprint is shown for confirmation before anything executes, and
the password is used only for that connection — it is never stored.

**3. Add storage.** In *Storage Targets*, supply your bucket endpoint and keys. For
Backblaze B2 use `https://s3.<region>.backblazeb2.com` with path-style addressing enabled.
Every target is verified with a write/read/delete probe before it is accepted.

**4. Create a backup job.** Choose virtual machines individually or by tag, pick a target,
then set the schedule: hourly, daily, weekly on chosen days, or monthly on a chosen date,
with the time picked directly. The console confirms in plain language — *"Runs every Sunday
and Saturday at 02:00. Next run Saturday 1 Aug, 02:00"* — and states the server's time zone.
Cron remains available under Advanced for unusual cadences.

## Protection policy

Beyond sources, target, schedule and retention, a job can carry an optional protection
policy: guest-agent quiescing, disk or path exclusions, automatic retry, a maximum run
duration, a permitted backup window, and pre/post scripts run where the data lives.
Defaults are safe and the whole step is collapsed — a six-VM estate never needs to open it.

Retention supports GFS: keep-last plus daily, weekly, monthly and yearly copies. Before
saving, the console shows exactly which restore points a policy would keep and which it
would prune, and why each survivor was kept.

## Windows and Linux agents

For file-level protection inside a guest, install the agent and register it as a service in
one elevated command:

```powershell
proxback-agent.exe --server https://proxback:8443 --token <enrollment-token> --install
```

```bash
sudo proxback-agent --server https://proxback:8443 --token <enrollment-token> --install
```

The installer registers a real system service (Windows service or systemd unit), sets it to
start automatically, restarts it on failure, and verifies it reached running state before
reporting success. On Windows it logs to the Event Log, so a service that will not start
can be diagnosed. Remove with `--uninstall`; `--print-install` prints the manual steps.

The agent does not require the guest to run on Proxmox — any Windows or Linux machine that
can reach the server works.

## Monitoring

The Monitor page shows runs in flight as live sessions: overall progress, current
throughput with a short history, elapsed and estimated remaining time, and beneath it the
**objects in the session** — every virtual machine with its own progress, size, bytes read,
deduplication saving, and state. Finished runs drop into a history table; opening any run
shows the same object breakdown alongside a timestamped activity log of exactly what
happened. Runs can be cancelled while active, retried after a failure, and removed from
history individually or in bulk.

## Recovery

Restore points are listed per protected object, showing the full-to-incremental chain.
Recovery options:

- **Restore in place** — overwrite the original virtual machine.
- **Restore alongside** — recover to a new VMID for verification or granular extraction,
  optionally onto a different Proxmox storage.
- **File-level restore** — for agent-protected sources, unpack to any path on the guest.
- **Verify** — re-read and hash every chunk without writing anything, to confirm a restore
  point is sound.

Every chunk is validated against its SHA-256 during recovery; a corrupted or truncated
transfer fails loudly rather than producing a silently damaged disk.

## Configuration reference

Settings live in the console under *Settings*.

| Setting | Default | Purpose |
|---|---|---|
| Concurrency | 2 | Backup objects processed simultaneously |
| Parallel chunk uploads | 4 | Chunks in flight per stream. Object stores charge a fixed latency per object; overlapping uploads hides it |
| Compression | zstd | Per-chunk compression, applied after chunking |
| Upload limit | unlimited | Ceiling in Mbps, shared across all runs |
| Webhook URL, Notify on | off | JSON run summaries to any endpoint |

Environment overrides: `PROXBACK_UPDATE_REPO` points the updater at a different
repository; `PROXBACK_GC_GRACE` adjusts how long unreferenced chunks are protected
(default 24h, which is what allows an interrupted backup to resume rather than restart).

## Operating notes

**Interrupted backups resume.** Chunks uploaded by a run that was cancelled or failed are
retained, so the next attempt deduplicates against them instead of starting over.

**Updates respect running work.** The updater refuses to install while backups are in
flight, since the restart would cancel them.

**Retention is per job.** After each successful run, restore points beyond the keep count
are removed and chunks referenced by no remaining restore point are collected.

## Building from source

Requires Go 1.26+ and Node 22+.

```bash
git clone https://github.com/tlaskar-git/ProxBack.git
cd ProxBack

cd web && npm install && npm run build && cd ..
rm -rf internal/api/webdist && cp -r web/dist internal/api/webdist

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o out/proxback-server ./cmd/proxback-server
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o out/proxback-helper ./cmd/proxback-helper
```

Run the test suite, which exercises the full stack against bundled Proxmox and S3
simulators:

```bash
go test ./... -timeout 15m
```

To try the product without touching real infrastructure:

```bash
go run ./cmd/s3-sim  --listen :19000 &
go run ./cmd/pve-sim --listen :18006 &
go run ./cmd/proxback-server --listen :8443 --data ./data
```

Add the simulated host (`http://127.0.0.1:18006`, any token) and target
(`http://127.0.0.1:19000`, any keys, path-style on), then run a job.

## Repository layout

| Path | Contents |
|---|---|
| `cmd/proxback-server` | Server: REST API, web console, backup engine |
| `cmd/proxback-helper` | Node service for image backup and recovery |
| `cmd/proxback-agent` | In-guest agent for file-level protection |
| `cmd/pve-sim`, `cmd/s3-sim` | Simulators used by the test suite |
| `internal/engine` | Chunking, deduplication, compression, verification |
| `internal/sched` | Scheduling, retention, run orchestration |
| `web/` | React console, compiled into the server binary |
| `e2e/` | End-to-end suite driving the whole stack through the public API |
| `PLAN.md` | Architecture notes and the REST API contract |

## License

MIT. See [LICENSE](LICENSE).
