#!/usr/bin/env bash
# shellcheck source-path=SCRIPTDIR
# Host smoke test: deploy/host's scripts and deploy/systemd's units on Debian 13,
# against stubs for systemd, Tailscale, apt and the byname binary (test/host/stubs,
# fake-byname.py). The firewall, sshd's config test, sqlite3, sudo and users are real.
#
# Run it with `make host-test`. This file runs INSIDE the throwaway container
# (no network), from the repo mounted read-only at /src: it rewrites /etc.
set -uo pipefail

if [[ ! -f /.dockerenv && ! -f /run/.containerenv ]] || [[ $EUID -ne 0 ]]; then
  echo "run.sh rewrites /etc, users and the firewall: run it in its container (make host-test)" >&2
  exit 2
fi
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"

# ---------------------------------------------------------------- setup
mkdir -p "$WORK" "$OUT" /boot/firmware
if ! nft list ruleset >/dev/null 2>&1; then
  echo "nft can't reach the kernel: run the container with --cap-add NET_ADMIN" >&2
  exit 2
fi
ssh-keygen -A >/dev/null
mkdir -p /run/sshd
# Lookups fail at once (Docker bind-mounts the file, so write it in place).
echo 'nameserver 127.0.0.1' > /etc/resolv.conf
printf '# Raspberry Pi config.txt (host test)\n[all]\n' > "$WORK/config.txt.orig"
id admin >/dev/null 2>&1 || useradd --create-home --shell /bin/bash admin
install -d -m 0700 -o admin -g admin /home/admin/.ssh
echo 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHostTestOnlyHostTestOnlyHostTestOnly admin@host-test' > /home/admin/.ssh/authorized_keys
id nokey >/dev/null 2>&1 || useradd --create-home --shell /bin/bash nokey
# A LAN interface and a tailscale0, so address detection has something to find.
HAVE_IFACES=0
if ip link add lan0 type dummy 2>/dev/null; then
  ip addr add 192.168.1.50/24 dev lan0 && ip link set lan0 up &&
    ip link add tailscale0 type dummy && ip addr add 100.101.102.103/32 dev tailscale0 &&
    ip link set tailscale0 up && HAVE_IFACES=1
fi
[[ $HAVE_IFACES -eq 1 ]] || note "no dummy interfaces here: the LAN-address checks are skipped"
# Debian's nftables.service flushes the whole ruleset when it stops. The
# package ships it; this is in case it ever doesn't.
NFT_UNIT=/usr/lib/systemd/system/nftables.service
if ! grep -qs '^ExecStop=/usr/sbin/nft flush ruleset' "$NFT_UNIT" /lib/systemd/system/nftables.service; then
  mkdir -p "${NFT_UNIT%/*}"
  printf '[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=/usr/sbin/nft -f /etc/nftables.conf\nExecReload=/usr/sbin/nft -f /etc/nftables.conf\nExecStop=/usr/sbin/nft flush ruleset\n' > "$NFT_UNIT"
fi
reset_host

T=(--admin-user admin)  # every run
PI=(--platform pi --lan-subnet 192.168.1.0/24)

# ---------------------------------------------------------------- preflight
section "bootstrap.sh preflight: refusals"
# refuse CASE REGEX ARGS...: preflight stops with REGEX and changes nothing.
refuse() {
  local case=$1 want=$2
  shift 2
  if run_bootstrap "$case" "$want" "$@"; then
    if [[ ! -e /etc/byname && ! -e $STUB_STATE/hostname ]]; then ok "nothing changed"; else bad "preflight changed the host before refusing"; fi
  fi
  reset_host
}
refuse funnel-without-ts 'needs Tailscale' "${T[@]}" --platform cloud --no-tailscale --ssh-from 203.0.113.7/32 --funnel
refuse funnel-and-domain 'choose one of --funnel and --public-domain' "${T[@]}" --platform cloud --funnel --public-domain me.example.com
refuse cloud-no-ts-no-ssh 'without Tailscale, --ssh-from is required' "${T[@]}" --platform cloud --no-tailscale
refuse cloud-lan 'a cloud server is tailnet-only' "${T[@]}" --platform cloud --lan-subnet 192.168.1.0/24
refuse cloud-lan-access 'a cloud server is tailnet-only' "${T[@]}" --platform cloud --lan-access
refuse bad-platform '--platform must be pi or cloud' "${T[@]}" --platform mac
refuse wide-ssh-from '--ssh-from must be' "${T[@]}" --platform cloud --ssh-from 10.0.0.0/8
refuse bad-ssh-from '--ssh-from must be' "${T[@]}" --platform cloud --ssh-from 203.0.113.7/24
refuse bad-name "invalid --name 'Bad_Name'" "${T[@]}" --platform cloud --name Bad_Name
refuse name-localhost 'would shadow' "${T[@]}" --platform cloud --name localhost
refuse bad-tag "invalid --tag" "${T[@]}" --platform cloud --tag me
refuse bad-domain '--public-domain must be' "${T[@]}" --platform cloud --public-domain https://me.example.com
refuse ts-net-domain "can't be a ts.net name" "${T[@]}" --platform cloud --public-domain me.tail1234.ts.net
refuse public-no-signin 'needs Google or Slack sign-in' "${T[@]}" --platform cloud --public-domain me.example.com
install -d /etc/byname
printf 'BYNAME_GOOGLE_CLIENT_ID=123-test.apps.googleusercontent.com\nBYNAME_GOOGLE_ALLOW=family.example\n' > /etc/byname/byname.local.env
CASE=public-no-secret
run_bootstrap public-no-secret 'client-secret is missing or empty' "${T[@]}" --platform cloud --funnel
reset_host
refuse unknown-admin 'user ghost does not exist' --admin-user ghost --platform cloud
refuse admin-without-key 'authorized_keys is empty' --admin-user nokey --platform cloud
ARCH=aarch64
refuse pi-no-lan '--lan-subnet is required on a Raspberry Pi' "${T[@]}" --platform pi
ARCH=aarch64
refuse pi-ssh-any '--ssh-from any is for cloud servers' "${T[@]}" "${PI[@]}" --ssh-from any
ARCH=aarch64
refuse pi-no-ts-unreachable 'a Pi needs --lan-access or --public-domain' "${T[@]}" "${PI[@]}" --no-tailscale
ARCH=aarch64
refuse pi-bad-subnet '--lan-subnet must be an IPv4 network' "${T[@]}" --platform pi --lan-subnet 192.168.1.1/24
ARCH=aarch64
refuse pi-mdns-domain 'mDNS' "${T[@]}" "${PI[@]}" --lan-domain local
ARCH=x86_64
refuse pi-32-bit 'expected 64-bit Raspberry Pi OS' "${T[@]}" "${PI[@]}"
touch "$STUB_STATE/units/tailscaled.service.active"
refuse no-ts-but-tailscaled 'tailscaled is running' "${T[@]}" --platform cloud --no-tailscale --ssh-from 203.0.113.7/32

# ---------------------------------------------------------------- variants
section "bootstrap.sh: every platform and access combination"
TS_ALLOW="100.64.0.0/10,fd7a:115c:a1e0::/48,127.0.0.1/32,::1/128"
LAN_IP_HOST=""
[[ $HAVE_IFACES -eq 1 ]] && LAN_IP_HOST=",192.168.1.50"
FW_TS=('+udp dport 41641 accept' '+iifname "tailscale0" tcp dport 80 accept'
  '+iifname != "tailscale0" ip saddr 100.64.0.0/10 drop' '+iifname != "tailscale0" ip6 saddr fd7a:115c:a1e0::/48 drop')
FW_NO_TS=('-udp dport 41641 accept' '-iifname "tailscale0"' '-iifname != "tailscale0"' '-100.64.0.0/10')
FW_NO_LAN=('-ip saddr 192.168.1.0/24 tcp dport 80 accept')
# OpenSSH from anywhere (--ssh-from any): the bare rule, not one limited by interface or address.
SSH_ANY='(^|;)[[:space:]]*tcp dport 22 accept;'
FW_PUBLIC=('+tcp dport { 80, 443 } accept')
FW_NO_PUBLIC=('-tcp dport { 80, 443 } accept')

# Common to every successful run.
after_run() {
  check "hostname set" grep -qx "${1:-me}" "$STUB_STATE/hostname"
  check "/etc/hosts names this machine" grep -qP "^127\\.0\\.1\\.1\\t${1:-me}\$" /etc/hosts
  check "sshd accepts the hardened config" sshd -t
  expect /etc/ssh/sshd_config.d/01-byname-hardening.conf '+AllowUsers admin' '+PasswordAuthentication no' '+TrustedUserCAKeys none'
  check "sshd trusts no certificate authority" bash -c "sshd -T | grep -qx 'trustedusercakeys none'"
  check "no candidate sshd files left" bash -c "! compgen -G '/etc/ssh/sshd_config.d/.01-byname-hardening.*' && ! compgen -G '/etc/ssh/.byname-sshd-test.*'"
  check "host.conf written" test -s /etc/byname/host.conf
  check "byname.service installed" test -f /etc/systemd/system/byname.service
  check "byname-firewall.service enabled" called '^systemctl enable byname-firewall.service'
  # Reloaded in one transaction, never deleted first by a restart.
  check "firewall reloaded, not restarted" called '^systemctl reload-or-restart byname-firewall.service'
  check "firewall never restarted" bash -c "! grep -q '^systemctl restart byname-firewall' '$STUB_STATE/systemctl.log'"
  check "byname-verify and byname-restore installed" test -x /usr/local/sbin/byname-verify -a -x /usr/local/sbin/byname-restore
  # byname.service itself never pulls in tailscaled; the drop-in does, with Tailscale.
  expect /etc/systemd/system/byname.service '!^(Wants|After)=.*tailscaled'
  if grep -qx 'TAILSCALE=1' /etc/byname/host.conf; then
    expect /etc/systemd/system/byname.service.d/05-tailscale.conf '+Wants=tailscaled.service' '+After=tailscaled.service'
  else
    check "no tailscaled drop-in" test ! -e /etc/systemd/system/byname.service.d/05-tailscale.conf
  fi
}

ARCH=aarch64
if run_bootstrap pi-tailscale ok "${T[@]}" "${PI[@]}"; then
  after_run
  env_is BYNAME_HOSTS "me,me.tail1234.ts.net"
  env_is BYNAME_ALLOW_CIDRS "$TS_ALLOW"
  env_is BYNAME_NO_WHOIS false
  env_is BYNAME_PUBLIC_URL ""
  check "no LAN drop-in" test ! -e /etc/systemd/system/byname.service.d/10-lan-access.conf
  check "no public drop-in" test ! -e /etc/systemd/system/byname.service.d/20-public.conf
  expect /boot/firmware/config.txt '+dtoverlay=disable-wifi' '+dtoverlay=disable-bt'
  check_firewall "${FW_TS[@]}" "${FW_NO_LAN[@]}" "${FW_NO_PUBLIC[@]}" '+ip saddr 192.168.1.0/24 tcp dport 22 accept'

  # Re-run with LAN access, then without: each run applies exactly its flags.
  ARCH=aarch64
  if run_bootstrap pi-tailscale-lan ok "${T[@]}" "${PI[@]}" --lan-access --lan-domain lan --name links; then
    after_run links
    env_is BYNAME_HOSTS "links,links.tail1234.ts.net,links.lan$LAN_IP_HOST"
    env_is BYNAME_ALLOW_CIDRS "$TS_ALLOW,192.168.1.0/24"
    expect /etc/systemd/system/byname.service.d/10-lan-access.conf '+IPAddressAllow=192.168.1.0/24'
    check_firewall "${FW_TS[@]}" "${FW_NO_PUBLIC[@]}" '+ip saddr 192.168.1.0/24 tcp dport 80 accept'
  fi
  ARCH=aarch64
  if run_bootstrap pi-tailscale-lan-off ok "${T[@]}" "${PI[@]}"; then
    env_is BYNAME_ALLOW_CIDRS "$TS_ALLOW"
    check "LAN drop-in removed" test ! -e /etc/systemd/system/byname.service.d/10-lan-access.conf
    check_firewall "${FW_TS[@]}" "${FW_NO_LAN[@]}"
  fi
fi
reset_host

ARCH=aarch64
if run_bootstrap pi-no-tailscale-lan ok "${T[@]}" "${PI[@]}" --no-tailscale --lan-access; then
  after_run
  env_is BYNAME_HOSTS "me$LAN_IP_HOST"
  env_is BYNAME_ALLOW_CIDRS "127.0.0.1/32,::1/128,192.168.1.0/24"
  env_is BYNAME_NO_WHOIS true
  check_firewall "${FW_NO_TS[@]}" "${FW_NO_PUBLIC[@]}" '+ip saddr 192.168.1.0/24 tcp dport 80 accept'
fi
reset_host

signin
ARCH=aarch64
if run_bootstrap pi-tailscale-lan-domain ok "${T[@]}" "${PI[@]}" --lan-access --public-domain me.example.com; then
  after_run
  env_is BYNAME_PUBLIC_URL "https://me.example.com"
  env_is BYNAME_PUBLIC_LISTEN ":443"
  expect /etc/systemd/system/byname.service.d/20-public.conf '+Requires=byname-firewall.service' '+ImportCredential=byname.*'
  check_firewall "${FW_TS[@]}" "${FW_PUBLIC[@]}" '+ip saddr 192.168.1.0/24 tcp dport 80 accept'
  # Tailnet addresses are dropped before anything opens port 80 beyond tailscale0.
  first_line() { grep -nF -- "$1" /etc/byname/firewall.nft | head -n 1 | cut -d: -f1; }
  spoof="$(first_line 'ip saddr 100.64.0.0/10 drop')"
  check "spoofed tailnet addresses dropped before the LAN rule" test "${spoof:-999}" -lt "$(first_line 'ip saddr 192.168.1.0/24 tcp dport 80 accept')"
  check "and before the public rule" test "${spoof:-999}" -lt "$(first_line 'tcp dport { 80, 443 } accept')"
  check "loaded in the kernel" bash -c "nft list chain inet byname_filter input | grep -q 'iifname != \"tailscale0\" ip saddr 100.64.0.0/10 drop'"
fi
reset_host

signin
ARCH=aarch64
if run_bootstrap pi-tailscale-funnel ok "${T[@]}" "${PI[@]}" --funnel; then
  after_run
  env_is BYNAME_PUBLIC_URL "https://me.tail1234.ts.net"
  env_is BYNAME_PUBLIC_LISTEN "unix:/run/byname/public.sock"
  check "funnel publishes byname's socket" grep -qF unix:/run/byname/public.sock "$STUB_STATE/funnel"
  check_firewall "${FW_TS[@]}" "${FW_NO_LAN[@]}" "${FW_NO_PUBLIC[@]}"
  # Funnel off again when the flag goes.
  ARCH=aarch64
  if run_bootstrap pi-tailscale-funnel-off ok "${T[@]}" "${PI[@]}"; then
    check "funnel turned off" test ! -e "$STUB_STATE/funnel"
    check "public drop-in removed" test ! -e /etc/systemd/system/byname.service.d/20-public.conf
  fi
fi
reset_host

signin
ARCH=aarch64
if run_bootstrap pi-no-tailscale-domain ok "${T[@]}" "${PI[@]}" --no-tailscale --public-domain me.example.com; then
  after_run
  env_is BYNAME_ALLOW_CIDRS "127.0.0.1/32,::1/128"
  check_firewall "${FW_NO_TS[@]}" "${FW_NO_LAN[@]}" "${FW_PUBLIC[@]}" '+ip saddr 192.168.1.0/24 tcp dport 22 accept'
fi
reset_host

if run_bootstrap cloud-tailscale ok "${T[@]}" --platform cloud; then
  after_run
  env_is BYNAME_HOSTS "me,me.tail1234.ts.net"
  env_is BYNAME_ALLOW_CIDRS "$TS_ALLOW"
  check_firewall "${FW_TS[@]}" "${FW_NO_LAN[@]}" "${FW_NO_PUBLIC[@]}" '+iifname "tailscale0" tcp dport 22 accept' '-192.168.1.0/24' "!$SSH_ANY"
  cp /etc/byname/byname.env "$WORK/env.first"
  cp /etc/byname/firewall.nft "$WORK/fw.first"
  : > "$STUB_STATE/systemctl.log"
  if run_bootstrap cloud-tailscale-again ok "${T[@]}" --platform cloud; then
    check "re-run: same byname.env" cmp -s /etc/byname/byname.env "$WORK/env.first"
    check "re-run: same firewall" cmp -s /etc/byname/firewall.nft "$WORK/fw.first"
    check "re-run: the loaded firewall was reloaded (ExecReload)" called 'byname-firewall.service ExecReload: /usr/sbin/nft -f /etc/byname/firewall.nft'
    check "re-run: its table was never deleted (ExecStop)" bash -c "! grep -q 'byname-firewall.service ExecStop' '$STUB_STATE/systemctl.log'"
  fi
fi
reset_host

if run_bootstrap cloud-tailscale-ssh-from ok "${T[@]}" --platform cloud --ssh-from 203.0.113.7/32,2001:db8::/48; then
  check_firewall "${FW_TS[@]}" '+iifname "tailscale0" tcp dport 22 accept' \
    '+ip saddr { 203.0.113.7/32 } tcp dport 22 accept' '+ip6 saddr { 2001:db8::/48 } tcp dport 22 accept'
fi
reset_host

signin
if run_bootstrap cloud-tailscale-funnel ok "${T[@]}" --platform cloud --funnel; then
  after_run
  env_is BYNAME_PUBLIC_LISTEN "unix:/run/byname/public.sock"
  check_firewall "${FW_TS[@]}" "${FW_NO_PUBLIC[@]}"
fi
reset_host

signin
if run_bootstrap cloud-tailscale-domain-ssh-any ok "${T[@]}" --platform cloud --public-domain me.example.com --ssh-from any; then
  after_run
  check_firewall "${FW_TS[@]}" "${FW_PUBLIC[@]}" '+iifname "tailscale0" tcp dport 22 accept' "~$SSH_ANY"
fi
reset_host

# A server built without a Tailscale auth key has no Tailscale installed.
TAILSCALE_INSTALLED=0
if run_bootstrap cloud-no-tailscale ok "${T[@]}" --platform cloud --no-tailscale --ssh-from 203.0.113.7/32; then
  after_run
  check "nothing to mask" bash -c "! grep -q 'mask tailscaled' '$STUB_STATE/systemctl.log'"
  check "warns there's no public address yet" grep -q 'no --public-domain yet' "$LOG"
  env_is BYNAME_NO_WHOIS true
  env_is BYNAME_ALLOW_CIDRS "127.0.0.1/32,::1/128"
  check_firewall "${FW_NO_TS[@]}" "${FW_NO_PUBLIC[@]}" '+ip saddr { 203.0.113.7/32 } tcp dport 22 accept' "!$SSH_ANY"
fi
reset_host

signin
TAILSCALE_INSTALLED=0
if run_bootstrap cloud-no-tailscale-domain ok "${T[@]}" --platform cloud --no-tailscale --ssh-from any --public-domain me.example.com; then
  after_run
  check_firewall "${FW_NO_TS[@]}" "${FW_PUBLIC[@]}" "~$SSH_ANY"
fi
reset_host

# ---------------------------------------------------------------- sshd
section "bootstrap.sh: an sshd configuration that fails its test"
id admin2 >/dev/null 2>&1 || useradd --create-home --shell /bin/bash admin2
install -d -m 0700 -o admin2 -g admin2 /home/admin2/.ssh
cp /home/admin/.ssh/authorized_keys /home/admin2/.ssh/authorized_keys
if run_bootstrap sshd-good ok "${T[@]}" --platform cloud; then
  cp /etc/ssh/sshd_config.d/01-byname-hardening.conf "$WORK/sshd-dropin.before"
  echo 'NoSuchOption yes' > /etc/ssh/sshd_config.d/99-host-test-broken.conf
  if run_bootstrap sshd-broken 'sshd rejects this configuration' --admin-user admin2 --platform cloud; then
    check "the working drop-in is untouched" cmp -s /etc/ssh/sshd_config.d/01-byname-hardening.conf "$WORK/sshd-dropin.before"
    check "no candidate sshd files left" bash -c "! compgen -G '/etc/ssh/sshd_config.d/.01-byname-hardening.*' && ! compgen -G '/etc/ssh/.byname-sshd-test.*'"
  fi
  rm -f /etc/ssh/sshd_config.d/99-host-test-broken.conf
fi
reset_host

# ---------------------------------------------------------------- nftables.service
section "bootstrap.sh: Debian's nftables.service running"
# Running and enabled, next to tailscaled's own rules (a stand-in table).
touch "$STUB_STATE/units/nftables.service.active" "$STUB_STATE/units/nftables.service.enabled"
nft add table inet ts_standin
if run_bootstrap nftables-service ok "${T[@]}" --platform cloud; then
  check "tailscaled's rules survive (nothing flushed the ruleset)" nft list table inet ts_standin
  check "nftables.service disabled" test ! -e "$STUB_STATE/units/nftables.service.enabled"
  check "nftables.service stopped" test ! -e "$STUB_STATE/units/nftables.service.active"
  check "its flush never ran" bash -c "! grep -q 'nft flush ruleset' '$STUB_STATE/systemctl.log'"
  check "the no-flush drop-in is gone" test ! -e /run/systemd/system/nftables.service.d/50-byname-no-flush.conf
  check_firewall "${FW_TS[@]}"
fi
reset_host

# ---------------------------------------------------------------- tailscale off/on
section "bootstrap.sh: Tailscale off, and on again"
if run_bootstrap ts-on ok "${T[@]}" --platform cloud; then
  check "tailscaled running" test -e "$STUB_STATE/units/tailscaled.service.active"
  # Turning it off while tailscaled runs is refused; do what the message says.
  if run_bootstrap ts-off-while-running 'sudo systemctl mask --now tailscaled' "${T[@]}" --platform cloud --no-tailscale --ssh-from 203.0.113.7/32; then
    PATH="$(stub_path)" systemctl mask --now tailscaled >/dev/null 2>&1
  fi
  if run_bootstrap ts-off ok "${T[@]}" --platform cloud --no-tailscale --ssh-from 203.0.113.7/32; then
    after_run
    check "tailscaled masked" test -e "$STUB_STATE/units/tailscaled.service.masked"
    check "tailscaled not running" test ! -e "$STUB_STATE/units/tailscaled.service.active"
    check "byname.service doesn't want tailscaled" \
      bash -c "! PATH='$(stub_path)' systemctl show byname.service -p Wants --value | grep -qw tailscaled.service"
    run_verify ts-off
    expect "$LOG" '+PASS tailscaled masked' "+PASS byname.service doesn't pull in tailscaled" '+PASS WhoIs off' '+PASS tailnet ranges not trusted'
  fi
  if run_bootstrap ts-on-again ok "${T[@]}" --platform cloud; then
    after_run
    check "tailscaled unmasked" test ! -e "$STUB_STATE/units/tailscaled.service.masked"
    check "tailscaled running again" test -e "$STUB_STATE/units/tailscaled.service.active"
    check "byname.service wants tailscaled" \
      bash -c "PATH='$(stub_path)' systemctl show byname.service -p Wants --value | grep -qw tailscaled.service"
    run_verify ts-on-again
    expect "$LOG" '+PASS byname.service starts after tailscaled' '+PASS tailnet addresses dropped unless they arrive on tailscale0' \
      '+PASS tailnet hostname me' '+PASS BYNAME_HOSTS includes the tailnet FQDN'
  fi
fi
reset_host

# ---------------------------------------------------------------- platform detection
section "bootstrap.sh: platform detection"
if run_bootstrap detect-platform ok "${T[@]}"; then
  check "not a Raspberry Pi: cloud" grep -qx 'PLATFORM=cloud' /etc/byname/host.conf
  check "no 'No such file' noise" bash -c "! grep -q 'No such file' '$LOG'"
fi
reset_host

# ---------------------------------------------------------------- lockout guard
section "bootstrap.sh: OpenSSH lockout guard"
printf '0      0      192.168.1.50:22      198.51.100.9:51234\n' > "$STUB_STATE/ss-established"
ARCH=aarch64
if run_bootstrap ssh-from-outside 'OpenSSH session\(s\) from outside' "${T[@]}" "${PI[@]}"; then
  check "firewall not replaced" test ! -e /etc/byname/firewall.nft -a ! -e /etc/byname/firewall.nft.new
fi
ARCH=aarch64
run_bootstrap ssh-from-outside-forced ok "${T[@]}" "${PI[@]}" --force-firewall
reset_host
printf '0      0      192.168.1.50:22      192.168.1.20:51234\n' > "$STUB_STATE/ss-established"
ARCH=aarch64
run_bootstrap ssh-from-lan ok "${T[@]}" "${PI[@]}"

# ---------------------------------------------------------------- install.sh
section "install.sh: deploy, health check, rollback"
# The host install.sh deploys to: a Pi with Tailscale.
reset_host
ARCH=aarch64
run_bootstrap install-host ok "${T[@]}" "${PI[@]}" || true
CASE=install
LIB=/usr/local/lib/byname
DB=/var/lib/byname/byname.db
SNAPS=/var/lib/byname/snapshots

# release VERSION SCHEMA [BROKEN]: a staging directory like deploy.sh's.
release() {
  local stage="$WORK/stage-$1"
  rm -rf "$stage"
  mkdir -p "$stage"
  sed -e "s/@VERSION@/$1/" -e "s/@SCHEMA@/$2/" -e "s/@BROKEN@/${3:-0}/" "$SRC/test/host/fake-byname.py" > "$stage/byname"
  cp "$DEPLOY"/host/{install,verify,restore}.sh "$DEPLOY"/systemd/*.service "$DEPLOY"/systemd/*.timer "$stage/"
  echo "$stage"
}

# deploy VERSION: run install.sh like deploy.sh does. Output in $LOG.
deploy() {
  local stage="$WORK/stage-$1" rc=0
  CASE="deploy-$1"
  LOG="$OUT/install-$1.log"
  printf '\n\033[1m-- %s\033[0m\n' "$CASE"
  PATH="$(stub_path)" bash "$stage/install.sh" "$stage" "$1" "$(sha256sum "$stage/byname" | cut -d' ' -f1)" > "$LOG" 2>&1 || rc=$?
  return $rc
}

current() { readlink "$LIB/current"; }
live_version() { curl -fsS -m 3 -H "Host: ${1:-me}" http://127.0.0.1/_/healthz 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin)["version"])' 2>/dev/null; }
is_live() {
  if [[ "$(live_version "${2:-me}")" == "$1" ]]; then ok "byname $1 is live"; else bad "want byname $1 live, got '$(live_version "${2:-me}")'"; tail -n 5 "$LOG" | sed 's/^/         /'; fi
}

release v1 2 >/dev/null
if deploy v1; then ok "first deploy"; else bad "first deploy failed"; tail -n 8 "$LOG" | sed 's/^/         /'; fi
check "current -> v1" test "$(current)" == v1
is_live v1
# Give the database byname's real tables and a link.
for m in "$SRC"/internal/store/migrations/*.sql; do sqlite3 "$DB" < "$m"; done
sqlite3 "$DB" "INSERT INTO links (name, key, target, created_by, created_at, updated_by, updated_at) VALUES ('wiki', 'wiki', 'https://wiki.example', 'test', 0, 'test', 0);"

release v2 2 >/dev/null
if deploy v2; then ok "upgrade deploy"; else bad "upgrade failed"; tail -n 8 "$LOG" | sed 's/^/         /'; fi
check "pre-deploy snapshot taken with v1" grep -q 'pre-deploy snapshot (with v1)' "$LOG"
check "a snapshot exists" compgen -G "$SNAPS/byname-*.db"
is_live v2

release v3 2 1 >/dev/null
if deploy v3; then bad "a broken release deployed"; else ok "broken release refused (exit $?)"; fi
check "rolled back" grep -q 'rolling back to v2' "$LOG"
check "current -> v2" test "$(current)" == v2
is_live v2

# A release that migrates the schema and then fails: the rollback fails too
# (the old binary refuses the newer schema), and the restore that install.sh
# prints must bring the old release back. The restore can't use the binary.
release v4-migrates 3 1 >/dev/null
if deploy v4-migrates; then bad "a broken release deployed"; else ok "broken migrating release refused"; fi
check "rollback reported unhealthy" grep -q 'rollback also unhealthy' "$LOG"
check "database left at schema 3" test "$(sqlite3 "$DB" 'PRAGMA user_version;')" == 3
# shellcheck disable=SC2016 # the literal text: the old hint had the admin's shell expand $(ls …)
expect "$LOG" '-$(ls'
HINT="$(grep -oE 'sudo byname-restore [^ ]+' "$LOG" | tail -n 1 | cut -d' ' -f3)"
if [[ $HINT == "$SNAPS"/byname-*.db && -f $HINT ]]; then
  ok "install.sh printed the exact pre-deploy snapshot: $HINT"
  LOG="$OUT/restore-after-migration.log"
  if PATH="$(stub_path)" byname-restore "$HINT" > "$LOG" 2>&1; then ok "printed restore command works"; else bad "printed restore command failed"; tail -n 8 "$LOG" | sed 's/^/         /'; fi
  check "database back at schema 2" test "$(sqlite3 "$DB" 'PRAGMA user_version;')" == 2
  is_live v2
  KEPT="$(find "$SNAPS" -name 'pre-restore-*.db' | sort | tail -n 1)"
  check "schema-3 database kept as $KEPT" test "$(sqlite3 "$KEPT" 'PRAGMA user_version;')" == 3
else
  bad "no usable 'sudo byname-restore <file>' in install.sh's output (got '$HINT')"
fi

# ---------------------------------------------------------------- restore.sh
section "restore.sh"
CASE=restore
links_in() { sqlite3 "$1" "SELECT group_concat(name, ',') FROM (SELECT name FROM links WHERE deleted_at IS NULL ORDER BY name);"; }
add_link() { sqlite3 "${2:-$DB}" "INSERT INTO links (name, key, target, created_by, created_at, updated_by, updated_at) VALUES ('$1', '$1', 'https://$1.example', 'test', 0, 'test', 0);"; }
# restore ARGS...: byname-restore with the stubs; output in $LOG.
restore() { PATH="$(stub_path)" byname-restore "$@" > "$LOG" 2>&1; }
SNAP="$(find "$SNAPS" -name 'byname-*.db' | sort | tail -1)"
add_link later
LOG="$OUT/restore-snapshot.log"
if restore "$SNAP"; then ok "restore from a snapshot"; else bad "restore failed"; tail -n 8 "$LOG" | sed 's/^/         /'; fi
check "restored links: wiki only" test "$(links_in "$DB")" == wiki
is_live v2

# Sign-in sessions in the snapshot don't come back.
sqlite3 "$DB" "INSERT INTO sessions (token_hash, provider, subject, email, created_at, expires_at) VALUES
  (x'01', 'google', 's1', 'a@family.example', 0, 9999999999999), (x'02', 'google', 's2', 'b@family.example', 0, 9999999999999);"
SNAP="$(sudo -u byname "$LIB/current/byname" snapshot --db "$DB" --snapshot-dir "$SNAPS")"
LOG="$OUT/restore-sessions.log"
if restore "$SNAP"; then ok "restore a snapshot with sessions"; else bad "restore failed"; tail -n 8 "$LOG" | sed 's/^/         /'; fi
check "sessions cleared" test "$(sqlite3 "$DB" 'SELECT count(*) FROM sessions;')" == 0
check "and says so" grep -q 'cleared 2 sign-in session' "$LOG"

# The pre-restore copy keeps what is only in the live database's -wal, and
# restoring it (the undo) brings that back.
python3 - "$DB" <<'PY2'
import os, sqlite3, sys
con = sqlite3.connect(sys.argv[1], isolation_level=None)
con.execute("PRAGMA journal_mode = WAL")
con.execute("PRAGMA wal_autocheckpoint = 0")
con.execute("INSERT INTO links (name, key, target, created_by, created_at, updated_by, updated_at) "
            "VALUES ('walonly', 'walonly', 'https://walonly.example', 'test', 0, 'test', 0)")
os._exit(0)  # like a killed process: the change stays in byname.db-wal
PY2
check "test setup: a change only in the -wal" test -s "$DB-wal"
LOG="$OUT/restore-latest.log"
if restore --latest; then ok "restore --latest"; else bad "restore --latest failed"; tail -n 8 "$LOG" | sed 's/^/         /'; fi
check "--latest took the newest byname-*.db" grep -qF "newest snapshot: $SNAPS/$(find "$SNAPS" -name 'byname-*.db' -printf '%f\n' | sort | tail -n 1)" "$LOG"
KEPT="$(grep -oE "$SNAPS/pre-restore-[^ ]+\.db" "$LOG" | head -n 1)"
check "kept the live database: $KEPT" test -f "$KEPT"
check "with its -wal" test -s "$KEPT-wal"
check "walonly gone after the restore" test "$(links_in "$DB")" == wiki
LOG="$OUT/restore-undo.log"
if restore "$KEPT"; then ok "undo by restoring the pre-restore copy"; else bad "undo failed"; tail -n 8 "$LOG" | sed 's/^/         /'; fi
check "walonly is back" test "$(links_in "$DB")" == "walonly,wiki"
is_live v2

# A damaged live database: the restore still works and keeps the damaged copy.
SNAP="$(find "$SNAPS" -name 'byname-*.db' | sort | tail -1)"
head -c 8192 /dev/urandom > "$DB"
LOG="$OUT/restore-corrupt-live.log"
if restore "$SNAP"; then ok "restore over a corrupt database"; else bad "restore over a corrupt database failed"; tail -n 8 "$LOG" | sed 's/^/         /'; fi
is_live v2
KEPT="$(grep -oE "$SNAPS/pre-restore-[^ ]+\.db" "$LOG" | head -n 1)"
check "corrupt copy kept" test -s "$KEPT"

# A snapshot from before sign-in existed (schema 1, no sessions table).
rm -f "$WORK/schema1.db"
sqlite3 "$WORK/schema1.db" < "$SRC/internal/store/migrations/0001_init.sql"
sqlite3 "$WORK/schema1.db" 'PRAGMA user_version = 1;'
add_link old "$WORK/schema1.db"
LOG="$OUT/restore-schema1.log"
if restore "$WORK/schema1.db"; then ok "restore a schema-1 snapshot"; else bad "restore of a schema-1 snapshot failed"; tail -n 8 "$LOG" | sed 's/^/         /'; fi
check "restored links: old" test "$(links_in "$DB")" == old
is_live v2

check "pre-restore copies belong to byname" test "$(stat -c %U "$KEPT")" == byname

# A compromised byname must not get root through a restore. A snapshot is
# data: a trigger in it that calls writefile() must not run as root.
SNAP="$(find "$SNAPS" -name 'byname-*.db' | sort | tail -1)"
cp "$SNAP" "$WORK/evil.db"
sqlite3 "$WORK/evil.db" "INSERT INTO sessions (token_hash, provider, subject, email, created_at, expires_at) VALUES (x'03', 'google', 's3', 'c@family.example', 0, 1);
  CREATE TRIGGER evil BEFORE DELETE ON sessions BEGIN SELECT writefile('/root/pwned', 'owned'); END;"
cp "$DB" "$WORK/live.before"
LOG="$OUT/restore-evil-trigger.log"
if restore "$WORK/evil.db"; then bad "restored a snapshot whose trigger calls writefile()"; else ok "refused a snapshot whose trigger calls writefile()"; fi
check "the trigger wrote nothing" test ! -e /root/pwned
check "live database untouched" cmp -s "$DB" "$WORK/live.before"
# A symlink byname planted in its snapshot directory is read as byname, so it
# can't hand root's files to the restore.
sqlite3 /root/secret.db < "$SRC/internal/store/migrations/0001_init.sql"
add_link rootonly /root/secret.db
chmod 0600 /root/secret.db
ln -sf /root/secret.db "$SNAPS/byname-29990101T000000.000Z.db"
LOG="$OUT/restore-symlink.log"
if restore "$SNAPS/byname-29990101T000000.000Z.db"; then bad "restored through a symlink to a root-only file"; else ok "a planted symlink can't read root's files"; fi
check "live database untouched" cmp -s "$DB" "$WORK/live.before"
LOG="$OUT/restore-latest-symlink.log"
restore --latest || true
check "--latest skips the symlink" bash -c "! grep -q 29990101 '$LOG'"
# Nor through a path that reaches byname's directory via a symlink of yours.
mkdir -p /srv
ln -sfn "$SNAPS" /srv/byname-snapshots
LOG="$OUT/restore-via-symlinked-dir.log"
if restore /srv/byname-snapshots/byname-29990101T000000.000Z.db; then bad "restored through a symlinked directory"; else ok "a path through a symlink is read as byname"; fi
rm -f /srv/byname-snapshots
check "rootonly never restored" test "$(sqlite3 "$DB" "SELECT count(*) FROM links WHERE name = 'rootonly';")" == 0
rm -f "$SNAPS/byname-29990101T000000.000Z.db"
is_live v2

LOG="$OUT/restore-list.log"
if restore --list; then ok "restore --list"; else bad "restore --list failed"; fi
expect "$LOG" "+$SNAPS/byname-" "+$SNAPS/pre-restore-"
check "refuses to run as another user" bash -c "! runuser -u admin -- /usr/local/sbin/byname-restore --list"

gzip -c "$SNAP" > "$WORK/backup.db.gz"
LOG="$OUT/restore-gz.log"
if restore "$WORK/backup.db.gz"; then ok "restore from a .gz backup"; else bad "restore from .gz failed"; tail -n 8 "$LOG" | sed 's/^/         /'; fi

printf 'not a database' > "$WORK/junk.db"
cp "$DB" "$WORK/live.before"
: > "$STUB_STATE/systemctl.log"
LOG="$OUT/restore-junk.log"
if restore "$WORK/junk.db"; then bad "restored a file that isn't a database"; else ok "refused a file that isn't a database"; fi
check "live database untouched" cmp -s "$DB" "$WORK/live.before"
check "byname never stopped" bash -c "! grep -q 'stop byname' '$STUB_STATE/systemctl.log'"
is_live v2

# ---------------------------------------------------------------- install.sh rollback details
section "install.sh: rollback details"
CASE=install-rollback
# A broken release whose units differ: the rollback puts the old units back.
cp /etc/systemd/system/byname.service "$WORK/byname.service.before"
release v5-units 2 1 >/dev/null
echo '# changed by v5-units' >> "$WORK/stage-v5-units/byname.service"
if deploy v5-units; then bad "a broken release deployed"; else ok "broken release with new units refused"; fi
check "previous byname.service put back" cmp -s /etc/systemd/system/byname.service "$WORK/byname.service.before"
check "says so" grep -q 'put back the previous systemd units' "$LOG"
is_live v2

# A failing `systemctl restart` (a dependency, a start limit) still rolls back.
release v6-nostart 2 >/dev/null
echo v6-nostart > "$STUB_STATE/fail-start"
if deploy v6-nostart; then bad "deployed although byname didn't start"; else ok "failed restart refused"; fi
rm -f "$STUB_STATE/fail-start"
check "rolled back" grep -q 'rolling back to v2' "$LOG"
check "current -> v2" test "$(current)" == v2
is_live v2

# BYNAME_HOSTS in byname.local.env wins, for the health check too.
cp /etc/byname/byname.local.env "$WORK/local.env.before"
echo 'BYNAME_HOSTS="links,links.lan"' >> /etc/byname/byname.local.env
release v7 2 >/dev/null
if deploy v7; then ok "deploy with BYNAME_HOSTS set in byname.local.env"; else bad "deploy with BYNAME_HOSTS set in byname.local.env failed"; tail -n 8 "$LOG" | sed 's/^/         /'; fi
is_live v7 links
cp "$WORK/local.env.before" /etc/byname/byname.local.env
PATH="$(stub_path)" systemctl restart byname.service
is_live v7

# ---------------------------------------------------------------- byname-verify
section "byname-verify"
run_verify host
expect "$LOG" '+PASS unattended-upgrades enabled' '+PASS sshd trustedusercakeys none' '+PASS input policy drop' \
  '+PASS tailnet addresses dropped unless they arrive on tailscale0' '+PASS byname.service starts after tailscaled' \
  '+PASS byname.service active (v7)'
if [[ $HAVE_IFACES -eq 1 ]]; then
  expect "$LOG" '+PASS health OK over 100.101.102.103' '+PASS foreign Host header refused (421)'
fi
# Backups are judged by what was uploaded, not by the last run's Result
# (systemd says "success" before the first run).
CASE=verify-backups
touch "$STUB_STATE/units/byname-backup.timer.enabled"
NEWEST="$(find "$SNAPS" -maxdepth 1 -type f -name 'byname-*.db' -printf '%f\n' | sort | tail -n 1)"
touch -d '3 hours ago' "$SNAPS/$NEWEST"
rm -f /var/lib/byname-backup/last-upload
run_verify backup-never
expect "$LOG" "+FAIL newest snapshot $NEWEST is" '+last upload: never' '-PASS last backup run'
install -d -m 0700 /var/lib/byname-backup
echo "$NEWEST" > /var/lib/byname-backup/last-upload
run_verify backup-uploaded
expect "$LOG" "+PASS newest snapshot uploaded ($NEWEST)"
echo byname-20000101T000000.000Z.db > /var/lib/byname-backup/last-upload
touch "$SNAPS/$NEWEST"
run_verify backup-pending
expect "$LOG" "+PASS newest snapshot $NEWEST is 0 min old"

# ---------------------------------------------------------------- summary
printf '\n\033[1m== %d checks passed, %d failed\033[0m\n' "$PASSES" "${#FAILED[@]}"
if [[ ${#FAILED[@]} -gt 0 ]]; then
  printf '  %s\n' "${FAILED[@]}"
  echo "Logs and rendered firewalls: $OUT (in the container)"
  exit 1
fi
echo "Rendered firewall variants: $(find "$OUT" -name 'firewall-*.nft' | wc -l)"
