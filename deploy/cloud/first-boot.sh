#!/usr/bin/env bash
# first-boot.sh — user data for an byname cloud server (AWS Lightsail or Hetzner).
#
# OpenTofu (deploy/cloud/{aws,hetzner}) embeds this file in
# user-data.sh.tftpl, which cloud-init runs once, as root, on first boot. The
# wrapper runs this with bash and these variables in the environment, each
# validated by OpenTofu first:
#
#   BYNAME_NAME  BYNAME_TAG  BYNAME_ADMIN_USER  BYNAME_SSH_KEY  BYNAME_TS_AUTHKEY (may be empty)
#
# Nothing in this file is templated, so it stays plain, shellcheck-able bash.
#
# It does only what is needed to reach the server: a hostname, an admin user
# with your key, and, when BYNAME_TS_AUTHKEY is set, Tailscale joined with that
# one-off tagged key. Without a key the server doesn't use Tailscale, and you
# reach it with OpenSSH from the addresses the provider firewall allows.
# deploy/host/bootstrap.sh does the rest. Output:
# /var/log/cloud-init-output.log.
set -euo pipefail
# The wrapper ran with umask 077 to keep itself private. Files made here must
# be world-readable where Debian expects it: apt verifies signatures as the
# unprivileged _apt user and ignores a keyring it can't read.
umask 022
: "${BYNAME_NAME:?}" "${BYNAME_TAG:?}" "${BYNAME_ADMIN_USER:?}" "${BYNAME_SSH_KEY:?}"
BYNAME_TS_AUTHKEY="${BYNAME_TS_AUTHKEY:-}"

log() { printf 'byname first-boot: %s\n' "$*"; }
export DEBIAN_FRONTEND=noninteractive
# cloud-init or unattended-upgrades may hold the apt lock right after boot,
# and the network can take a moment: wait for the lock and retry.
apt_get() {
  local i
  for i in 1 2 3 4 5; do
    apt-get -o DPkg::Lock::Timeout=300 -qq "$@" && return 0
    log "apt-get $* failed (attempt $i); retrying"
    sleep 15
  done
  return 1
}

# ---------------------------------------------------------------- hostname
hostnamectl set-hostname "$BYNAME_NAME"
if grep -q '^127\.0\.1\.1' /etc/hosts; then
  sed -i "s/^127\.0\.1\.1.*/127.0.1.1\t$BYNAME_NAME/" /etc/hosts
else
  printf '127.0.1.1\t%s\n' "$BYNAME_NAME" >> /etc/hosts
fi
printf '# Set by byname first-boot.sh\npreserve_hostname: true\n' > /etc/cloud/cloud.cfg.d/99-byname-hostname.cfg
log "hostname $BYNAME_NAME"

# ---------------------------------------------------------------- packages
apt_get update
apt_get install -y curl ca-certificates sudo >/dev/null

# ---------------------------------------------------------------- admin user
# Lightsail's Debian image already has "admin"; Hetzner's has only root.
if ! id "$BYNAME_ADMIN_USER" >/dev/null 2>&1; then
  useradd --create-home --shell /bin/bash --groups sudo "$BYNAME_ADMIN_USER"
fi
home="$(getent passwd "$BYNAME_ADMIN_USER" | cut -d: -f6)"
install -d -m 0700 -o "$BYNAME_ADMIN_USER" -g "$BYNAME_ADMIN_USER" "$home/.ssh"
touch "$home/.ssh/authorized_keys"
grep -qxF "$BYNAME_SSH_KEY" "$home/.ssh/authorized_keys" || printf '%s\n' "$BYNAME_SSH_KEY" >> "$home/.ssh/authorized_keys"
chown "$BYNAME_ADMIN_USER:$BYNAME_ADMIN_USER" "$home/.ssh/authorized_keys"
chmod 0600 "$home/.ssh/authorized_keys"
# The account has no password (SSH keys and Tailscale SSH only), so sudo can't
# ask for one.
printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$BYNAME_ADMIN_USER" > /etc/sudoers.d/90-byname-admin
chmod 0440 /etc/sudoers.d/90-byname-admin
visudo -cqf /etc/sudoers.d/90-byname-admin
log "admin user $BYNAME_ADMIN_USER"

# ---------------------------------------------------------------- tailscale
if [[ -z "$BYNAME_TS_AUTHKEY" ]]; then
  log "no Tailscale auth key: not using Tailscale; next: deploy/host/bootstrap.sh --no-tailscale over SSH"
  exit 0
fi
# shellcheck source=/dev/null
. /etc/os-release
repo="https://pkgs.tailscale.com/stable/${ID}/${VERSION_CODENAME}"
curl -fsSL --retry 5 "$repo.noarmor.gpg" -o /usr/share/keyrings/tailscale-archive-keyring.gpg
curl -fsSL --retry 5 "$repo.tailscale-keyring.list" -o /etc/apt/sources.list.d/tailscale.list
chmod 0644 /usr/share/keyrings/tailscale-archive-keyring.gpg /etc/apt/sources.list.d/tailscale.list
apt_get update
apt_get install -y tailscale >/dev/null
systemctl enable --now tailscaled >/dev/null

# The key goes in a 0600 file on tmpfs, never on a command line (ps shows those).
keyfile="$(mktemp /run/byname-ts-authkey.XXXXXX)"
trap 'rm -f "$keyfile"' EXIT
printf '%s' "$BYNAME_TS_AUTHKEY" > "$keyfile"
for i in 1 2 3 4 5; do
  if tailscale up --auth-key="file:$keyfile" --hostname="$BYNAME_NAME" --advertise-tags="$BYNAME_TAG" --ssh; then
    break
  fi
  [[ $i -eq 5 ]] && { log "tailscale up failed; is the auth key valid, pre-approved and allowed to use $BYNAME_TAG?"; exit 1; }
  sleep 15
done
log "joined the tailnet as $BYNAME_NAME ($BYNAME_TAG); next: deploy/host/bootstrap.sh over Tailscale SSH"
