#!/bin/sh
# f2f service-node installer for Linux (systemd).
#
#   curl -fsSL https://raw.githubusercontent.com/vseplet/f2f/main/service.sh | \
#     sh -s -- --camp <camp_id> --name <node-name>
#
# Downloads the release binary, then installs and starts a systemd unit that
# runs `f2f --service`. The camp_id (a bearer secret) is written to a root-only
# EnvironmentFile (/etc/f2f/service.env), NOT the command line, so it doesn't
# show in `ps` / `systemctl cat`.
#
# Options:
#   --camp <id>        camp_id to join           (required)
#   --name <name>      this node's display name   (required)
#   --user <name>      own config/identity as this user (default: the invoking
#                      user, so the service reuses the SAME identity as your
#                      interactive `sudo f2f` runs — no duplicate node)
#   --version vX.Y.Z   pin a release              (default: latest)
#   --log  debug|info  F2F_LOG level              (default: info)
#   --bin-dir <dir>    binary location            (default: /usr/local/bin)
set -eu

REPO="vseplet/f2f"
VERSION="latest"
BIN_DIR="/usr/local/bin"
LOG="info"
CAMP=""
NAME=""
OWNER=""
UNIT="f2f.service"
ENV_DIR="/etc/f2f"
ENV_FILE="${ENV_DIR}/service.env"

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# --- parse args --------------------------------------------------------------

while [ $# -gt 0 ]; do
  case "$1" in
    --camp)    CAMP="${2:-}"; shift 2 ;;
    --name)    NAME="${2:-}"; shift 2 ;;
    --user)    OWNER="${2:-}"; shift 2 ;;
    --version) VERSION="${2:-}"; shift 2 ;;
    --log)     LOG="${2:-}"; shift 2 ;;
    --bin-dir) BIN_DIR="${2:-}"; shift 2 ;;
    *) die "unknown option: $1" ;;
  esac
done

[ -n "$CAMP" ] || die "--camp <camp_id> is required"
[ -n "$NAME" ] || die "--name <node-name> is required"

# systemd only (this is the headless-server path; workstations use install.sh).
[ "$(uname -s)" = "Linux" ] || die "service.sh is Linux/systemd only; on macOS run 'f2f --service' yourself"
command -v systemctl >/dev/null 2>&1 || die "systemctl not found — this host isn't systemd"
command -v curl >/dev/null 2>&1 || die "curl is required"

# Elevate the privileged steps (write /usr/local/bin, /etc, systemd) via sudo
# when not already root.
SUDO=""
if [ "$(id -u)" -ne 0 ]; then
  command -v sudo >/dev/null 2>&1 || die "need root (or sudo) to install the service"
  SUDO="sudo"
fi

# Whose config/identity the service owns. f2f keeps state in <home>/.f2f, and
# picks the home from SUDO_USER (else HOME). So to reuse the SAME identity as a
# person's interactive `sudo f2f` runs — and not mint a fresh keypair under
# /root — we tell the unit to run as if invoked by that user (SUDO_USER + HOME).
# Default: the invoking user (SUDO_USER when curl|sudo sh, else the current
# non-root user). Genuine root with no invoker → own state under /root.
if [ -z "$OWNER" ]; then
  if [ -n "${SUDO_USER:-}" ]; then
    OWNER="$SUDO_USER"
  elif [ "$(id -u)" -ne 0 ]; then
    OWNER="$(id -un)"
  else
    OWNER="root"
  fi
fi
if [ "$OWNER" = "root" ]; then
  OWNER_HOME="/root"
else
  OWNER_HOME="$(getent passwd "$OWNER" 2>/dev/null | cut -d: -f6)"
  [ -n "$OWNER_HOME" ] || die "can't resolve home for user '$OWNER' (use --user)"
fi
say "identity dir: ${OWNER_HOME}/.f2f/${CAMP}/identity (owned as ${OWNER})"

# --- detect target -----------------------------------------------------------

arch="$(uname -m)"
case "$arch" in
  x86_64|amd64)  arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) die "unsupported architecture: $arch" ;;
esac
asset="f2f-linux-${arch}"

if [ "$VERSION" = "latest" ]; then
  url="https://github.com/${REPO}/releases/latest/download/${asset}"
else
  url="https://github.com/${REPO}/releases/download/${VERSION}/${asset}"
fi

# --- download + install binary ----------------------------------------------

say "downloading $asset ($VERSION)..."
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
curl -4 -fSL --proto '=https' --connect-timeout 20 "$url" -o "$tmp" \
  || die "download failed: $url (does a release exist for $VERSION?)"
chmod +x "$tmp"

dest="${BIN_DIR%/}/f2f"
$SUDO mkdir -p "$BIN_DIR"
$SUDO mv "$tmp" "$dest"
$SUDO chmod +x "$dest"
trap - EXIT
say "installed: $dest ($("$dest" version 2>/dev/null || echo '?'))"

# --- write the root-only env file (holds the camp secret) --------------------

$SUDO mkdir -p "$ENV_DIR"
$SUDO chmod 700 "$ENV_DIR"
# Written atomically-ish via a temp then move; 0600 root so the camp_id secret
# is not world-readable.
tmpenv="$(mktemp)"
{
  printf 'F2F_CAMP=%s\n' "$CAMP"
  printf 'F2F_NAME=%s\n' "$NAME"
  printf 'F2F_LOG=%s\n' "$LOG"
} > "$tmpenv"
$SUDO mv "$tmpenv" "$ENV_FILE"
$SUDO chmod 600 "$ENV_FILE"
$SUDO chown root:root "$ENV_FILE" 2>/dev/null || true

# --- write the systemd unit --------------------------------------------------

# Run as root (tunnel/routes need it), but own state as $OWNER so the identity
# matches that user's interactive `sudo f2f` — SUDO_USER makes f2f store/chown
# config+identity under the user's ~/.f2f. Omitted when OWNER=root (defaults ok).
OWNER_ENV=""
if [ "$OWNER" != "root" ]; then
  OWNER_ENV="Environment=SUDO_USER=${OWNER}
Environment=HOME=${OWNER_HOME}"
fi

tmpunit="$(mktemp)"
cat > "$tmpunit" <<UNIT
[Unit]
Description=f2f service node (${NAME})
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=${ENV_FILE}
${OWNER_ENV}
ExecStart=${dest} --service
Restart=on-failure
RestartSec=5
# root: creating the tunnel + routes needs privileges.

[Install]
WantedBy=multi-user.target
UNIT
$SUDO mv "$tmpunit" "/etc/systemd/system/${UNIT}"
$SUDO chmod 644 "/etc/systemd/system/${UNIT}"

# --- enable + (re)start ------------------------------------------------------

$SUDO systemctl daemon-reload
$SUDO systemctl enable "$UNIT"
# restart, NOT `enable --now`: on a re-run the unit is already active, and start
# is then a no-op — the OLD unit keeps running and our new SUDO_USER/HOME env
# never takes effect. restart always re-reads the unit.
$SUDO systemctl restart "$UNIT"

say ""
say "f2f service '${NAME}' installed and started."
if [ "$OWNER" != "root" ]; then
  say "  identity: owned as ${OWNER} (state in ${OWNER_HOME}/.f2f) — same as your 'sudo f2f'"
else
  say "  identity: owned as root (state in /root/.f2f)"
fi
say "  status:  ${SUDO} systemctl status ${UNIT}"
say "  logs:    ${SUDO} journalctl -u ${UNIT} -f"
say "  manage:  f2f tui        (status, certs, domains, ports, channels, OIDC, logs, update)"
say "  camp_id: ${ENV_FILE} (root-only)"
