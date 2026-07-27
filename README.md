# ProxBack

ProxBack is a self-hosted, Veeam-style backup platform for [Proxmox VE](https://www.proxmox.com/):

- **Agentless VM backup** of Windows and Linux VMs via the Proxmox API (snapshot → disk
  export), plus **optional in-guest agents** for file-level backup of individual machines.
- **Incremental forever** — 4 MiB content chunks with SHA-256 deduplication. Unchanged data
  is never uploaded twice, and every restore point is self-contained via its manifest.
- **S3-compatible storage targets** — Backblaze B2, MinIO, AWS S3, or anything
  S3-compatible (custom endpoint and path-style addressing supported).
- **Web control panel** — a dark, button-first dashboard for hosts, VMs, backup jobs, live
  job monitoring, restore points, storage targets, and one-click agent deployment.
- **Tag-based grouping** — back up by Proxmox tag instead of a fixed VM list; guests
  tagged later are picked up automatically at the next run.
- **Restore-point verification** — one click re-downloads and SHA-256-checks every chunk
  of a restore point, proving it is restorable without writing anywhere.
- **Webhook notifications** — POST a JSON summary of every finished (or only failed) run
  to ntfy, Gotify, or any automation endpoint.
- **Single static binary** with the web UI embedded, SQLite state, systemd deployment,
  in-app updates from GitHub releases, no Docker required.

## Try it in five minutes (no Proxmox needed)

The repo ships simulators for both Proxmox and S3, so you can try the whole product on any
machine with Go 1.26+ installed:

```bash
git clone https://github.com/tlaskar-git/ProxBack.git
cd ProxBack
go run ./cmd/s3-sim  --listen :19000 &
go run ./cmd/pve-sim --listen :18006 &
go run ./cmd/proxback-server --listen :8443 --data ./data
```

Open http://localhost:8443 and sign in with the default credentials — username **admin**,
password **admin**. The UI will nag you to change the password; do that first (Settings →
Password). Then:

1. **Proxmox Hosts → Add Host** — base URL `http://127.0.0.1:18006`, token id
   `root@pam!proxback`, any secret.
2. **Storage Targets → Add Target** — endpoint `http://127.0.0.1:19000`, region
   `us-east-1`, bucket `proxback`, any keys, **path-style on**.
3. **Virtual Machines → Backup Now** on any of the simulated VMs, follow the wizard, and
   watch the run in **Monitor**. Run it twice to see deduplication (second run uploads 0 B).

## Installing on a Proxmox VM or LXC (production)

ProxBack runs as a systemd service inside a Debian/Ubuntu VM or LXC on (or near) your
Proxmox cluster.

### 1. Build the binaries

On any machine with **Go 1.26+** and **Node 22+**:

```bash
git clone https://github.com/tlaskar-git/ProxBack.git
cd ProxBack

# Web UI → embedded into the server binary
cd web && npm install && npm run build && cd ..
rm -rf internal/api/webdist && cp -r web/dist internal/api/webdist

# Server (Linux) and agents (Linux + Windows)
mkdir -p out
GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -o out/proxback-server               ./cmd/proxback-server
GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -o out/proxback-agent-linux-amd64    ./cmd/proxback-agent
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o out/proxback-agent-windows-amd64.exe ./cmd/proxback-agent
```

### 2. Install the server

Copy `out/*` plus `deploy/install.sh` and `deploy/proxback.service` to the target machine,
then as root:

```bash
sudo ./install.sh
```

The installer creates a `proxback` system user, installs the server to `/opt/proxback`,
stores state in `/var/lib/proxback`, stages the agent binaries for the web UI's download
page, and enables the `proxback` systemd service.

### 3. First sign-in

Open `http://<server-ip>:8443`. Sign in with **admin** / **admin**, then **change the
password immediately** — the login page, the dashboard banner, and the server log all
remind you until you do. Changing the password also revokes every other session.

> **Security notes**
> - The server speaks plain HTTP. Put it behind a TLS reverse proxy (nginx, Caddy,
>   Traefik) or keep it on a trusted management network.
> - Secrets (Proxmox token secrets, S3 keys) are AES-GCM encrypted at rest with a key file
>   stored next to the database. Back up `/var/lib/proxback` if you back up the server.

### 4. Connect your Proxmox host

1. In the Proxmox UI: **Datacenter → Permissions → API Tokens → Add** (e.g.
   `root@pam!proxback`). Either disable privilege separation or grant the token
   `VM.Audit`, `VM.Snapshot`, `VM.Backup`, and `Datastore.Audit`.
2. In ProxBack: **Proxmox Hosts → Add Host** — base URL `https://your-host:8006`, the
   token id and secret, and enable **insecure TLS** if the host still uses its default
   self-signed certificate.
3. **Install the node helper on each Proxmox node** (required for agentless VM image
   backup — real Proxmox has no disk-export API). In ProxBack, open the host's card →
   "Deploy node helper" → generate a token and paste the one-line install command into a
   root shell on each PVE node. The helper is a small systemd service that streams
   backups through Proxmox's own `vzdump` (snapshot-consistent, every storage type,
   qemu-guest-agent freeze) and restores through `qmrestore`. Without a helper on a VM's
   node, backup runs for that VM fail with an "install the node helper" error; file-level
   agent backups work regardless.

### 5. Add a storage target

**Storage Targets → Add Target.** For Backblaze B2:

- Endpoint `https://s3.<region>.backblazeb2.com` (e.g. `https://s3.us-west-004.backblazeb2.com`)
- Region `<region>` (e.g. `us-west-004`)
- Your bucket name and an application key with read/write access to that bucket
- **Path-style on**

Every target gets a connection test (write/read/delete of a probe object) so a typo fails
loudly before your first backup, not during it.

### 6. Create jobs, restore, deploy agents

- **Backup Jobs → Create Job** — pick VMs manually or **by Proxmox tag** (dynamic
  membership), or an agent + folders; then a target, a schedule (manual, hourly, daily,
  weekly, or any cron expression), and a keep-last-N retention. Job rows show the next
  scheduled run.
- **Restore Points** shows each source's full → incremental chain. Restores go to the
  original VMID or side by side into a free VMID; every chunk is hash-verified during the
  stream. The **Verify** button health-checks a point on demand (full chunk re-download +
  hash check) — the result appears in Monitor like any other run.
- **Settings → Notifications** — set a webhook URL and choose failures-only or all runs;
  every finished backup/restore/verify POSTs a JSON summary there.
- **Agents** — generate a single-use enrollment token and copy the one-line install
  command for Linux (bash) or Windows (PowerShell), or download the binary and pass
  `--server` and `--token` yourself. Agents heartbeat every 15 s and never hold S3
  credentials: chunks flow through the server.

## Updating

ProxBack updates itself from this repository's GitHub releases:

- **Settings → Software update** shows the installed version, checks the latest release,
  and — when a newer version with a binary for your platform exists — offers a one-click
  **Install update**. The new binary is downloaded, verified against the release's
  `checksums.txt`, swapped in place (the previous binary is kept as `proxback-server.old`),
  and the service restarts itself into the new build (`Restart=always` in the shipped
  systemd unit). Expect a few seconds of downtime; let running jobs finish first.
- Running outside systemd (or on Windows), the update is installed the same way but you
  restart the process yourself.
- Air-gapped or building from source? `git pull && go build` and replace the binary — the
  in-app updater is optional.
- The update source can be overridden with the `PROXBACK_UPDATE_REPO` environment variable
  (`owner/name`), e.g. to point a fleet at your own fork.

## Building and testing from source

```bash
go vet ./...
go test ./... -timeout 10m     # includes the full E2E scenario (sims + server in-process)
cd web && npm install && npm run build
```

The E2E suite proves: full backup, dedup on unchanged re-run, true incrementals after
mutation (with parent chains), byte-identical VM restores (including side-by-side to a new
VMID), agent file backup/restore round-trips, retention pruning with orphan-chunk GC,
index/bucket reconciliation after out-of-band chunk loss, and auth enforcement — including
the default-credential lifecycle.

## Repository layout

| Path | What it is |
|---|---|
| `cmd/proxback-server` | The server (REST API + embedded web UI + backup engine) |
| `cmd/proxback-agent` | In-guest agent for Windows/Linux file-level backup |
| `cmd/pve-sim` | Proxmox VE API simulator used by the E2E suite and the quick start |
| `cmd/s3-sim` | S3-compatible object store simulator |
| `internal/` | Engine, PVE client, S3 client, store, scheduler, agent manager, API |
| `web/` | React + Vite + Tailwind control panel (built into the server binary) |
| `deploy/` | `install.sh` and systemd units |
| `e2e/` | End-to-end test driving the whole stack through the public API |
| `PLAN.md` | Architecture spec and the REST API contract |

## License

MIT — see [LICENSE](LICENSE).
