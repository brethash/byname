#!/usr/bin/env bash
# bootstrap.sh — harden a fresh Raspberry Pi OS Lite (64-bit) install for ot.
#
# Run on the Pi, as root, from a copy of the repo's deploy/ directory:
#
#   sudo deploy/pi/bootstrap.sh --admin-user <you> --lan-subnet 192.168.1.0/24
#
# Safe to re-run: every step checks before it changes anything. It never
# runs `tailscale up` for you (that needs your browser); it prints the command.
set -euo pipefail

DEPLOY_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ADMIN_USER=""
LAN_SUBNET=""
HOSTNAME_WANT="ot"
FORCE_FIREWALL=0
REBOOT_NEEDED=0

usage() {
  cat <<'USAGE'
usage: sudo bootstrap.sh --admin-user USER --lan-subnet CIDR [--hostname ot] [--force-firewall]

  --admin-user      the account you log in with (must already have an SSH key)
  --lan-subnet      your home LAN, e.g. 192.168.1.0/24 (break-glass SSH is allowed from here)
  --hostname        machine and tailnet name (default: ot)
  --force-firewall  load the firewall even if your current SSH session is not
                    from the LAN subnet (you could lock yourself out)
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --admin-user) ADMIN_USER="${2:?}"; shift 2 ;;
    --lan-subnet) LAN_SUBNET="${2:?}"; shift 2 ;;
    --hostname) HOSTNAME_WANT="${2:?}"; shift 2 ;;
    --force-firewall) FORCE_FIREWALL=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage; exit 2 ;;
  esac
done

step() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
ok()   { printf '    \033[32mok\033[0m %s\n' "$*"; }
warn() { printf '    \033[33mwarn\033[0m %s\n' "$*"; }
die()  { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------- preflight
step "Preflight"
[[ $EUID -eq 0 ]] || die "run as root (sudo)"
[[ -n "$ADMIN_USER" && -n "$LAN_SUBNET" ]] || { usage; exit 2; }
[[ "$(uname -m)" == "aarch64" ]] || die "expected 64-bit Raspberry Pi OS (aarch64), got $(uname -m)"
# shellcheck source=/dev/null
. /etc/os-release
[[ -n "${VERSION_CODENAME:-}" ]] || die "cannot read VERSION_CODENAME from /etc/os-release"
id "$ADMIN_USER" >/dev/null 2>&1 || die "user $ADMIN_USER does not exist"
ADMIN_HOME="$(getent passwd "$ADMIN_USER" | cut -d: -f6)"
[[ -s "$ADMIN_HOME/.ssh/authorized_keys" ]] ||
  die "$ADMIN_HOME/.ssh/authorized_keys is empty; add your SSH key before password logins are disabled"
python3 - "$LAN_SUBNET" <<'PY' || die "--lan-subnet must be an IPv4 network like 192.168.1.0/24"
import ipaddress, sys
ipaddress.IPv4Network(sys.argv[1], strict=True)
PY
[[ "$HOSTNAME_WANT" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ ]] || die "invalid hostname: $HOSTNAME_WANT"
ok "$PRETTY_NAME on $(uname -m), admin user $ADMIN_USER, LAN $LAN_SUBNET"

# ---------------------------------------------------------------- hostname
step "Hostname"
if [[ "$(hostname)" != "$HOSTNAME_WANT" ]]; then
  hostnamectl set-hostname "$HOSTNAME_WANT"
  if grep -q '^127\.0\.1\.1' /etc/hosts; then
    sed -i "s/^127\.0\.1\.1.*/127.0.1.1\t$HOSTNAME_WANT/" /etc/hosts
  else
    printf '127.0.1.1\t%s\n' "$HOSTNAME_WANT" >> /etc/hosts
  fi
  ok "hostname set to $HOSTNAME_WANT"
else
  ok "hostname is $HOSTNAME_WANT"
fi

# ---------------------------------------------------------------- radios
step "Disable Wi-Fi and Bluetooth (wired Ethernet only)"
CONFIG_TXT=/boot/firmware/config.txt
[[ -f $CONFIG_TXT ]] || CONFIG_TXT=/boot/config.txt
missing=()
for line in "dtoverlay=disable-wifi" "dtoverlay=disable-bt" "gpu_mem=16"; do
  grep -qxF "$line" "$CONFIG_TXT" || missing+=("$line")
done
if [[ ${#missing[@]} -gt 0 ]]; then
  printf '\n[all]\n# Added by ot bootstrap.sh\n' >> "$CONFIG_TXT"
  printf '%s\n' "${missing[@]}" >> "$CONFIG_TXT"
  ok "added to $CONFIG_TXT: ${missing[*]}"
  REBOOT_NEEDED=1
else
  ok "$CONFIG_TXT already disables Wi-Fi and Bluetooth"
fi
systemctl disable --now hciuart.service 2>/dev/null || true

# ---------------------------------------------------------------- packages
step "Packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
purge=()
for pkg in avahi-daemon bluez triggerhappy rpi-connect rpi-connect-lite modemmanager cups cups-daemon; do
  if dpkg-query -W -f='${Status}' "$pkg" 2>/dev/null | grep -q 'install ok installed'; then
    purge+=("$pkg")
  fi
done
if [[ ${#purge[@]} -gt 0 ]]; then
  apt-get purge -y -qq "${purge[@]}"
  ok "removed: ${purge[*]}"
else
  ok "nothing to remove"
fi
apt-get install -y -qq unattended-upgrades apt-listchanges nftables sqlite3 curl ca-certificates >/dev/null
ok "installed unattended-upgrades, nftables, sqlite3, curl"

# ---------------------------------------------------------------- tailscale
step "Tailscale"
if ! command -v tailscale >/dev/null 2>&1; then
  repo="https://pkgs.tailscale.com/stable/${ID}/${VERSION_CODENAME}"
  curl -fsSL "$repo.noarmor.gpg" -o /usr/share/keyrings/tailscale-archive-keyring.gpg
  curl -fsSL "$repo.tailscale-keyring.list" -o /etc/apt/sources.list.d/tailscale.list
  apt-get update -qq
  apt-get install -y -qq tailscale >/dev/null
  ok "installed tailscale from $repo"
else
  ok "tailscale $(tailscale version | head -1) present"
fi
systemctl enable --now tailscaled >/dev/null 2>&1

# ---------------------------------------------------------------- ssh
step "OpenSSH (break-glass: key-only, LAN-only via firewall)"
# 01- sorts before any Imager/cloud-init drop-in; sshd keeps the first value.
cat > /etc/ssh/sshd_config.d/01-ot-hardening.conf <<EOF
# Managed by ot bootstrap.sh
PermitRootLogin no
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
AuthenticationMethods publickey
AllowUsers $ADMIN_USER
X11Forwarding no
MaxAuthTries 3
LoginGraceTime 20
EOF
sshd -t || die "sshd config test failed; not reloading"
systemctl reload ssh 2>/dev/null || systemctl reload sshd
ok "sshd: keys only, root login off, AllowUsers $ADMIN_USER"

# ---------------------------------------------------------------- updates
step "Unattended upgrades"
cat > /etc/apt/apt.conf.d/20auto-upgrades <<'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
APT::Periodic::AutocleanInterval "7";
EOF
cat > /etc/apt/apt.conf.d/52ot-unattended-upgrades <<'EOF'
// Managed by ot bootstrap.sh: Debian, Raspberry Pi and Tailscale updates.
Unattended-Upgrade::Origins-Pattern {
  "origin=Debian,codename=${distro_codename},label=Debian";
  "origin=Debian,codename=${distro_codename}-updates,label=Debian";
  "origin=Debian,codename=${distro_codename},label=Debian-Security";
  "origin=Debian,codename=${distro_codename}-security,label=Debian-Security";
  "origin=Raspberry Pi Foundation,codename=${distro_codename}";
  "origin=Tailscale";
};
Unattended-Upgrade::Remove-Unused-Dependencies "true";
Unattended-Upgrade::Automatic-Reboot "true";
Unattended-Upgrade::Automatic-Reboot-Time "04:00";
EOF
systemctl enable --now unattended-upgrades >/dev/null 2>&1 || true
ok "daily security + Pi + Tailscale updates, reboot at 04:00 when required"

# ---------------------------------------------------------------- sd wear
step "SD card wear"
if findmnt -no OPTIONS / | grep -qw noatime; then
  ok "root mounted noatime"
else
  warn "root is not mounted noatime; add noatime to / in /etc/fstab"
fi
if command -v dphys-swapfile >/dev/null 2>&1; then
  dphys-swapfile swapoff || true
  systemctl disable --now dphys-swapfile >/dev/null 2>&1 || true
  ok "disabled dphys-swapfile (file swap on SD)"
fi
if swapon --show=TYPE --noheadings 2>/dev/null | grep -qx file; then
  warn "a file-backed swap is still active: $(swapon --show=NAME --noheadings | tr '\n' ' ')"
else
  ok "no file-backed swap (zram is fine)"
fi
mkdir -p /etc/systemd/journald.conf.d
cat > /etc/systemd/journald.conf.d/10-ot-volatile.conf <<'EOF'
# Managed by ot bootstrap.sh: keep the journal in RAM. ot's audit trail lives
# in its database, not in logs.
[Journal]
Storage=volatile
RuntimeMaxUse=32M
EOF
systemctl restart systemd-journald
ok "journal kept in RAM (32 MB)"
systemctl enable --now systemd-timesyncd >/dev/null 2>&1 || warn "systemd-timesyncd unavailable; make sure time sync works (no RTC on the Pi)"

# ---------------------------------------------------------------- users/dirs
step "Users and directories"
if ! id ot >/dev/null 2>&1; then
  useradd --system --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin ot
fi
if ! id ot-backup >/dev/null 2>&1; then
  useradd --system --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin ot-backup
fi
usermod -aG ot ot-backup
install -d -m 0755 -o root -g root /usr/local/lib/ot /etc/ot
install -d -m 0700 -o root -g root /etc/credstore
ok "users ot, ot-backup (in group ot); /usr/local/lib/ot, /etc/ot, /etc/credstore"

# ---------------------------------------------------------------- config
step "ot configuration"
TS_SUFFIX="$(tailscale status --json 2>/dev/null | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("MagicDNSSuffix") or "")
except Exception: print("")' || true)"
if [[ -n "$TS_SUFFIX" ]]; then
  HOSTS="$HOSTNAME_WANT,$HOSTNAME_WANT.$TS_SUFFIX"
else
  HOSTS="$HOSTNAME_WANT"
fi
if [[ ! -f /etc/ot/ot.env ]] || grep -q '^OT_HOSTS=@\|^OT_HOSTS='"$HOSTNAME_WANT"'$' /etc/ot/ot.env; then
  sed "s|@OT_HOSTS@|$HOSTS|" "$DEPLOY_DIR/pi/ot.env.in" > /etc/ot/ot.env
  chmod 0644 /etc/ot/ot.env
  ok "wrote /etc/ot/ot.env (OT_HOSTS=$HOSTS)"
else
  ok "kept existing /etc/ot/ot.env ($(grep '^OT_HOSTS=' /etc/ot/ot.env))"
fi
[[ -n "$TS_SUFFIX" ]] || warn "tailnet not joined yet; re-run this script after 'tailscale up' to add the full tailnet name to OT_HOSTS"
install -m 0644 "$DEPLOY_DIR/pi/ot-backup.env.example" /etc/ot/ot-backup.env.example

# ---------------------------------------------------------------- firewall
step "Firewall (nftables, table inet ot_filter)"
sed "s|@LAN_SUBNET@|$LAN_SUBNET|" "$DEPLOY_DIR/pi/firewall.nft.in" > /etc/ot/firewall.nft.new
nft -c -f /etc/ot/firewall.nft.new || die "firewall rules failed to parse"
# Lockout guard: an OpenSSH session from outside the LAN subnet would survive
# (it is established) but you could not reconnect. sudo strips SSH_CLIENT, so
# look at the live connections instead. Tailscale SSH sessions never appear
# here (tailscaled terminates them itself).
if [[ $FORCE_FIREWALL -eq 0 ]]; then
  outside="$(ss -Htn state established '( sport = :22 )' | awk '{print $4}' | python3 -c '
import ipaddress, sys
lan = ipaddress.ip_network(sys.argv[1])
for line in sys.stdin:
    host = line.strip().rsplit(":", 1)[0].strip("[]")
    if not host:
        continue
    ip = ipaddress.ip_address(host)
    if getattr(ip, "ipv4_mapped", None):
        ip = ip.ipv4_mapped
    if not (ip.is_loopback or ip in lan if ip.version == 4 else ip.is_loopback):
        print(ip)
' "$LAN_SUBNET")"
  if [[ -n "$outside" ]]; then
    rm -f /etc/ot/firewall.nft.new
    die "SSH session(s) from outside $LAN_SUBNET: $outside — fix --lan-subnet, use Tailscale SSH, or pass --force-firewall"
  fi
fi
mv /etc/ot/firewall.nft.new /etc/ot/firewall.nft
# Debian's nftables.service flushes the whole ruleset (including tailscaled's).
systemctl disable --now nftables.service >/dev/null 2>&1 || true
install -m 0644 "$DEPLOY_DIR/systemd/ot-firewall.service" /etc/systemd/system/ot-firewall.service
systemctl daemon-reload
systemctl enable ot-firewall.service >/dev/null 2>&1
systemctl restart ot-firewall.service
ok "input policy drop; allowed: tcp/80 on tailscale0, tcp/22 from $LAN_SUBNET, udp/41641, ICMP, DHCP"

# ---------------------------------------------------------------- units
step "systemd units"
for unit in ot.service ot-backup.service ot-backup.timer; do
  install -m 0644 "$DEPLOY_DIR/systemd/$unit" "/etc/systemd/system/$unit"
done
install -m 0755 "$DEPLOY_DIR/pi/verify.sh" /usr/local/sbin/ot-verify
install -m 0755 "$DEPLOY_DIR/pi/restore.sh" /usr/local/sbin/ot-restore
systemctl daemon-reload
systemctl enable ot.service >/dev/null 2>&1
ok "ot.service enabled (starts once a binary is deployed)"
if [[ -f /etc/ot/ot-backup.env && -f /etc/credstore/ot-backup-aws ]]; then
  systemctl enable --now ot-backup.timer >/dev/null 2>&1
  ok "ot-backup.timer enabled"
else
  ok "backups not configured yet (see docs/operations.md, 'Backups')"
fi
ok "installed ot-verify and ot-restore to /usr/local/sbin"

# ---------------------------------------------------------------- summary
step "Next steps"
if ! tailscale status >/dev/null 2>&1; then
  cat <<EOF
    1. Join the tailnet (opens a login URL; your policy must define tag:ot first):
         sudo tailscale up --ssh --hostname=$HOSTNAME_WANT --advertise-tags=tag:ot
    2. Re-run this script so OT_HOSTS includes the full tailnet name.
EOF
fi
[[ $REBOOT_NEEDED -eq 1 ]] && echo "    * Reboot to apply config.txt changes: sudo reboot"
echo "    * Deploy from your Mac:  make deploy PI=$ADMIN_USER@$HOSTNAME_WANT"
echo "    * Then check everything: sudo ot-verify"
