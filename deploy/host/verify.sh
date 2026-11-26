#!/usr/bin/env bash
# shellcheck disable=SC2015 # pass/warn/fail always succeed, so `test && pass || fail` is safe here.
# byname-verify — check this host matches the byname security baseline (milestone M0).
# Run on the host (Pi or cloud server): sudo byname-verify
# Exit status is the number of failed checks (0 = all good).
set -uo pipefail

FAILS=0
pass() { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; FAILS=$((FAILS + 1)); }
warn() { printf '  \033[33mWARN\033[0m %s\n' "$*"; }
section() { printf '\n\033[1m%s\033[0m\n' "$*"; }

[[ $EUID -eq 0 ]] || { echo "run as root: sudo byname-verify" >&2; exit 1; }

# What bootstrap.sh recorded about this host. Parsed, not sourced.
conf() { sed -n "s/^$1=//p" /etc/byname/host.conf 2>/dev/null | head -1; }
PLATFORM="$(conf PLATFORM)"; PLATFORM="${PLATFORM:-pi}"
FIRST_HOST="$(conf NAME)"
if [[ -r /etc/byname/byname.env ]]; then
  FIRST_HOST="$(sed -n 's/^BYNAME_HOSTS=\([^,]*\).*/\1/p' /etc/byname/byname.env)"
fi
FIRST_HOST="${FIRST_HOST:-me}"
TAG="$(conf TAG)"; TAG="${TAG:-tag:$FIRST_HOST}"
TAILSCALE="$(conf TAILSCALE)"; TAILSCALE="${TAILSCALE:-1}"
PUBLIC_DOMAIN="$(conf PUBLIC_DOMAIN)"

section "Platform ($PLATFORM)"
if [[ "$PLATFORM" == pi ]]; then
  [[ "$(uname -m)" == "aarch64" ]] && pass "64-bit kernel ($(uname -r))" || fail "not aarch64: $(uname -m)"
  if [[ -e /sys/class/net/wlan0 ]]; then fail "wlan0 present (Wi-Fi not disabled; reboot after bootstrap?)"; else pass "Wi-Fi disabled"; fi
  if [[ -n "$(ls -A /sys/class/bluetooth 2>/dev/null)" ]]; then fail "Bluetooth adapter present"; else pass "Bluetooth disabled"; fi
  throttled="$(cat /sys/devices/platform/soc/soc:firmware/get_throttled 2>/dev/null || echo)"
  if [[ -n "$throttled" ]]; then
    v=$((16#${throttled#0x}))
    if (( v & 0x1 )); then fail "under-voltage NOW (use the official 5.1 V / 2.5 A supply)"
    elif (( v & 0x10000 )); then warn "under-voltage occurred since boot (0x$throttled)"
    else pass "power OK (get_throttled=0x$throttled)"; fi
  fi
else
  pass "$(sed -n 's/^PRETTY_NAME=//p' /etc/os-release | tr -d '"') on $(uname -m), $(awk '/^MemTotal:/ {printf "%d MB RAM", $2/1024}' /proc/meminfo)"
fi
if [[ "$(timedatectl show -p NTPSynchronized --value 2>/dev/null)" == "yes" ]]; then pass "clock synchronised"; else fail "clock not NTP-synchronised"; fi

section "SSH"
sshcfg="$(sshd -T 2>/dev/null)"
for kv in "passwordauthentication no" "permitrootlogin no" "kbdinteractiveauthentication no" "trustedusercakeys none"; do
  if grep -qx "$kv" <<<"$sshcfg"; then pass "sshd $kv"; else fail "sshd should have: $kv"; fi
done

section "Updates and storage"
if apt-config dump 2>/dev/null | grep -q 'APT::Periodic::Unattended-Upgrade "1"'; then pass "unattended-upgrades enabled"; else fail "unattended-upgrades not enabled"; fi
origins=("Debian")
[[ "$TAILSCALE" == 1 ]] && origins+=("Tailscale")
[[ "$PLATFORM" == pi ]] && origins+=("Raspberry Pi Foundation")
for origin in "${origins[@]}"; do
  if apt-cache policy 2>/dev/null | grep -q "o=$origin,"; then pass "apt origin '$origin' present (matches upgrade pattern)"; else warn "apt origin '$origin' not seen; check 52byname-unattended-upgrades patterns against 'apt-cache policy'"; fi
done
journal="$(systemd-analyze cat-config systemd/journald.conf 2>/dev/null)"
if [[ "$PLATFORM" == pi ]]; then
  grep -q '^Storage=volatile' <<<"$journal" && pass "journal in RAM" || fail "journald not volatile"
  if swapon --show=TYPE --noheadings 2>/dev/null | grep -qx file; then warn "file-backed swap active (SD wear)"; else pass "no file-backed swap"; fi
  findmnt -no OPTIONS / | grep -qw noatime && pass "root mounted noatime" || warn "root not mounted noatime"
else
  grep -q '^SystemMaxUse=' <<<"$journal" && pass "journal size capped" || warn "journal size not capped"
  if [[ -n "$(swapon --show --noheadings 2>/dev/null)" ]] || awk '/^MemTotal:/ {exit !($2 >= 1500000)}' /proc/meminfo; then
    pass "memory headroom (swap or ≥ 1.5 GB RAM)"
  else
    warn "no swap on a small server; re-run bootstrap.sh"
  fi
fi

section "Firewall"
if nft list table inet byname_filter >/dev/null 2>&1; then pass "table inet byname_filter loaded"; else fail "byname firewall not loaded (systemctl status byname-firewall)"; fi
if nft list chain inet byname_filter input 2>/dev/null | grep -q 'policy drop'; then pass "input policy drop"; else fail "input policy is not drop"; fi
if systemctl is-enabled --quiet nftables.service 2>/dev/null; then fail "Debian nftables.service enabled (its 'flush ruleset' wipes tailscale rules)"; else pass "Debian nftables.service disabled"; fi
# LAN access must be on (or off) consistently in all three layers.
LAN_MODE=off
[[ -f /etc/systemd/system/byname.service.d/10-lan-access.conf ]] && LAN_MODE=on
lan_nft=off; nft list chain inet byname_filter input 2>/dev/null | grep -q 'ip saddr .* tcp dport 80 accept' && lan_nft=on
# systemd reads byname.local.env after byname.env, so its value wins.
ALLOW_CIDRS="$(sed -n 's/^BYNAME_ALLOW_CIDRS=//p' /etc/byname/byname.env /etc/byname/byname.local.env 2>/dev/null | tail -1)"
lan_env=off; printf '%s\n' "$ALLOW_CIDRS" | tr ',' '\n' | grep -vE '^(100\.64\.0\.0/10|fd7a:115c:a1e0::/48|127\.0\.0\.1/32|::1/128)$' | grep -q . && lan_env=on
if ! [[ "$LAN_MODE" == "$lan_nft" && "$LAN_MODE" == "$lan_env" ]]; then
  fail "LAN access inconsistent: systemd drop-in=$LAN_MODE firewall=$lan_nft BYNAME_ALLOW_CIDRS=$lan_env; re-run bootstrap.sh"
elif [[ "$PLATFORM" == cloud && "$LAN_MODE" == on ]]; then
  fail "LAN access is on, on a cloud server (that would be the provider's network); re-run bootstrap.sh"
elif [[ "$PLATFORM" == cloud ]]; then
  pass "no LAN access (firewall, systemd and app agree)"
else
  pass "LAN access $LAN_MODE (firewall, systemd and app agree)"
fi
echo "       listening sockets:"
ss -Hltnu | awk '{printf "         %-5s %s\n", $1, $5}'

section "Tailscale"
TS_IP="?"
if [[ "$TAILSCALE" == 0 ]]; then
  pass "not used (bootstrap.sh --no-tailscale)"
  grep -q '^BYNAME_NO_WHOIS=true' /etc/byname/byname.env && pass "WhoIs off" || fail "BYNAME_NO_WHOIS isn't true without Tailscale (re-run bootstrap.sh)"
  if systemctl is-active --quiet tailscaled 2>/dev/null; then
    fail "tailscaled is running without Tailscale configured: Tailscale SSH bypasses the firewall (sudo tailscale down; sudo systemctl mask --now tailscaled)"
  fi
  if command -v tailscaled >/dev/null 2>&1; then
    if [[ "$(systemctl is-enabled tailscaled.service 2>/dev/null)" == masked ]]; then
      pass "tailscaled masked (nothing can start it)"
    else
      fail "tailscaled is installed but not masked, so something could start it (sudo systemctl mask --now tailscaled, or re-run bootstrap.sh)"
    fi
  fi
  wants="$(systemctl show byname.service -p Wants --value 2>/dev/null)"
  if [[ " $wants " == *" tailscaled.service "* ]]; then
    fail "byname.service wants tailscaled.service, which starts it on every restart (re-run bootstrap.sh)"
  else
    pass "byname.service doesn't pull in tailscaled"
  fi
  if grep -qE '100\.64\.0\.0/10|fd7a:115c' <<<"$ALLOW_CIDRS"; then
    fail "BYNAME_ALLOW_CIDRS still trusts tailnet ranges without Tailscale (re-run bootstrap.sh)"
  else
    pass "tailnet ranges not trusted"
  fi
else
input_chain="$(nft list chain inet byname_filter input 2>/dev/null)"
if grep -q 'iifname != "tailscale0" ip saddr 100.64.0.0/10 drop' <<<"$input_chain" &&
  grep -q 'iifname != "tailscale0" ip6 saddr fd7a:115c:a1e0::/48 drop' <<<"$input_chain"; then
  pass "tailnet addresses dropped unless they arrive on tailscale0"
else
  fail "the firewall doesn't drop tailnet addresses arriving outside tailscale0 (re-run bootstrap.sh)"
fi
after="$(systemctl show byname.service -p After --value 2>/dev/null)"
[[ " $after " == *" tailscaled.service "* ]] && pass "byname.service starts after tailscaled" ||
  warn "byname.service isn't ordered after tailscaled (re-run bootstrap.sh to add byname.service.d/05-tailscale.conf)"
ts_json="$(tailscale status --json 2>/dev/null || echo '{}')"
ts_check="$(python3 -c '
import json, sys
d = json.loads(sys.stdin.read() or "{}")
s = d.get("Self") or {}
print(d.get("BackendState", "?"))
print(s.get("HostName", "?"))
print(",".join(s.get("Tags") or []))
print((s.get("TailscaleIPs") or ["?"])[0])
print(d.get("MagicDNSSuffix", ""))
' <<<"$ts_json")"
mapfile -t ts <<<"$ts_check"
[[ "${ts[0]}" == "Running" ]] && pass "tailscaled running" || fail "tailscale state: ${ts[0]} (run: sudo tailscale up --ssh --hostname=$FIRST_HOST --advertise-tags=$TAG)"
[[ "${ts[1],,}" == "${FIRST_HOST,,}" ]] && pass "tailnet hostname ${ts[1]}" || fail "tailnet hostname is '${ts[1]}', expected '$FIRST_HOST' (is another device already named $FIRST_HOST?)"
[[ ",${ts[2]}," == *",$TAG,"* ]] && pass "node tagged $TAG (no key expiry)" || warn "node not tagged $TAG (tags: '${ts[2]}'); its key will expire"
TS_IP="${ts[3]}"
if tailscale debug prefs 2>/dev/null | grep -q '"RunSSH": true'; then pass "Tailscale SSH enabled"; else warn "Tailscale SSH off (tailscale set --ssh)"; fi
if grep -q "${ts[4]:-no-suffix}" /etc/byname/byname.env 2>/dev/null; then pass "BYNAME_HOSTS includes the tailnet FQDN"; else warn "BYNAME_HOSTS lacks $FIRST_HOST.${ts[4]} (re-run bootstrap.sh)"; fi
fi

section "byname service"
if systemctl is-active --quiet byname.service; then pass "byname.service active ($(readlink /usr/local/lib/byname/current))"; else fail "byname.service not active (journalctl -u byname)"; fi
score="$(systemd-analyze security byname.service --no-pager 2>/dev/null | sed -n 's/.*Overall exposure level for byname.service: \([0-9.]*\).*/\1/p')"
if [[ -n "$score" ]] && awk -v s="$score" 'BEGIN{exit !(s <= 2.0)}'; then pass "sandbox exposure $score (≤ 2.0)"; else fail "sandbox exposure '${score:-?}' (want ≤ 2.0): systemd-analyze security byname.service"; fi
# Over the tailnet address with Tailscale, else over localhost.
CHECK_IP="127.0.0.1"
[[ "$TS_IP" != "?" ]] && CHECK_IP="$TS_IP"
health="$(curl -fsS -m 5 -H "Host: $FIRST_HOST" "http://$CHECK_IP/_/healthz" 2>&1)"
if grep -q '"status": "ok"' <<<"$health"; then pass "health OK over $CHECK_IP"; else fail "health check failed: $health"; fi
if grep -q '"low_disk": true' <<<"$health"; then fail "low disk space for /var/lib/byname: $(grep -o '"disk_free_mb": [0-9]*' <<<"$health")"; fi
if [[ "$TS_IP" != "?" ]]; then
  who="$(curl -fsS -m 5 -H "Host: $FIRST_HOST" "http://$TS_IP/_/whoami" 2>&1)"
  if grep -q 'Tailscale user' <<<"$who"; then pass "WhoIs works from the sandboxed service"; else fail "WhoIs not working from the service (see http://$FIRST_HOST/_/whoami)"; fi
fi
code="$(curl -s -o /dev/null -w '%{http_code}' -m 5 -H 'Host: evil.example' "http://$CHECK_IP/_/healthz")"
[[ "$code" == "421" ]] && pass "foreign Host header refused (421)" || fail "foreign Host header got $code, want 421"

# Public address: your domain, or Tailscale Funnel. byname.local.env overrides byname.env.
PUBLIC_URL="$(sed -n 's/^BYNAME_PUBLIC_URL=//p' /etc/byname/byname.env /etc/byname/byname.local.env 2>/dev/null | tail -1)"
SOCK="$(sed -n 's/^BYNAME_PUBLIC_LISTEN=unix://p' /etc/byname/byname.env /etc/byname/byname.local.env 2>/dev/null | tail -1)"
SOCK="${SOCK:-/run/byname/public.sock}"
# Everything Tailscale Serve/Funnel publishes on this host. Anything other
# than byname's socket would skip byname's sign-in.
serve_check="$( { command -v tailscale >/dev/null && tailscale serve status --json 2>/dev/null; } | python3 -c '
import json, sys
sock = "unix:" + sys.argv[1]
try:
    d = json.loads(sys.stdin.read() or "{}") or {}
except ValueError:
    d = {}
funnel = any((d.get("AllowFunnel") or {}).values())
targets, others = 0, []
for hp, web in (d.get("Web") or {}).items():
    for path, h in ((web or {}).get("Handlers") or {}).items():
        t = h.get("Proxy") or h.get("Path") or ("text" if h.get("Text") else "?")
        if t == sock:
            targets += 1
        else:
            others.append(hp + path + " -> " + t)
for port, h in (d.get("TCP") or {}).items():
    if (h or {}).get("TCPForward"):
        others.append("tcp " + port + " -> " + h["TCPForward"])
print(int(funnel))
print(targets)
print("; ".join(others))
' "$SOCK")"
mapfile -t serve <<<"$serve_check"
if [[ -z "$PUBLIC_URL" && "${serve[0]:-0}" == 1 ]]; then
  section "Public address"
  fail "Tailscale Funnel is on, but byname's public address is off: whatever it publishes skips sign-in (tailscale funnel status; turn it off with: tailscale funnel reset)"
fi
if [[ -n "$PUBLIC_URL" ]]; then
  section "Public address ($PUBLIC_URL)"
  PUBLIC_HOST="${PUBLIC_URL#https://}"
  [[ -f /etc/systemd/system/byname.service.d/20-public.conf ]] && pass "drop-in 20-public.conf present" || fail "20-public.conf missing (re-run bootstrap.sh)"
  systemctl show byname.service -p Requires --value | grep -qw byname-firewall.service && pass "byname.service requires the firewall" || fail "byname.service doesn't require byname-firewall.service (re-run bootstrap.sh)"
  [[ -z "${serve[2]:-}" ]] && pass "nothing else published by Tailscale Serve/Funnel" || fail "Serve/Funnel also publishes: ${serve[2]} (these skip sign-in; remove them)"
  if [[ -n "$PUBLIC_DOMAIN" ]]; then
    # Served directly: DNS, the firewall, a real certificate, and byname's gate.
    resolved="$(getent ahosts "$PUBLIC_DOMAIN" | awk '{print $1}' | sort -u | paste -sd' ' -)"
    [[ -n "$resolved" ]] && pass "$PUBLIC_DOMAIN resolves to $resolved" || fail "$PUBLIC_DOMAIN doesn't resolve: $([[ "$PLATFORM" == pi ]] && echo "point its A record at your home's public IP address (dynamic DNS if that changes)" || echo "create its A/AAAA records (tofu output dns_records)")"
    nft list chain inet byname_filter input 2>/dev/null | grep -q 'tcp dport { 80, 443 } accept' && pass "firewall open for tcp/80 and tcp/443" || fail "firewall doesn't open tcp/80 and tcp/443 (re-run bootstrap.sh --public-domain)"
    [[ "${serve[0]:-0}" == 1 ]] && fail "Tailscale Funnel is on as well; turn it off: tailscale funnel reset"
    # Through the real TLS listener, with certificate checks. The first
    # request may wait while Let's Encrypt issues the certificate.
    out="$(curl -sS -o /dev/null -m 60 -w '%{http_code} %{redirect_url}' --resolve "$PUBLIC_DOMAIN:443:127.0.0.1" "https://$PUBLIC_DOMAIN/_/links" 2>&1)"
    if [[ "$out" == 303* && "$out" == *"/_/signin?next="* ]]; then
      pass "https://$PUBLIC_DOMAIN has a valid certificate and asks for sign-in"
    else
      fail "https://$PUBLIC_DOMAIN: '$out' (want 303 to /_/signin; certificate problems usually mean DNS doesn't point here yet: journalctl -u byname | grep -i acme)"
    fi
    # Port 80 from a peer the private listener doesn't trust must only
    # redirect. Use this host's own address if it isn't trusted. On a Pi with
    # --lan-access, or with Tailscale, the LAN and tailnet addresses are
    # trusted (they'd get the private page), so use 127.0.0.2: loopback, but
    # not the one localhost address byname allows.
    own="$(ip -o addr show scope global | awk '$2 !~ /^tailscale/ {print $4}' | cut -d/ -f1 | python3 -c '
import ipaddress, sys
nets = [ipaddress.ip_network(n) for n in sys.argv[1].split(",") if n]
for line in sys.stdin:
    a = line.strip()
    if not a:
        continue
    ip = ipaddress.ip_address(a)
    if not any(ip in n for n in nets if n.version == ip.version):
        print(a)
        break
' "$ALLOW_CIDRS")"
    probe=(); probe_from="from $own"
    if [[ -n "$own" ]]; then
      [[ "$own" == *:* ]] && own="[$own]"
    else
      own=127.0.0.1; probe=(--interface 127.0.0.2); probe_from="from 127.0.0.2 (every address of this host is trusted)"
    fi
    out="$(curl -s -o /dev/null -m 5 -w '%{http_code} %{redirect_url}' "${probe[@]}" -H "Host: $FIRST_HOST" "http://$own/_/new")"
    [[ "$out" == "301 https://$PUBLIC_DOMAIN/_/new" ]] && pass "port 80 only redirects untrusted peers to https" ||
      fail "port 80 $probe_from got '$out', want 301 to https://$PUBLIC_DOMAIN/_/new (check BYNAME_ALLOW_CIDRS)"
    if [[ "$LAN_MODE" == on ]]; then
      # A trusted LAN peer gets the private page, except on the public name.
      lan_ip="$(ip -4 -o addr show scope global | awk '$2 !~ /^tailscale/ {print $4}' | cut -d/ -f1 | head -1)"
      if [[ -n "$lan_ip" ]]; then
        out="$(curl -s -o /dev/null -m 5 -w '%{http_code} %{redirect_url}' -H "Host: $PUBLIC_DOMAIN" "http://$lan_ip/_/new")"
        [[ "$out" == "301 https://$PUBLIC_DOMAIN/_/new" ]] && pass "LAN peers on the public name are redirected to https" ||
          fail "LAN address $lan_ip with Host $PUBLIC_DOMAIN got '$out', want 301 to https://$PUBLIC_DOMAIN/_/new"
      fi
    fi
    days="$(echo | openssl s_client -connect 127.0.0.1:443 -servername "$PUBLIC_DOMAIN" 2>/dev/null | openssl x509 -noout -enddate 2>/dev/null | sed 's/notAfter=//')"
    [[ -n "$days" ]] && pass "certificate valid until $days (byname renews it automatically)"
    perm="$(stat -c '%a %U' /var/lib/byname/acme 2>/dev/null)"
    [[ "$perm" == "700 byname" ]] && pass "/var/lib/byname/acme is 0700, owner byname" || fail "/var/lib/byname/acme is '${perm:-missing}', want '700 byname'"
  else
    if [[ "${serve[0]:-0}" == 1 && "${serve[1]:-0}" -gt 0 ]]; then pass "Tailscale Funnel → $SOCK"; else fail "Funnel isn't publishing $SOCK (tailscale funnel status; re-run bootstrap.sh --funnel)"; fi
    perm="$(stat -c '%a %U' "$(dirname "$SOCK")" 2>/dev/null)"
    [[ "$perm" == "700 byname" ]] && pass "$(dirname "$SOCK") is 0700, owner byname" || fail "$(dirname "$SOCK") is '${perm:-missing}', want '700 byname'"
    perm="$(stat -c '%a %U' "$SOCK" 2>/dev/null)"
    [[ "$perm" == "600 byname" ]] && pass "socket is 0600, owner byname" || fail "$SOCK is '${perm:-missing}', want '600 byname'"
  fi
  providers=0
  for p in google slack; do
    P="${p^^}"
    grep -Eq "^BYNAME_${P}_CLIENT_ID=.+" /etc/byname/byname.local.env 2>/dev/null || continue
    providers=$((providers + 1))
    secret="/etc/credstore/byname.$p-client-secret"
    perm="$(stat -c '%a %U' "$secret" 2>/dev/null)"
    [[ "$perm" == "600 root" || "$perm" == "400 root" ]] && pass "$p client secret ($secret, $perm)" || fail "$secret is '${perm:-missing}', want 0600 root"
    if [[ $p == google ]]; then endpoint=https://oauth2.googleapis.com/token; else endpoint=https://slack.com/api/openid.connect.token; fi
    code="$(curl -s -o /dev/null -m 10 -w '%{http_code}' -X POST "$endpoint")"
    if [[ "$code" =~ ^[1-5][0-9][0-9]$ ]]; then
      pass "$p token endpoint reachable (HTTP $code)"
    elif [[ $p == slack ]] && ! ip -4 route show default | grep -q .; then
      fail "slack.com has no IPv6 address and this server has no IPv4 route: Sign in with Slack can't work (set ipv4 = true in deploy/cloud)"
    else
      fail "$p token endpoint unreachable from this host ($endpoint)"
    fi
  done
  [[ $providers -gt 0 ]] && pass "$providers sign-in provider(s) configured" || fail "no sign-in provider in /etc/byname/byname.local.env (byname won't start)"
  # Funnel: byname's own gate, through the socket, with the headers tailscaled
  # would add.
  if [[ -z "$PUBLIC_DOMAIN" ]]; then
  hdr=(-H "X-Forwarded-Host: $PUBLIC_HOST" -H 'X-Forwarded-Proto: https')
  out="$(curl -s -o /dev/null -m 5 -w '%{http_code} %{redirect_url}' --unix-socket "$SOCK" "${hdr[@]}" \
    -H 'X-Forwarded-For: 192.0.2.1' -H 'Tailscale-Funnel-Request: ?1' http://localhost/_/links)"
  [[ "$out" == 303* && "$out" == *"/_/signin?next="* ]] && pass "internet request without sign-in → sign-in page" || fail "internet request without sign-in got '$out', want 303 to /_/signin"
  if [[ "$TS_IP" != "?" ]]; then
    code="$(curl -s -o /dev/null -m 5 -w '%{http_code}' --unix-socket "$SOCK" "${hdr[@]}" -H "X-Forwarded-For: $TS_IP" http://localhost/_/healthz)"
    [[ "$code" == 200 ]] && pass "tailnet request through Serve skips sign-in" || fail "tailnet request through Serve got $code, want 200"
  fi
  code="$(curl -s -o /dev/null -m 5 -w '%{http_code}' --unix-socket "$SOCK" -H 'X-Forwarded-Host: evil.example' http://localhost/)"
  [[ "$code" == 421 ]] && pass "foreign forwarded host refused (421)" || fail "foreign forwarded host got $code, want 421"
  fi
fi

if nft list chain inet byname_filter input 2>/dev/null | grep -qE '^\s*tcp dport 22 accept'; then
  warn "the host firewall lets SSH in from anywhere (--ssh-from any): the provider firewall (ssh_cidrs) must limit it"
fi

section "Backups"
if systemctl is-enabled --quiet byname-backup.timer 2>/dev/null; then
  pass "byname-backup.timer enabled"
  last="$(systemctl show byname-backup.service -p Result --value)"
  [[ "$last" == "success" ]] && pass "last backup run: success" || warn "last backup result: $last (journalctl -u byname-backup)"
else
  warn "backups not configured (docs/operations.md → Backups)"
fi
snaps="$(find /var/lib/byname/snapshots -name 'byname-*.db' 2>/dev/null | wc -l)"
[[ "$snaps" -gt 0 ]] && pass "$snaps local snapshot(s)" || warn "no local snapshots yet"

section "Checks to run from other devices"
if [[ "$TAILSCALE" == 1 ]]; then
  cat <<EOF
  * From each household device (Tailscale on): open  http://$FIRST_HOST/_/whoami
    and type  $FIRST_HOST/test  in the address bar. Record results in docs/client-setup.md.
EOF
fi
if [[ -n "$PUBLIC_URL" ]]; then
  cat <<EOF
  * Public address: on a phone with Wi-Fi$([[ "$TAILSCALE" == 1 ]] && echo ' AND Tailscale') off, open  $PUBLIC_URL/_/whoami
    It must ask you to sign in, and refuse an account that isn't on the allow list.
EOF
fi
if [[ -n "$PUBLIC_DOMAIN" ]]; then
  cat <<EOF
  * http://$PUBLIC_DOMAIN/wiki must redirect to https://$PUBLIC_DOMAIN/wiki.
EOF
  if [[ "$PLATFORM" == pi ]]; then
    cat <<EOF
  * Your router must forward only tcp/80 and tcp/443 to this Pi. From outside your home
    network (phone on mobile data), nc -vz -w 5 $PUBLIC_DOMAIN 22  must time out.
EOF
  else
    cat <<EOF
  * From the internet, only tcp/80 and tcp/443 should answer (plus tcp/22 from your
    --ssh-from addresses only): from elsewhere,  nc -vz -w 5 $PUBLIC_DOMAIN 22  must time out.
EOF
  fi
  if [[ "$PLATFORM" == pi && "$LAN_MODE" == on ]]; then
    cat <<EOF
  * LAN access is ON next to a public domain. From outside your home network, this must
    redirect to https, never show the "home network (no Tailscale)" page. If it shows that
    page, your router rewrites the source address of forwarded connections: turn LAN access
    off (re-run bootstrap.sh without --lan-access).
        curl -m 5 -i -H 'Host: $FIRST_HOST' http://$PUBLIC_DOMAIN/_/whoami
EOF
  fi
elif [[ "$PLATFORM" == cloud ]]; then
  pub6="$(ip -6 -o addr show scope global | awk '$2 != "tailscale0" {print $4}' | cut -d/ -f1 | head -1)"
  pub4="$(ip -4 -o addr show scope global | awk '$2 != "tailscale0" {print $4}' | cut -d/ -f1 | head -1)"
  cat <<EOF
  * From the internet (Tailscale OFF), these must time out, not be refused:
        curl -m 5 'http://[${pub6:-<public-ipv6>}]/'      nc -vz -w 5 ${pub6:-<public-ipv6>} 22
EOF
  if [[ -n "$pub4" ]]; then
    echo "        curl -m 5 http://$pub4/   (private address $pub4; check the provider's public IPv4 too)"
  fi
fi
if [[ "$PLATFORM" == pi ]]; then
  lan_ip="$(ip -4 -o addr show scope global | awk '$2 != "tailscale0" {print $4}' | cut -d/ -f1 | head -1)"
  if [[ "$LAN_MODE" == on ]]; then
    cat <<EOF
  * LAN access is ON. From a device on home Wi-Fi with Tailscale OFF, both should work:
        nslookup $FIRST_HOST          (router DNS must answer ${lan_ip:-<pi-lan-ip>})
        curl -m 5 http://$FIRST_HOST/_/healthz
    If you have a guest Wi-Fi, from it this should time out:  curl -m 5 http://${lan_ip:-<pi-lan-ip>}/
EOF
  else
    cat <<EOF
  * LAN access is OFF. From a LAN device with Tailscale OFF, this must time out:
        curl -m 5 http://${lan_ip:-<pi-lan-ip>}/
EOF
  fi
fi

echo
if [[ $FAILS -eq 0 ]]; then printf '\033[32mAll checks passed.\033[0m\n'; else printf '\033[31m%d check(s) failed.\033[0m\n' "$FAILS"; fi
exit "$FAILS"
