#!/usr/bin/env bash
# bootstrap.sh — harden a fresh Debian host for byname: a Raspberry Pi at home, or
# a small cloud server (AWS Lightsail, Hetzner) that deploy/cloud created.
#
# Run on the host, as root, from a copy of the repo's deploy/ directory:
#
#   Pi:     sudo deploy/host/bootstrap.sh --admin-user <you> --lan-subnet 192.168.1.0/24 \
#                [--name me] [--lan-access --lan-domain lan]
#   cloud:  sudo deploy/host/bootstrap.sh --admin-user admin [--name me]
#   cloud without Tailscale:
#           sudo deploy/host/bootstrap.sh --admin-user admin --name hg --no-tailscale \
#                --ssh-from 203.0.113.7/32 --public-domain me.example.com
#
# Public access, with Google or Slack sign-in (docs/public-access.md):
# --public-domain serves your own domain directly; --funnel publishes the
# tailnet name through Tailscale Funnel. Both work on a Pi as well as in the
# cloud (a Pi also needs TCP 80 and 443 forwarded from the router for
# --public-domain).
#
# Safe to re-run: every step checks before it changes anything, and each run
# applies exactly the flags given (leave one out to turn it off). On a Pi it
# never runs `tailscale up` for you (that needs your browser); it prints the
# command. Cloud servers with Tailscale joined the tailnet on first boot.
set -euo pipefail

DEPLOY_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TAILNET_NETS="100.64.0.0/10 fd7a:115c:a1e0::/48"
ADMIN_USER=""
SVC_NAME="me" # --name; not NAME, which /etc/os-release sets
TAG=""
PLATFORM=""
LAN_SUBNET=""
LAN_ACCESS=0
LAN_DOMAIN=""
FORCE_FIREWALL=0
TAILSCALE=1
SSH_FROM=""
FUNNEL=0
PUBLIC_DOMAIN=""
PUBLIC_SOCK=/run/byname/public.sock
REBOOT_NEEDED=0

usage() {
  cat <<'USAGE'
usage: sudo bootstrap.sh --admin-user USER [--name NAME] [--platform pi|cloud]
                         [--tag tag:NAME | --no-tailscale] [--ssh-from CIDR[,CIDR...]]
                         [--lan-subnet CIDR [--lan-access] [--lan-domain DOMAIN]]
                         [--public-domain DOMAIN | --funnel] [--force-firewall]

  --admin-user      the account you log in with (must already have an SSH key)
  --name            the service name people type, e.g. me for me/wiki (default: me).
                    Also the machine name and the tailnet (MagicDNS) name.
  --tag             Tailscale tag for this host (default: tag:NAME); your tailnet
                    policy must define it
  --platform        pi or cloud (default: detected; a Raspberry Pi is "pi")
  --no-tailscale    don't use Tailscale: no tailnet access and no Tailscale SSH. On a cloud
                    server you then need --ssh-from, and --public-domain so people can use it
  --ssh-from        where OpenSSH may be reached from on the internet (cloud): addresses like
                    203.0.113.7/32 (comma-separated), or "any" to leave the address filter to
                    the provider's firewall (ssh_cidrs), which you can change from your Mac.
                    A specific address here locks you out if your address changes.

Public access (Google or Slack sign-in must be set up first: docs/public-access.md):
  --public-domain   serve your own domain, e.g. me.example.com, over HTTPS with a Let's
                    Encrypt certificate. Opens TCP 80 and 443; its DNS must point here.
                    Everyone reaching it signs in (tailnet devices can still use NAME/).
                    On a Pi, also forward TCP 80 and 443 from your router to the Pi.
  --funnel          publish https://NAME.<tailnet>.ts.net through Tailscale Funnel instead:
                    no open ports; tailnet devices skip sign-in, everyone else signs in.
                    On a Pi it needs no router changes (works behind carrier-grade NAT).

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
    --name|--hostname) SVC_NAME="${2:?}"; shift 2 ;;
    --tag) TAG="${2:?}"; shift 2 ;;
    --platform) PLATFORM="${2:?}"; shift 2 ;;
    --lan-subnet) LAN_SUBNET="${2:?}"; shift 2 ;;
    --lan-access) LAN_ACCESS=1; shift ;;
    --lan-domain) LAN_DOMAIN="${2:?}"; shift 2 ;;
    --force-firewall) FORCE_FIREWALL=1; shift ;;
    --no-tailscale) TAILSCALE=0; shift ;;
    --ssh-from) SSH_FROM="${2:?}"; shift 2 ;;
    --funnel) FUNNEL=1; shift ;;
    --public-domain) PUBLIC_DOMAIN="${2:?}"; shift 2 ;;
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
# /etc/os-release sets NAME, VERSION, ID and more: read the fields needed in a
# subshell, so none of them can overwrite this script's variables.
os_release() { (
  # shellcheck source=/dev/null
  . /etc/os-release && printf '%s' "${!1:-}"
) 2>/dev/null; }
OS_ID="$(os_release ID || true)"
OS_CODENAME="$(os_release VERSION_CODENAME || true)"
OS_PRETTY="$(os_release PRETTY_NAME || true)"
[[ "$OS_ID" == "debian" ]] || die "expected Debian or Raspberry Pi OS, got ${OS_PRETTY:-unknown}"
[[ -n "$OS_CODENAME" ]] || die "cannot read VERSION_CODENAME from /etc/os-release"
id "$ADMIN_USER" >/dev/null 2>&1 || die "user $ADMIN_USER does not exist"
ADMIN_HOME="$(getent passwd "$ADMIN_USER" | cut -d: -f6)"
[[ -s "$ADMIN_HOME/.ssh/authorized_keys" ]] ||
  die "$ADMIN_HOME/.ssh/authorized_keys is empty; add your SSH key before password logins are disabled"
# A DNS label that starts with a letter: it is typed into address bars as
# NAME/..., and becomes the hostname and the tailnet name.
[[ "$SVC_NAME" =~ ^[a-z]([a-z0-9-]{0,30}[a-z0-9])?$ ]] ||
  die "invalid --name '$SVC_NAME': use 1-32 lower-case letters, digits or dashes, starting with a letter"
[[ "$SVC_NAME" != "localhost" ]] || die "--name localhost would shadow this machine's loopback name"
[[ -n "$TAG" ]] || TAG="tag:$SVC_NAME"
[[ "$TAG" =~ ^tag:[a-z0-9]([a-z0-9-]*[a-z0-9])?$ ]] || die "invalid --tag '$TAG' (want tag:something)"
PUBLIC=0
[[ $FUNNEL -eq 1 || -n "$PUBLIC_DOMAIN" ]] && PUBLIC=1
[[ $FUNNEL -eq 1 && -n "$PUBLIC_DOMAIN" ]] && die "choose one of --funnel and --public-domain"
[[ $FUNNEL -eq 1 && $TAILSCALE -eq 0 ]] && die "--funnel needs Tailscale; use --public-domain without it"
if [[ -n "$PUBLIC_DOMAIN" ]]; then
  [[ "$PUBLIC_DOMAIN" =~ ^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$ ]] ||
    die "--public-domain must be a lower-case DNS name like me.example.com (no https://, no path)"
  [[ "$PUBLIC_DOMAIN" != *.ts.net ]] || die "--public-domain can't be a ts.net name; use --funnel for that"
fi
if [[ -n "$SSH_FROM" && "$SSH_FROM" != any ]]; then
  python3 - "$SSH_FROM" <<'PY' || die "--ssh-from must be \"any\" or comma-separated networks like 203.0.113.7/32, no wider than /16 (IPv4) or /48 (IPv6)"
import ipaddress, sys
for c in sys.argv[1].split(","):
    n = ipaddress.ip_network(c.strip(), strict=True)
    if n.prefixlen < (16 if n.version == 4 else 48):
        sys.exit(1)
PY
fi
[[ "$SSH_FROM" == any && "$PLATFORM" == pi ]] &&
  die "--ssh-from any is for cloud servers, whose provider firewall filters SSH; a Pi has none (it allows SSH from --lan-subnet)"
if [[ $TAILSCALE -eq 0 && "$PLATFORM" == cloud ]]; then
  [[ -n "$SSH_FROM" ]] || die "without Tailscale, --ssh-from is required so you can still reach this server over SSH (\"any\" leaves the address filter to the provider's firewall)"
  # Sign-in has to be configured on the server before --public-domain, so a
  # first run without it is expected.
  [[ -n "$PUBLIC_DOMAIN" ]] || warn "no --public-domain yet: nobody can use the service until you add it (after setting up sign-in)"
fi
if [[ $TAILSCALE -eq 0 ]] && systemctl is-active --quiet tailscaled 2>/dev/null; then
  # Tailscale SSH is answered by tailscaled itself, past the host firewall,
  # so leaving it running would leave tailnet SSH open. Stopping it here could
  # cut off the session running this script.
  die "--no-tailscale, but tailscaled is running (Tailscale SSH bypasses the firewall). From an OpenSSH session, run: sudo tailscale down; sudo systemctl disable --now tailscaled. Then re-run"
fi
if [[ $TAILSCALE -eq 0 && "$PLATFORM" == pi && $LAN_ACCESS -eq 0 && -z "$PUBLIC_DOMAIN" ]]; then
  die "without Tailscale, a Pi needs --lan-access or --public-domain: otherwise nobody can use the service"
fi
if [[ $PUBLIC -eq 1 ]]; then
  # Sign-in must be configured before the public address opens: byname refuses
  # to start with a public address and no way to sign in.
  sysv="$(systemctl --version | awk 'NR==1 {print $2}')"
  [[ "$sysv" =~ ^[0-9]+$ && "$sysv" -ge 254 ]] || die "public access needs systemd 254 or later (ImportCredential); this is ${sysv:-unknown}"
  SIGNIN=""
  for p in google slack; do
    P="${p^^}"
    grep -Eq "^BYNAME_${P}_CLIENT_ID=.+" /etc/byname/byname.local.env 2>/dev/null || continue
    [[ -s "/etc/credstore/byname.$p-client-secret" ]] ||
      die "BYNAME_${P}_CLIENT_ID is set in /etc/byname/byname.local.env, but /etc/credstore/byname.$p-client-secret is missing or empty"
    if [[ $p == google ]]; then list=BYNAME_GOOGLE_ALLOW; else list=BYNAME_SLACK_TEAMS; fi
    grep -Eq "^$list=.+" /etc/byname/byname.local.env || die "BYNAME_${P}_CLIENT_ID is set but $list is empty: say who may sign in"
    SIGNIN+=" $p"
  done
  [[ -n "$SIGNIN" ]] || die "public access needs Google or Slack sign-in in /etc/byname/byname.local.env first (docs/public-access.md)"
fi
if [[ "$PLATFORM" == pi ]]; then
  python3 - "$LAN_SUBNET" <<'PY' || die "--lan-subnet must be an IPv4 network like 192.168.1.0/24"
import ipaddress, sys
ipaddress.IPv4Network(sys.argv[1], strict=True)
PY
  [[ -z "$LAN_DOMAIN" || "$LAN_DOMAIN" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ ]] || die "invalid --lan-domain: $LAN_DOMAIN"
  [[ "$LAN_DOMAIN" == "local" ]] && die "--lan-domain local is mDNS, not router DNS; use your router's domain (lan, home, home.arpa…)"
  ok "$OS_PRETTY on $(uname -m) (Raspberry Pi), service $SVC_NAME, admin $ADMIN_USER, LAN $LAN_SUBNET, LAN access $([[ $LAN_ACCESS -eq 1 ]] && echo on || echo off)"
else
  ok "$OS_PRETTY on $(uname -m) (cloud), service $SVC_NAME, admin $ADMIN_USER, Tailscale $([[ $TAILSCALE -eq 1 ]] && echo on || echo off)"
fi

# ---------------------------------------------------------------- hostname
step "Hostname"
if [[ "$(hostname)" != "$SVC_NAME" ]]; then
  hostnamectl set-hostname "$SVC_NAME"
  ok "hostname set to $SVC_NAME"
else
  ok "hostname is $SVC_NAME"
fi
if grep -q '^127\.0\.1\.1' /etc/hosts; then
  sed -i "s/^127\.0\.1\.1.*/127.0.1.1\t$SVC_NAME/" /etc/hosts
else
  printf '127.0.1.1\t%s\n' "$SVC_NAME" >> /etc/hosts
fi
if [[ -d /etc/cloud/cloud.cfg.d ]]; then
  # Stop cloud-init from putting the provider's hostname back on reboot.
  printf '# Managed by byname bootstrap.sh\npreserve_hostname: true\n' > /etc/cloud/cloud.cfg.d/99-byname-hostname.cfg
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
    printf '\n[all]\n# Added by byname bootstrap.sh\n' >> "$CONFIG_TXT"
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
if [[ $TAILSCALE -eq 0 ]]; then
  ok "not used (--no-tailscale)"
elif ! command -v tailscale >/dev/null 2>&1; then
  repo="https://pkgs.tailscale.com/stable/${OS_ID}/${OS_CODENAME}"
  curl -fsSL "$repo.noarmor.gpg" -o /usr/share/keyrings/tailscale-archive-keyring.gpg
  curl -fsSL "$repo.tailscale-keyring.list" -o /etc/apt/sources.list.d/tailscale.list
  apt-get -o DPkg::Lock::Timeout=300 update -qq
  apt-get -o DPkg::Lock::Timeout=300 install -y -qq tailscale >/dev/null
  ok "installed tailscale from $repo"
else
  ok "tailscale $(tailscale version | head -1) present"
fi
if [[ $TAILSCALE -eq 1 ]]; then
  systemctl enable --now tailscaled >/dev/null 2>&1
fi

# ---------------------------------------------------------------- ssh
if [[ "$PLATFORM" == pi ]]; then
  step "OpenSSH (break-glass: key-only, LAN-only via firewall)"
elif [[ $TAILSCALE -eq 1 ]]; then
  step "OpenSSH (fallback to Tailscale SSH: key-only, tailnet-only via firewall)"
else
  step "OpenSSH (key-only; reachable as --ssh-from says)"
fi
# 01- sorts before any Imager/cloud-init drop-in; sshd keeps the first value.
cat > /etc/ssh/sshd_config.d/01-byname-hardening.conf <<EOF
# Managed by byname bootstrap.sh
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
cat > /etc/apt/apt.conf.d/52byname-unattended-upgrades <<'EOF'
// Managed by byname bootstrap.sh: Debian, Raspberry Pi and Tailscale updates.
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
rm -f /etc/systemd/journald.conf.d/10-byname-volatile.conf # older name
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
  cat > /etc/systemd/journald.conf.d/10-byname-journal.conf <<'EOF'
# Managed by byname bootstrap.sh: keep the journal in RAM to spare the SD card.
# byname's audit trail lives in its database, not in logs.
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
    # 512 MB servers: headroom for apt and unattended-upgrades, not for byname.
    fallocate -l 512M /swapfile
    chmod 0600 /swapfile
    mkswap -q /swapfile
    swapon /swapfile
    grep -q '^/swapfile ' /etc/fstab || printf '/swapfile none swap sw 0 0\n' >> /etc/fstab
    ok "added a 512 MB swap file ($((mem_kb / 1024)) MB RAM)"
  else
    ok "swap: $(swapon --show=NAME,SIZE --noheadings 2>/dev/null | tr '\n' ' ' || true)${mem_kb:+ ($((mem_kb / 1024)) MB RAM)}"
  fi
  cat > /etc/systemd/journald.conf.d/10-byname-journal.conf <<'EOF'
# Managed by byname bootstrap.sh: keep logs across reboots, capped.
[Journal]
Storage=persistent
SystemMaxUse=200M
EOF
  systemctl restart systemd-journald
  ok "journal kept on disk, capped at 200 MB"
fi

# ---------------------------------------------------------------- users/dirs
step "Users and directories"
if ! id byname >/dev/null 2>&1; then
  useradd --system --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin byname
fi
if ! id byname-backup >/dev/null 2>&1; then
  useradd --system --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin byname-backup
fi
usermod -aG byname byname-backup
install -d -m 0755 -o root -g root /usr/local/lib/byname /etc/byname
install -d -m 0700 -o root -g root /etc/credstore
ok "users byname, byname-backup (in group byname); /usr/local/lib/byname, /etc/byname, /etc/credstore"

# ---------------------------------------------------------------- config
step "Service configuration"
TS_SUFFIX=""
if [[ $TAILSCALE -eq 1 ]]; then
  TS_SUFFIX="$(tailscale status --json 2>/dev/null | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("MagicDNSSuffix") or "")
except Exception: print("")' || true)"
fi
HOSTS="$SVC_NAME"
[[ -n "$TS_SUFFIX" ]] && HOSTS+=",$SVC_NAME.$TS_SUFFIX"
[[ -n "$LAN_DOMAIN" ]] && HOSTS+=",$SVC_NAME.$LAN_DOMAIN"
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
ALLOW="127.0.0.1/32,::1/128"
[[ $TAILSCALE -eq 1 ]] && ALLOW="100.64.0.0/10,fd7a:115c:a1e0::/48,$ALLOW"
[[ $LAN_ACCESS -eq 1 ]] && ALLOW+=",$LAN_SUBNET"
NO_WHOIS=false
[[ $TAILSCALE -eq 0 ]] && NO_WHOIS=true
PUBLIC_URL=""
PUBLIC_LISTEN=""
if [[ -n "$PUBLIC_DOMAIN" ]]; then
  PUBLIC_URL="https://$PUBLIC_DOMAIN"
  PUBLIC_LISTEN=":443"
  getent ahosts "$PUBLIC_DOMAIN" >/dev/null 2>&1 ||
    warn "$PUBLIC_DOMAIN doesn't resolve yet: byname can't get its certificate until its DNS points here"
fi
if [[ $FUNNEL -eq 1 ]]; then
  [[ -n "$TS_SUFFIX" ]] || die "--funnel: join the tailnet first (tailscale up), then re-run"
  ts_ver="$(tailscale version 2>/dev/null | head -1)"
  python3 -c 'import sys; v = tuple(int(x) for x in sys.argv[1].split("-")[0].split(".")[:2]); sys.exit(v < (1, 98))' "$ts_ver" 2>/dev/null ||
    die "--funnel needs Tailscale 1.98 or later (Funnel to a unix socket); this is ${ts_ver:-unknown}: apt-get install tailscale"
  DNS_NAME="$(tailscale status --json --peers=false 2>/dev/null | python3 -c 'import json, sys
print(((json.load(sys.stdin).get("Self") or {}).get("DNSName") or "").rstrip(".").lower())')"
  [[ "$DNS_NAME" == "$SVC_NAME.$TS_SUFFIX" ]] ||
    die "this machine's tailnet name is '$DNS_NAME', not $SVC_NAME.$TS_SUFFIX (is another device named $SVC_NAME?)"
  PUBLIC_URL="https://$DNS_NAME"
  PUBLIC_LISTEN="unix:$PUBLIC_SOCK"
fi
# byname.env is fully managed (rewritten on every run); local overrides go in
# byname.local.env, which byname.service reads afterwards.
sed -e "s|@BYNAME_HOSTS@|$HOSTS|" -e "s|@BYNAME_ALLOW_CIDRS@|$ALLOW|" -e "s|@BYNAME_NO_WHOIS@|$NO_WHOIS|" \
  -e "s|@BYNAME_PUBLIC_URL@|$PUBLIC_URL|" -e "s|@BYNAME_PUBLIC_LISTEN@|$PUBLIC_LISTEN|" \
  "$DEPLOY_DIR/host/byname.env.in" > /etc/byname/byname.env
chmod 0644 /etc/byname/byname.env
[[ -f /etc/byname/byname.local.env ]] || printf '# Local overrides for /etc/byname/byname.env (same BYNAME_* variables). Kept across bootstrap runs.\n' > /etc/byname/byname.local.env
# What this host is, for byname-verify. Not read by byname itself.
cat > /etc/byname/host.conf <<EOF
# Managed by byname bootstrap.sh; read by byname-verify.
PLATFORM=$PLATFORM
NAME=$SVC_NAME
TAG=$TAG
TAILSCALE=$TAILSCALE
LAN_SUBNET=$LAN_SUBNET
SSH_FROM=$SSH_FROM
PUBLIC_DOMAIN=$PUBLIC_DOMAIN
FUNNEL=$FUNNEL
EOF
chmod 0644 /etc/byname/host.conf
ok "wrote /etc/byname/byname.env (BYNAME_HOSTS=$HOSTS)"
ok "allowed peers: $ALLOW"
[[ -n "$PUBLIC_URL" ]] && ok "public address $PUBLIC_URL, sign-in with:$SIGNIN"
[[ $TAILSCALE -eq 0 || -n "$TS_SUFFIX" ]] || warn "tailnet not joined yet; re-run this script after 'tailscale up' to add the full tailnet name to BYNAME_HOSTS"
# systemd's IP filter for byname.service only allows the tailnet and localhost;
# LAN access adds the LAN subnet in a drop-in.
install -d -m 0755 /etc/systemd/system/byname.service.d
if [[ $LAN_ACCESS -eq 1 ]]; then
  printf '# Managed by byname bootstrap.sh --lan-access\n[Service]\nIPAddressAllow=%s\n' "$LAN_SUBNET" \
    > /etc/systemd/system/byname.service.d/10-lan-access.conf
  ok "byname.service accepts $LAN_SUBNET (drop-in 10-lan-access.conf)"
else
  rm -f /etc/systemd/system/byname.service.d/10-lan-access.conf
fi
if [[ $PUBLIC -eq 1 ]]; then
  cat > /etc/systemd/system/byname.service.d/20-public.conf <<EOF
# Managed by byname bootstrap.sh (--public-domain or --funnel)
[Unit]
# IPAddressAllow=any below also lifts systemd's inbound filter on port 80, so
# don't run without the firewall.
Requires=byname-firewall.service

[Service]
# With --funnel, Tailscale Serve/Funnel proxies the public address to
# $PUBLIC_SOCK. The directory is 0700, so only byname and root (tailscaled) can
# reach the socket; byname trusts tailscaled's forwarding headers there.
RuntimeDirectory=byname
RuntimeDirectoryMode=0700
# Google and Slack client secrets: /etc/credstore/byname.google-client-secret,
# /etc/credstore/byname.slack-client-secret.
ImportCredential=byname.*
# Sign-in calls Google's and Slack's token endpoints, and --public-domain
# serves the internet and fetches certificates from Let's Encrypt, so byname needs
# internet access. What reaches it is still limited by the firewall (which
# the unit requires) and by byname's own checks.
IPAddressAllow=any
EOF
  ok "byname.service: credentials, internet access, requires the firewall (drop-in 20-public.conf)"
else
  rm -f /etc/systemd/system/byname.service.d/20-public.conf
fi
install -m 0644 "$DEPLOY_DIR/host/byname-backup.env.example" /etc/byname/byname-backup.env.example

# ---------------------------------------------------------------- firewall
step "Firewall (nftables, table inet byname_filter)"
if [[ $LAN_ACCESS -eq 1 ]]; then
  LAN_HTTP_RULE="ip saddr $LAN_SUBNET tcp dport 80 accept"
else
  LAN_HTTP_RULE="# (no LAN HTTP access; on a Pi, bootstrap.sh --lan-access turns it on)"
fi
# SSH: from the LAN on a Pi, over the tailnet on a cloud server with
# Tailscale, and from --ssh-from addresses (required on a cloud server
# without Tailscale).
SSH_RULES=()
SSH_NETS=""
if [[ "$PLATFORM" == pi ]]; then
  SSH_RULES+=("ip saddr $LAN_SUBNET tcp dport 22 accept")
  SSH_NETS="$LAN_SUBNET"
elif [[ $TAILSCALE -eq 1 ]]; then
  SSH_RULES+=('iifname "tailscale0" tcp dport 22 accept')
  SSH_NETS="$TAILNET_NETS"
fi
if [[ "$SSH_FROM" == any ]]; then
  # Key-only sshd; the provider's firewall (ssh_cidrs) does the address
  # filtering, so a changed home address is fixed with `tofu apply`.
  SSH_RULES+=("tcp dport 22 accept")
  SSH_NETS="0.0.0.0/0 ::/0"
elif [[ -n "$SSH_FROM" ]]; then
  v4="$(tr ',' '\n' <<<"$SSH_FROM" | grep -v ':' | paste -sd, - || true)"
  v6="$(tr ',' '\n' <<<"$SSH_FROM" | grep ':' | paste -sd, - || true)"
  [[ -n "$v4" ]] && SSH_RULES+=("ip saddr { $v4 } tcp dport 22 accept")
  [[ -n "$v6" ]] && SSH_RULES+=("ip6 saddr { $v6 } tcp dport 22 accept")
  SSH_NETS="$SSH_NETS ${SSH_FROM//,/ }"
fi
SSH_RULE="$(printf '%s; ' "${SSH_RULES[@]}")"
if [[ $TAILSCALE -eq 1 ]]; then
  TAILNET_RULES='udp dport 41641 accept; iifname "tailscale0" tcp dport 80 accept'
else
  TAILNET_RULES="# (no Tailscale)"
fi
if [[ -n "$PUBLIC_DOMAIN" ]]; then
  PUBLIC_RULE="tcp dport { 80, 443 } accept"
else
  PUBLIC_RULE="# (no public domain; bootstrap.sh --public-domain opens 80 and 443)"
fi
sed -e "s|@SSH_RULE@|$SSH_RULE|" -e "s|@LAN_HTTP_RULE@|$LAN_HTTP_RULE|" \
  -e "s|@TAILNET_RULES@|$TAILNET_RULES|" -e "s|@PUBLIC_RULE@|$PUBLIC_RULE|" \
  "$DEPLOY_DIR/host/firewall.nft.in" > /etc/byname/firewall.nft.new
nft -c -f /etc/byname/firewall.nft.new || die "firewall rules failed to parse"
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
    rm -f /etc/byname/firewall.nft.new
    die "OpenSSH session(s) from outside ${SSH_NETS// /, }: $outside — fix --lan-subnet or --ssh-from, use Tailscale SSH, or pass --force-firewall"
  fi
fi
mv /etc/byname/firewall.nft.new /etc/byname/firewall.nft
# Debian's nftables.service flushes the whole ruleset (including tailscaled's).
systemctl disable --now nftables.service >/dev/null 2>&1 || true
install -m 0644 "$DEPLOY_DIR/systemd/byname-firewall.service" /etc/systemd/system/byname-firewall.service
systemctl daemon-reload
systemctl enable byname-firewall.service >/dev/null 2>&1
systemctl restart byname-firewall.service
if [[ "$PLATFORM" == cloud ]]; then
  allowed=""
  [[ $TAILSCALE -eq 1 ]] && allowed+="tcp/80 and tcp/22 on tailscale0, udp/41641, "
  [[ -n "$SSH_FROM" ]] && allowed+="tcp/22 from $SSH_FROM, "
  [[ -n "$PUBLIC_DOMAIN" ]] && allowed+="tcp/80 and tcp/443 from anywhere, "
  ok "input policy drop; allowed: ${allowed}ICMP, DHCP"
elif [[ $LAN_ACCESS -eq 1 ]]; then
  ok "input policy drop; allowed: tcp/80 on tailscale0 and from $LAN_SUBNET, tcp/22 from $LAN_SUBNET, udp/41641, ICMP, DHCP"
else
  ok "input policy drop; allowed: tcp/80 on tailscale0, tcp/22 from $LAN_SUBNET, udp/41641, ICMP, DHCP"
fi

# ---------------------------------------------------------------- units
step "systemd units"
for unit in byname.service byname-backup.service byname-backup.timer; do
  install -m 0644 "$DEPLOY_DIR/systemd/$unit" "/etc/systemd/system/$unit"
done
install -m 0755 "$DEPLOY_DIR/host/verify.sh" /usr/local/sbin/byname-verify
install -m 0755 "$DEPLOY_DIR/host/restore.sh" /usr/local/sbin/byname-restore
systemctl daemon-reload
systemctl enable byname.service >/dev/null 2>&1
# Pick up env/drop-in changes if byname is already running.
systemctl try-restart byname.service
ok "byname.service enabled (starts once a binary is deployed)"
if [[ -f /etc/byname/byname-backup.env && -f /etc/credstore/byname-backup-aws ]]; then
  systemctl enable --now byname-backup.timer >/dev/null 2>&1
  ok "byname-backup.timer enabled"
else
  ok "backups not configured yet (see docs/operations.md, 'Backups')"
fi
ok "installed byname-verify and byname-restore to /usr/local/sbin"

# ---------------------------------------------------------------- public
if [[ $FUNNEL -eq 1 ]]; then
  step "Public address (Tailscale Funnel)"
  # Funnel to byname's socket; also answers tailnet devices on https://. The
  # timeout stops a first-time consent prompt from hanging this script.
  if ! out="$(timeout 60 tailscale funnel --bg --yes --https=443 "unix:$PUBLIC_SOCK" 2>&1)"; then
    printf '%s\n' "$out" | sed 's/^/    /'
    die "tailscale funnel failed: turn on HTTPS certificates (admin console → DNS) and give $TAG the funnel attribute in your tailnet policy (deploy/tailscale/policy.hujson), then re-run"
  fi
  ok "$PUBLIC_URL is on the internet (Funnel → $PUBLIC_SOCK); outside the tailnet it asks for sign-in"
elif command -v tailscale >/dev/null 2>&1 && tailscale funnel status 2>/dev/null | grep -qF "$PUBLIC_SOCK"; then
  step "Public address"
  tailscale funnel --https=443 off >/dev/null 2>&1 || true
  tailscale serve --https=443 off >/dev/null 2>&1 || true
  ok "Funnel off"
fi
if [[ -n "$PUBLIC_DOMAIN" ]]; then
  step "Public address (your domain)"
  ok "$PUBLIC_URL: byname gets its certificate from Let's Encrypt on the first visit; everyone signs in"
fi

# ---------------------------------------------------------------- summary
step "Next steps"
if [[ $TAILSCALE -eq 1 ]] && ! tailscale status >/dev/null 2>&1; then
  if [[ "$PLATFORM" == pi ]]; then
    cat <<EOF
    1. Join the tailnet (opens a login URL; your policy must define $TAG first):
         sudo tailscale up --ssh --hostname=$SVC_NAME --advertise-tags=$TAG
    2. Re-run this script so BYNAME_HOSTS includes the full tailnet name.
EOF
  else
    warn "Tailscale is not connected; first boot should have joined it (see /var/log/cloud-init-output.log)"
  fi
fi
[[ $REBOOT_NEEDED -eq 1 ]] && echo "    * Reboot to apply config.txt changes: sudo reboot"
if [[ $TAILSCALE -eq 1 ]]; then
  echo "    * Deploy from your Mac:  make deploy TARGET=$ADMIN_USER@$SVC_NAME"
elif [[ "$PLATFORM" == pi ]]; then
  # OpenSSH on a Pi answers the LAN only, never the public domain.
  echo "    * Deploy from your Mac (on the home network):  make deploy TARGET=$ADMIN_USER@${LAN_IP:-PI-LAN-ADDRESS}"
else
  echo "    * Deploy from your Mac:  make deploy TARGET=$ADMIN_USER@${PUBLIC_DOMAIN:-SERVER-ADDRESS}"
fi
echo "    * Then check everything: sudo byname-verify"
if [[ $PUBLIC -eq 1 ]]; then
  echo "    * From a phone with Wi-Fi and Tailscale off, open $PUBLIC_URL/_/whoami: it should ask you to sign in"
fi
