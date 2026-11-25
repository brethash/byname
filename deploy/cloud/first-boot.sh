#!/usr/bin/env bash
# first-boot.sh — user data for an ot cloud server (AWS Lightsail or Hetzner).
#
# OpenTofu (deploy/cloud/{aws,hetzner}) embeds this file in
# user-data.sh.tftpl, which cloud-init runs once, as root, on first boot. The
# wrapper runs this with bash and these variables in the environment, each
# validated by OpenTofu first:
#
#   OT_NAME  OT_TAG  OT_ADMIN_USER  OT_SSH_KEY  OT_TS_AUTHKEY
#
# Nothing in this file is templated, so it stays plain, shellcheck-able bash.
#
# It does only what is needed to reach the server with no public SSH port: a
# hostname, an admin user with your key, and Tailscale joined with a one-off
# tagged auth key. deploy/host/bootstrap.sh, run over the tailnet, does the
# rest. Output: /var/log/cloud-init-output.log.
set -euo pipefail
# The wrapper ran with umask 077 to keep itself private. Files made here must
# be world-readable where Debian expects it: apt verifies signatures as the
# unprivileged _apt user and ignores a keyring it can't read.
umask 022
: "${OT_NAME:?}" "${OT_TAG:?}" "${OT_ADMIN_USER:?}" "${OT_SSH_KEY:?}" "${OT_TS_AUTHKEY:?}"

log() { printf 'ot first-boot: %s\n' "$*"; }
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
hostnamectl set-hostname "$OT_NAME"
if grep -q '^127\.0\.1\.1' /etc/hosts; then
  sed -i "s/^127\.0\.1\.1.*/127.0.1.1\t$OT_NAME/" /etc/hosts
else
  printf '127.0.1.1\t%s\n' "$OT_NAME" >> /etc/hosts
fi
printf '# Set by ot first-boot.sh\npreserve_hostname: true\n' > /etc/cloud/cloud.cfg.d/99-ot-hostname.cfg
log "hostname $OT_NAME"

# ---------------------------------------------------------------- packages
apt_get update
apt_get install -y curl ca-certificates sudo >/dev/null

# ---------------------------------------------------------------- admin user
# Lightsail's Debian image already has "admin"; Hetzner's has only root.
if ! id "$OT_ADMIN_USER" >/dev/null 2>&1; then
  useradd --create-home --shell /bin/bash --groups sudo "$OT_ADMIN_USER"
fi
home="$(getent passwd "$OT_ADMIN_USER" | cut -d: -f6)"
install -d -m 0700 -o "$OT_ADMIN_USER" -g "$OT_ADMIN_USER" "$home/.ssh"
touch "$home/.ssh/authorized_keys"
grep -qxF "$OT_SSH_KEY" "$home/.ssh/authorized_keys" || printf '%s\n' "$OT_SSH_KEY" >> "$home/.ssh/authorized_keys"
chown "$OT_ADMIN_USER:$OT_ADMIN_USER" "$home/.ssh/authorized_keys"
chmod 0600 "$home/.ssh/authorized_keys"
# The account has no password (keys and Tailscale SSH only), so sudo can't ask
# for one.
printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$OT_ADMIN_USER" > /etc/sudoers.d/90-ot-admin
chmod 0440 /etc/sudoers.d/90-ot-admin
visudo -cqf /etc/sudoers.d/90-ot-admin
log "admin user $OT_ADMIN_USER"

# ---------------------------------------------------------------- tailscale
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
keyfile="$(mktemp /run/ot-ts-authkey.XXXXXX)"
trap 'rm -f "$keyfile"' EXIT
printf '%s' "$OT_TS_AUTHKEY" > "$keyfile"
for i in 1 2 3 4 5; do
  if tailscale up --auth-key="file:$keyfile" --hostname="$OT_NAME" --advertise-tags="$OT_TAG" --ssh; then
    break
  fi
  [[ $i -eq 5 ]] && { log "tailscale up failed; is the auth key valid, pre-approved and allowed to use $OT_TAG?"; exit 1; }
  sleep 15
done
log "joined the tailnet as $OT_NAME ($OT_TAG); next: deploy/host/bootstrap.sh over Tailscale SSH"
