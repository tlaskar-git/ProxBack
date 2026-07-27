#!/usr/bin/env bash
#
# ProxBack server installer for Debian/Ubuntu.
#
#   curl -fsSL https://github.com/tlaskar-git/ProxBack/releases/latest/download/install.sh | sudo bash
#
# Downloads the current release, or uses binaries already present in the working
# directory when run offline. Safe to re-run: an existing installation is
# upgraded in place and its data directory is left untouched.
#
set -euo pipefail

REPO="${PROXBACK_REPO:-tlaskar-git/ProxBack}"
VERSION="${PROXBACK_VERSION:-latest}"
PREFIX=/opt/proxback
DATA=/var/lib/proxback
UNIT=/etc/systemd/system/proxback.service

log()  { printf '\033[0;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[0;33m==>\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[0;31m==>\033[0m %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "Run as root (use sudo)."
[[ "$(uname -m)" == "x86_64" ]] || die "Unsupported architecture: $(uname -m). ProxBack ships amd64 builds."

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

base_url() {
  if [[ "$VERSION" == "latest" ]]; then
    echo "https://github.com/$REPO/releases/latest/download"
  else
    echo "https://github.com/$REPO/releases/download/$VERSION"
  fi
}

# Prefer a binary sitting next to the installer; otherwise fetch the release.
fetch() {
  local name="$1" dest="$2" required="$3"
  if [[ -f "./$name" ]]; then
    cp "./$name" "$dest"
    log "using local $name"
    return 0
  fi
  if curl -fsSL "$(base_url)/$name" -o "$dest" 2>/dev/null; then
    log "downloaded $name"
    return 0
  fi
  if [[ "$required" == "required" ]]; then
    die "Could not obtain $name — check network access, or place the file next to this script."
  fi
  warn "skipping $name (not available)"
  return 1
}

command -v curl >/dev/null || die "curl is required."
fetch proxback-server-linux-amd64 "$WORK/proxback-server" required

# Verify against the release checksums when both are available.
if fetch checksums.txt "$WORK/checksums.txt" optional; then
  expected="$(awk '/proxback-server-linux-amd64/ {print $1}' "$WORK/checksums.txt" | head -1)"
  actual="$(sha256sum "$WORK/proxback-server" | awk '{print $1}')"
  if [[ -n "$expected" && "$expected" != "$actual" ]]; then
    die "Checksum mismatch for proxback-server (expected $expected, got $actual)."
  fi
  [[ -n "$expected" ]] && log "checksum verified"
fi

log "creating service account and directories"
id -u proxback &>/dev/null || useradd --system --home "$DATA" --shell /usr/sbin/nologin proxback
install -d -o proxback -g proxback "$DATA" "$DATA/downloads"
install -d -o proxback -g proxback "$PREFIX"

upgrade=no
[[ -x "$PREFIX/proxback-server" ]] && upgrade=yes
[[ "$upgrade" == yes ]] && systemctl stop proxback 2>/dev/null || true

install -m 0755 -o proxback -g proxback "$WORK/proxback-server" "$PREFIX/proxback-server"

# Helper and agent binaries are served to nodes and guests from the console.
for extra in proxback-helper-linux-amd64 proxback-agent-linux-amd64 proxback-agent-windows-amd64.exe; do
  if fetch "$extra" "$WORK/$extra" optional; then
    install -m 0644 -o proxback -g proxback "$WORK/$extra" "$DATA/downloads/$extra"
  fi
done

if [[ -f ./proxback.service ]]; then
  install -m 0644 ./proxback.service "$UNIT"
else
  cat > "$UNIT" <<'SERVICE'
[Unit]
Description=ProxBack backup server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=proxback
Group=proxback
ExecStart=/opt/proxback/proxback-server --listen :8443 --data /var/lib/proxback
Restart=always
RestartSec=5
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/var/lib/proxback /opt/proxback
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
SERVICE
fi

systemctl daemon-reload
systemctl enable --now proxback

sleep 2
if ! systemctl is-active --quiet proxback; then
  die "The service failed to start. Check: journalctl -u proxback -n 50"
fi

ADDR="$(hostname -I 2>/dev/null | awk '{print $1}')"
version="$("$PREFIX/proxback-server" --version 2>/dev/null || echo '')"

echo
if [[ "$upgrade" == yes ]]; then
  log "ProxBack upgraded${version:+ to $version} and restarted."
else
  log "ProxBack ${version:+$version }installed."
  echo
  echo "    Console:  http://${ADDR:-<this-host>}:8443"
  echo "    Sign in:  admin / admin  — change this immediately in Settings."
fi
echo
echo "  Next: add a Proxmox host, deploy node helpers, and add a storage target."
echo "  The server speaks plain HTTP; put it behind a TLS proxy or keep it on a"
echo "  trusted management network."
echo
