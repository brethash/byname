#!/usr/bin/env bash
# bootstrap.sh — harden a fresh Debian host for ot: a Raspberry Pi at home, or
# a small cloud server (AWS Lightsail, Hetzner) that deploy/cloud created.
#
# Run on the host, as root, from a copy of the repo's deploy/ directory:
#
#   Pi:     sudo deploy/host/bootstrap.sh --admin-user <you> --lan-subnet 192.168.1.0/24 \
#                [--name ot] [--lan-access --lan-domain lan]
#   cloud:  sudo deploy/host/bootstrap.sh --admin-user admin [--name ot]
#
# Safe to re-run: every step checks before it changes anything. On a Pi it
# never runs `tailscale up` for you (that needs your browser); it prints the
# command. Cloud servers joined the tailnet on first boot.
set -euo pipefail

DEPLOY_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TAILNET_NETS="100.64.0.0/10 fd7a:115c:a1e0::/48"
ADMIN_USER=""
NAME="ot"
TAG=""
PLATFORM=""
LAN_SUBNET=""
LAN_ACCESS=0
LAN_DOMAIN=""
FORCE_FIREWALL=0
REBOOT_NEEDED=0

usage() {
  cat <<'USAGE'
usage: sudo bootstrap.sh --admin-user USER [--name NAME] [--tag tag:NAME] [--platform pi|cloud]
                         [--lan-subnet CIDR [--lan-access] [--lan-domain DOMAIN]] [--force-firewall]

  --admin-user      the account you log in with (must already have an SSH key)
  --name            the service name people type, e.g. ot for ot/wiki (default: ot).
                    Also the machine name and the tailnet (MagicDNS) name.
  --tag             Tailscale tag for this host (default: tag:NAME); your tailnet
                    policy must define it
  --platform        pi or cloud (default: detected; a Raspberry Pi is "pi")

Raspberry Pi only:
  --lan-subnet      your home LAN, e.g. 192.168.1.0/24 (break-glass SSH is allowed from here)
  --lan-access      also serve NAME to LAN devices that don't run Tailscale (plain HTTP on
                    the LAN; their changes are recorded by IP address). Re-run without it
                    to turn LAN access off again.
  --lan-domain      your router's local domain (e.g. lan, home, home.arpa), so NAME.DOMAIN
                    is accepted as well as the bare NAME
  --force-firewall  load the firewall even if your current OpenSSH session would not be
                    allowed by it (you could lock yourself out)
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --admin-user) ADMIN_USER="${2:?}"; shift 2 ;;
    --name|--hostname) NAME="${2:?}"; shift 2 ;;
    --tag) TAG="${2:?}"; shift 2 ;;
    --platform) PLATFORM="${2:?}"; shift 2 ;;
    --lan-subnet) LAN_SUBNET="${2:?}"; shift 2 ;;
    --lan-access) LAN_ACCESS=1; shift ;;
    --lan-domain) LAN_DOMAIN="${2:?}"; shift 2 ;;
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
[[ -n "$ADMIN_USER" ]] || { usage; exit 2; }
if [[ -z "$PLATFORM" ]]; then
  if tr -d '\0' < /proc/device-tree/model 2>/dev/null | grep -q 'Raspberry Pi'; then PLATFORM=pi; else PLATFORM=cloud; fi
fi
case "$PLATFORM" in
  pi)
    [[ -n "$LAN_SUBNET" ]] || die "--lan-subnet is required on a Raspberry Pi"
    [[ "$(uname -m)" == "aarch64" ]] || die "expected 64-bit Raspberry Pi OS (aarch64), got $(uname -m)"
    ;;
  cloud)
    [[ -z "$LAN_SUBNET" && $LAN_ACCESS -eq 0 && -z "$LAN_DOMAIN" ]] ||
      die "--lan-subnet, --lan-access and --lan-domain are for a Pi on your home network; a cloud server is tailnet-only"
    [[ "$(uname -m)" =~ ^(aarch64|x86_64)$ ]] || die "unsupported architecture $(uname -m)"
    ;;
  *) die "--platform must be pi or cloud" ;;
esac
# shellcheck source=/dev/null
. /etc/os-release
[[ "${ID:-}" == "debian" ]] || die "expected Debian or Raspberry Pi OS, got ${PRETTY_NAME:-unknown}"
[[ -n "${VERSION_CODENAME:-}" ]] || die "cannot read VERSION_CODENAME from /etc/os-release"
id "$ADMIN_USER" >/dev/null 2>&1 || die "user $ADMIN_USER does not exist"
ADMIN_HOME="$(getent passwd "$ADMIN_USER" | cut -d: -f6)"
[[ -s "$ADMIN_HOME/.ssh/authorized_keys" ]] ||
  die "$ADMIN_HOME/.ssh/authorized_keys is empty; add your SSH key before password logins are disabled"
# A DNS label that starts with a letter: it is typed into address bars as
# NAME/..., and becomes the hostname and the tailnet name.
[[ "$NAME" =~ ^[a-z]([a-z0-9-]{0,30}[a-z0-9])?$ ]] ||
  die "invalid --name '$NAME': use 1-32 lower-case letters, digits or dashes, starting with a letter"
[[ "$NAME" != "localhost" ]] || die "--name localhost would shadow this machine's loopback name"
[[ -n "$TAG" ]] || TAG="tag:$NAME"
[[ "$TAG" =~ ^tag:[a-z0-9]([a-z0-9-]*[a-z0-9])?$ ]] || die "invalid --tag '$TAG' (want tag:something)"
if [[ "$PLATFORM" == pi ]]; then
  python3 - "$LAN_SUBNET" <<'PY' || die "--lan-subnet must be an IPv4 network like 192.168.1.0/24"
import ipaddress, sys
ipaddress.IPv4Network(sys.argv[1], strict=True)
PY
  [[ -z "$LAN_DOMAIN" || "$LAN_DOMAIN" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ ]] || die "invalid --lan-domain: $LAN_DOMAIN"
  [[ "$LAN_DOMAIN" == "local" ]] && die "--lan-domain local is mDNS, not router DNS; use your router's domain (lan, home, home.arpa…)"
  ok "$PRETTY_NAME on $(uname -m) (Raspberry Pi), service $NAME, admin $ADMIN_USER, LAN $LAN_SUBNET, LAN access $([[ $LAN_ACCESS -eq 1 ]] && echo on || echo off)"
else
  ok "$PRETTY_NAME on $(uname -m) (cloud), service $NAME, admin $ADMIN_USER, tailnet only"
fi

# ---------------------------------------------------------------- hostname
step "Hostname"
if [[ "$(hostname)" != "$NAME" ]]; then
  hostnamectl set-hostname "$NAME"
  ok "hostname set to $NAME"
else
  ok "hostname is $NAME"
fi
if grep -q '^127\.0\.1\.1' /etc/hosts; then
  sed -i "s/^127\.0\.1\.1.*/127.0.1.1\t$NAME/" /etc/hosts
else
  printf '127.0.1.1\t%s\n' "$NAME" >> /etc/hosts
fi
if [[ -d /etc/cloud/cloud.cfg.d ]]; then
  # Stop cloud-init from putting the provider's hostname back on reboot.
  printf '# Managed by ot bootstrap.sh\npreserve_hostname: true\n' > /etc/cloud/cloud.cfg.d/99-ot-hostname.cfg
fi

# ---------------------------------------------------------------- radios
if [[ "$PLATFORM" == pi ]]; then
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
fi

# ---------------------------------------------------------------- packages
step "Packages"
export DEBIAN_FRONTEND=noninteractive
apt-get -o DPkg::Lock::Timeout=300 update -qq
purge=()
for pkg in avahi-daemon bluez triggerhappy rpi-connect rpi-connect-lite modemmanager cups cups-daemon; do
  if dpkg-query -W -f='${Status}' "$pkg" 2>/dev/null | grep -q 'install ok installed'; then
    purge+=("$pkg")
  fi
done
if [[ ${#purge[@]} -gt 0 ]]; then
  apt-get -o DPkg::Lock::Timeout=300 purge -y -qq "${purge[@]}"
  ok "removed: ${purge[*]}"
else
  ok "nothing to remove"
fi
apt-get -o DPkg::Lock::Timeout=300 install -y -qq unattended-upgrades apt-listchanges nftables sqlite3 curl ca-certificates python3 >/dev/null
ok "installed unattended-upgrades, nftables, sqlite3, curl"

# ---------------------------------------------------------------- tailscale
step "Tailscale"
if ! command -v tailscale >/dev/null 2>&1; then
  repo="https://pkgs.tailscale.com/stable/${ID}/${VERSION_CODENAME}"
  curl -fsSL "$repo.noarmor.gpg" -o /usr/share/keyrings/tailscale-archive-keyring.gpg
  curl -fsSL "$repo.tailscale-keyring.list" -o /etc/apt/sources.list.d/tailscale.list
  apt-get -o DPkg::Lock::Timeout=300 update -qq
  apt-get -o DPkg::Lock::Timeout=300 install -y -qq tailscale >/dev/null
  ok "installed tailscale from $repo"
else
  ok "tailscale $(tailscale version | head -1) present"
fi
systemctl enable --now tailscaled >/dev/null 2>&1

# ---------------------------------------------------------------- ssh
if [[ "$PLATFORM" == pi ]]; then
  step "OpenSSH (break-glass: key-only, LAN-only via firewall)"
else
  step "OpenSSH (fallback to Tailscale SSH: key-only, tailnet-only via firewall)"
fi
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
ok "daily Debian security, Raspberry Pi and Tailscale updates, reboot at 04:00 when required"

# ---------------------------------------------------------------- storage
mkdir -p /etc/systemd/journald.conf.d
rm -f /etc/systemd/journald.conf.d/10-ot-volatile.conf # older name
if [[ "$PLATFORM" == pi ]]; then
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
  cat > /etc/systemd/journald.conf.d/10-ot-journal.conf <<'EOF'
# Managed by ot bootstrap.sh: keep the journal in RAM to spare the SD card.
# ot's audit trail lives in its database, not in logs.
[Journal]
Storage=volatile
RuntimeMaxUse=32M
EOF
  systemctl restart systemd-journald
  ok "journal kept in RAM (32 MB)"
  systemctl enable --now systemd-timesyncd >/dev/null 2>&1 || warn "systemd-timesyncd unavailable; make sure time sync works (no RTC on the Pi)"
else
  step "Memory and logs"
  mem_kb="$(awk '/^MemTotal:/ {print $2}' /proc/meminfo)"
  if [[ -z "$(swapon --show --noheadings 2>/dev/null)" && "$mem_kb" -lt 1500000 ]]; then
    # 512 MB servers: headroom for apt and unattended-upgrades, not for ot.
    fallocate -l 512M /swapfile
    chmod 0600 /swapfile
    mkswap -q /swapfile
    swapon /swapfile
    grep -q '^/swapfile ' /etc/fstab || printf '/swapfile none swap sw 0 0\n' >> /etc/fstab
    ok "added a 512 MB swap file ($((mem_kb / 1024)) MB RAM)"
  else
    ok "swap: $(swapon --show=NAME,SIZE --noheadings 2>/dev/null | tr '\n' ' ' || true)${mem_kb:+ ($((mem_kb / 1024)) MB RAM)}"
  fi
  cat > /etc/systemd/journald.conf.d/10-ot-journal.conf <<'EOF'
# Managed by ot bootstrap.sh: keep logs across reboots, capped.
[Journal]
Storage=persistent
SystemMaxUse=200M
EOF
  systemctl restart systemd-journald
  ok "journal kept on disk, capped at 200 MB"
fi

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
step "Service configuration"
TS_SUFFIX="$(tailscale status --json 2>/dev/null | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("MagicDNSSuffix") or "")
except Exception: print("")' || true)"
HOSTS="$NAME"
[[ -n "$TS_SUFFIX" ]] && HOSTS+=",$NAME.$TS_SUFFIX"
[[ -n "$LAN_DOMAIN" ]] && HOSTS+=",$NAME.$LAN_DOMAIN"
if [[ $LAN_ACCESS -eq 1 ]]; then
  # Also accept the Pi's own LAN address as a Host name (http://192.168.1.x/),
  # handy before router DNS is set up. Safe against DNS rebinding: a hostile
  # page can't make a browser send this Host. Reserve the address in DHCP.
  LAN_IP="$(ip -4 -o addr show scope global | awk '$2 !~ /^tailscale/ {print $4}' | cut -d/ -f1 | head -1)"
  if [[ -n "$LAN_IP" ]] && python3 -c 'import ipaddress,sys; sys.exit(ipaddress.ip_address(sys.argv[1]) not in ipaddress.ip_network(sys.argv[2]))' "$LAN_IP" "$LAN_SUBNET"; then
    HOSTS+=",$LAN_IP"
  else
    warn "could not find this Pi's address inside $LAN_SUBNET; LAN devices must use the name"
  fi
fi
ALLOW="100.64.0.0/10,fd7a:115c:a1e0::/48,127.0.0.1/32,::1/128"
[[ $LAN_ACCESS -eq 1 ]] && ALLOW+=",$LAN_SUBNET"
# ot.env is fully managed (rewritten on every run); local overrides go in
# ot.local.env, which ot.service reads afterwards.
sed -e "s|@OT_HOSTS@|$HOSTS|" -e "s|@OT_ALLOW_CIDRS@|$ALLOW|" "$DEPLOY_DIR/host/ot.env.in" > /etc/ot/ot.env
chmod 0644 /etc/ot/ot.env
[[ -f /etc/ot/ot.local.env ]] || printf '# Local overrides for /etc/ot/ot.env (same OT_* variables). Kept across bootstrap runs.\n' > /etc/ot/ot.local.env
# What this host is, for ot-verify. Not read by ot itself.
cat > /etc/ot/host.conf <<EOF
# Managed by ot bootstrap.sh; read by ot-verify.
PLATFORM=$PLATFORM
NAME=$NAME
TAG=$TAG
LAN_SUBNET=$LAN_SUBNET
EOF
chmod 0644 /etc/ot/host.conf
ok "wrote /etc/ot/ot.env (OT_HOSTS=$HOSTS)"
ok "allowed peers: $ALLOW"
[[ -n "$TS_SUFFIX" ]] || warn "tailnet not joined yet; re-run this script after 'tailscale up' to add the full tailnet name to OT_HOSTS"
# systemd's IP filter for ot.service only allows the tailnet and localhost;
# LAN access adds the LAN subnet in a drop-in.
install -d -m 0755 /etc/systemd/system/ot.service.d
if [[ $LAN_ACCESS -eq 1 ]]; then
  printf '# Managed by ot bootstrap.sh --lan-access\n[Service]\nIPAddressAllow=%s\n' "$LAN_SUBNET" \
    > /etc/systemd/system/ot.service.d/10-lan-access.conf
  ok "ot.service accepts $LAN_SUBNET (drop-in 10-lan-access.conf)"
else
  rm -f /etc/systemd/system/ot.service.d/10-lan-access.conf
fi
install -m 0644 "$DEPLOY_DIR/host/ot-backup.env.example" /etc/ot/ot-backup.env.example

# ---------------------------------------------------------------- firewall
step "Firewall (nftables, table inet ot_filter)"
if [[ $LAN_ACCESS -eq 1 ]]; then
  LAN_HTTP_RULE="ip saddr $LAN_SUBNET tcp dport 80 accept"
else
  LAN_HTTP_RULE="# (no LAN HTTP access; on a Pi, bootstrap.sh --lan-access turns it on)"
fi
if [[ "$PLATFORM" == pi ]]; then
  SSH_RULE="ip saddr $LAN_SUBNET tcp dport 22 accept"
  SSH_NETS="$LAN_SUBNET"
else
  SSH_RULE='iifname "tailscale0" tcp dport 22 accept'
  SSH_NETS="$TAILNET_NETS"
fi
sed -e "s|@SSH_RULE@|$SSH_RULE|" -e "s|@LAN_HTTP_RULE@|$LAN_HTTP_RULE|" \
  "$DEPLOY_DIR/host/firewall.nft.in" > /etc/ot/firewall.nft.new
nft -c -f /etc/ot/firewall.nft.new || die "firewall rules failed to parse"
# Lockout guard: an OpenSSH session from an address the new rules don't allow
# would survive (it is established) but you could not reconnect. sudo strips
# SSH_CLIENT, so look at the live connections instead. Tailscale SSH sessions
# never appear here (tailscaled terminates them itself).
if [[ $FORCE_FIREWALL -eq 0 ]]; then
  # shellcheck disable=SC2086 # SSH_NETS is a deliberate word list
  outside="$(ss -Htn state established '( sport = :22 )' | awk '{print $4}' | python3 -c '
import ipaddress, sys
nets = [ipaddress.ip_network(n) for n in sys.argv[1:]]
for line in sys.stdin:
    host = line.strip().rsplit(":", 1)[0].strip("[]").split("%")[0]
    if not host:
        continue
    ip = ipaddress.ip_address(host)
    if getattr(ip, "ipv4_mapped", None):
        ip = ip.ipv4_mapped
    if not (ip.is_loopback or any(ip in n for n in nets if n.version == ip.version)):
        print(ip)
' $SSH_NETS)"
  if [[ -n "$outside" ]]; then
    rm -f /etc/ot/firewall.nft.new
    die "OpenSSH session(s) from outside ${SSH_NETS// /, }: $outside — fix --lan-subnet, use Tailscale SSH, or pass --force-firewall"
  fi
fi
mv /etc/ot/firewall.nft.new /etc/ot/firewall.nft
# Debian's nftables.service flushes the whole ruleset (including tailscaled's).
systemctl disable --now nftables.service >/dev/null 2>&1 || true
install -m 0644 "$DEPLOY_DIR/systemd/ot-firewall.service" /etc/systemd/system/ot-firewall.service
systemctl daemon-reload
systemctl enable ot-firewall.service >/dev/null 2>&1
systemctl restart ot-firewall.service
if [[ "$PLATFORM" == cloud ]]; then
  ok "input policy drop; allowed: tcp/80 and tcp/22 on tailscale0, udp/41641, ICMP, DHCP"
elif [[ $LAN_ACCESS -eq 1 ]]; then
  ok "input policy drop; allowed: tcp/80 on tailscale0 and from $LAN_SUBNET, tcp/22 from $LAN_SUBNET, udp/41641, ICMP, DHCP"
else
  ok "input policy drop; allowed: tcp/80 on tailscale0, tcp/22 from $LAN_SUBNET, udp/41641, ICMP, DHCP"
fi

# ---------------------------------------------------------------- units
step "systemd units"
for unit in ot.service ot-backup.service ot-backup.timer; do
  install -m 0644 "$DEPLOY_DIR/systemd/$unit" "/etc/systemd/system/$unit"
done
install -m 0755 "$DEPLOY_DIR/host/verify.sh" /usr/local/sbin/ot-verify
install -m 0755 "$DEPLOY_DIR/host/restore.sh" /usr/local/sbin/ot-restore
systemctl daemon-reload
systemctl enable ot.service >/dev/null 2>&1
# Pick up env/drop-in changes if ot is already running.
systemctl try-restart ot.service
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
  if [[ "$PLATFORM" == pi ]]; then
    cat <<EOF
    1. Join the tailnet (opens a login URL; your policy must define $TAG first):
         sudo tailscale up --ssh --hostname=$NAME --advertise-tags=$TAG
    2. Re-run this script so OT_HOSTS includes the full tailnet name.
EOF
  else
    warn "Tailscale is not connected; first boot should have joined it (see /var/log/cloud-init-output.log)"
  fi
fi
[[ $REBOOT_NEEDED -eq 1 ]] && echo "    * Reboot to apply config.txt changes: sudo reboot"
echo "    * Deploy from your Mac:  make deploy TARGET=$ADMIN_USER@$NAME"
echo "    * Then check everything: sudo ot-verify"
