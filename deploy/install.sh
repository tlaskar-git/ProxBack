#!/usr/bin/env bash
# ProxBack server installer for a Debian/Ubuntu Proxmox VM or LXC.
# Usage: run as root from a directory containing proxback-server (linux/amd64 build)
#        and optionally proxback-agent-linux-amd64 / proxback-agent-windows-amd64.exe
#        (they will be served from the web UI's agent-deployment page).
set -euo pipefail

if [[ $EUID -ne 0 ]]; then echo "Run as root." >&2; exit 1; fi
if [[ ! -f ./proxback-server ]]; then echo "proxback-server binary not found in $(pwd)" >&2; exit 1; fi

echo "Installing ProxBack server..."
id -u proxback &>/dev/null || useradd --system --home /var/lib/proxback --shell /usr/sbin/nologin proxback

install -d -o proxback -g proxback /var/lib/proxback /var/lib/proxback/downloads
install -d -o proxback -g proxback /opt/proxback
install -m 0755 -o proxback -g proxback ./proxback-server /opt/proxback/proxback-server

# Agent binaries served from the "Deploy Agent" page, if present alongside the installer.
[[ -f ./proxback-agent-linux-amd64 ]] && install -m 0644 ./proxback-agent-linux-amd64 /var/lib/proxback/downloads/proxback-agent-linux-amd64
[[ -f ./proxback-agent-windows-amd64.exe ]] && install -m 0644 ./proxback-agent-windows-amd64.exe /var/lib/proxback/downloads/proxback-agent-windows-amd64.exe
chown -R proxback:proxback /var/lib/proxback/downloads

install -m 0644 ./proxback.service /etc/systemd/system/proxback.service
systemctl daemon-reload
systemctl enable --now proxback

echo
echo "ProxBack is starting. Open http://$(hostname -I | awk '{print $1}'):8443"
echo "Sign in with the default credentials:  admin / admin"
echo ">>> Change the password immediately (Settings -> Password). The UI nags until you do. <<<"
echo
echo "Note: the server speaks plain HTTP; put it behind a TLS reverse proxy (nginx/caddy)"
echo "or keep it on a trusted management network."
