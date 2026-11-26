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
FW_TS=('+udp dport 41641 accept' '+iifname "tailscale0" tcp dport 80 accept')
FW_NO_TS=('-udp dport 41641 accept' '-tailscale0')
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
  expect /etc/ssh/sshd_config.d/01-byname-hardening.conf '+AllowUsers admin' '+PasswordAuthentication no'
  check "host.conf written" test -s /etc/byname/host.conf
  check "byname.service installed" test -f /etc/systemd/system/byname.service
  check "byname-firewall.service enabled" called '^systemctl enable byname-firewall.service'
  check "byname-verify and byname-restore installed" test -x /usr/local/sbin/byname-verify -a -x /usr/local/sbin/byname-restore
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
  if run_bootstrap cloud-tailscale-again ok "${T[@]}" --platform cloud; then
    check "re-run: same byname.env" cmp -s /etc/byname/byname.env "$WORK/env.first"
    check "re-run: same firewall" cmp -s /etc/byname/firewall.nft "$WORK/fw.first"
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

if run_bootstrap cloud-no-tailscale ok "${T[@]}" --platform cloud --no-tailscale --ssh-from 203.0.113.7/32; then
  after_run
  check "warns there's no public address yet" grep -q 'no --public-domain yet' "$LOG"
  env_is BYNAME_NO_WHOIS true
  env_is BYNAME_ALLOW_CIDRS "127.0.0.1/32,::1/128"
  check_firewall "${FW_NO_TS[@]}" "${FW_NO_PUBLIC[@]}" '+ip saddr { 203.0.113.7/32 } tcp dport 22 accept' "!$SSH_ANY"
fi
reset_host

signin
if run_bootstrap cloud-no-tailscale-domain ok "${T[@]}" --platform cloud --no-tailscale --ssh-from any --public-domain me.example.com; then
  after_run
  check_firewall "${FW_NO_TS[@]}" "${FW_PUBLIC[@]}" "~$SSH_ANY"
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

# ---------------------------------------------------------------- restore.sh
section "restore.sh"
CASE=restore
links_in() { sqlite3 "$1" "SELECT group_concat(name, ',') FROM (SELECT name FROM links WHERE deleted_at IS NULL ORDER BY name);"; }
SNAP="$(find "$SNAPS" -name 'byname-*.db' | sort | tail -1)"
sqlite3 "$DB" "INSERT INTO links (name, key, target, created_by, created_at, updated_by, updated_at) VALUES ('later', 'later', 'https://later.example', 'test', 0, 'test', 0);"
LOG="$OUT/restore-snapshot.log"
if PATH="$(stub_path)" byname-restore "$SNAP" > "$LOG" 2>&1; then ok "restore from a snapshot"; else bad "restore failed"; tail -n 8 "$LOG" | sed 's/^/         /'; fi
check "restored links: wiki only" test "$(links_in "$DB")" == wiki
is_live v2

gzip -c "$SNAP" > "$WORK/backup.db.gz"
LOG="$OUT/restore-gz.log"
if PATH="$(stub_path)" byname-restore "$WORK/backup.db.gz" > "$LOG" 2>&1; then ok "restore from a .gz backup"; else bad "restore from .gz failed"; tail -n 8 "$LOG" | sed 's/^/         /'; fi

printf 'not a database' > "$WORK/junk.db"
cp "$DB" "$WORK/live.before"
LOG="$OUT/restore-junk.log"
if PATH="$(stub_path)" byname-restore "$WORK/junk.db" > "$LOG" 2>&1; then bad "restored a file that isn't a database"; else ok "refused a file that isn't a database"; fi
check "live database untouched" cmp -s "$DB" "$WORK/live.before"
is_live v2

# ---------------------------------------------------------------- summary
printf '\n\033[1m== %d checks passed, %d failed\033[0m\n' "$PASSES" "${#FAILED[@]}"
if [[ ${#FAILED[@]} -gt 0 ]]; then
  printf '  %s\n' "${FAILED[@]}"
  echo "Logs and rendered firewalls: $OUT (in the container)"
  exit 1
fi
echo "Rendered firewall variants: $(find "$OUT" -name 'firewall-*.nft' | wc -l)"
