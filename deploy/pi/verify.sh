#!/usr/bin/env bash
# shellcheck disable=SC2015 # pass/warn/fail always succeed, so `test && pass || fail` is safe here.
# ot-verify — check the Pi matches the ot security baseline (milestone M0).
# Run on the Pi: sudo ot-verify
# Exit status is the number of failed checks (0 = all good).
set -uo pipefail

FAILS=0
pass() { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; FAILS=$((FAILS + 1)); }
warn() { printf '  \033[33mWARN\033[0m %s\n' "$*"; }
section() { printf '\n\033[1m%s\033[0m\n' "$*"; }

[[ $EUID -eq 0 ]] || { echo "run as root: sudo ot-verify" >&2; exit 1; }

FIRST_HOST="ot"
if [[ -r /etc/ot/ot.env ]]; then
  FIRST_HOST="$(sed -n 's/^OT_HOSTS=\([^,]*\).*/\1/p' /etc/ot/ot.env)"
fi

section "Platform"
[[ "$(uname -m)" == "aarch64" ]] && pass "64-bit kernel ($(uname -r))" || fail "not aarch64: $(uname -m)"
if [[ -e /sys/class/net/wlan0 ]]; then fail "wlan0 present (Wi-Fi not disabled; reboot after bootstrap?)"; else pass "Wi-Fi disabled"; fi
if [[ -n "$(ls -A /sys/class/bluetooth 2>/dev/null)" ]]; then fail "Bluetooth adapter present"; else pass "Bluetooth disabled"; fi
if [[ "$(timedatectl show -p NTPSynchronized --value 2>/dev/null)" == "yes" ]]; then pass "clock synchronised"; else fail "clock not NTP-synchronised (no RTC on the Pi)"; fi
throttled="$(cat /sys/devices/platform/soc/soc:firmware/get_throttled 2>/dev/null || echo)"
if [[ -n "$throttled" ]]; then
  v=$((16#${throttled#0x}))
  if (( v & 0x1 )); then fail "under-voltage NOW (use the official 5.1 V / 2.5 A supply)"
  elif (( v & 0x10000 )); then warn "under-voltage occurred since boot (0x$throttled)"
  else pass "power OK (get_throttled=0x$throttled)"; fi
fi

section "SSH"
sshcfg="$(sshd -T 2>/dev/null)"
for kv in "passwordauthentication no" "permitrootlogin no" "kbdinteractiveauthentication no"; do
  if grep -qx "$kv" <<<"$sshcfg"; then pass "sshd $kv"; else fail "sshd should have: $kv"; fi
done

section "Updates and storage"
if apt-config dump 2>/dev/null | grep -q 'APT::Periodic::Unattended-Upgrade "1"'; then pass "unattended-upgrades enabled"; else fail "unattended-upgrades not enabled"; fi
for origin in "Raspberry Pi Foundation" "Tailscale"; do
  if apt-cache policy 2>/dev/null | grep -q "o=$origin"; then pass "apt origin '$origin' present (matches upgrade pattern)"; else warn "apt origin '$origin' not seen; check 52ot-unattended-upgrades patterns against 'apt-cache policy'"; fi
done
if systemd-analyze cat-config systemd/journald.conf 2>/dev/null | grep -q '^Storage=volatile'; then pass "journal in RAM"; else fail "journald not volatile"; fi
if swapon --show=TYPE --noheadings 2>/dev/null | grep -qx file; then warn "file-backed swap active (SD wear)"; else pass "no file-backed swap"; fi
findmnt -no OPTIONS / | grep -qw noatime && pass "root mounted noatime" || warn "root not mounted noatime"

section "Firewall"
if nft list table inet ot_filter >/dev/null 2>&1; then pass "table inet ot_filter loaded"; else fail "ot firewall not loaded (systemctl status ot-firewall)"; fi
if nft list chain inet ot_filter input 2>/dev/null | grep -q 'policy drop'; then pass "input policy drop"; else fail "input policy is not drop"; fi
if systemctl is-enabled --quiet nftables.service 2>/dev/null; then fail "Debian nftables.service enabled (its 'flush ruleset' wipes tailscale rules)"; else pass "Debian nftables.service disabled"; fi
echo "       listening sockets:"
ss -Hltnu | awk '{printf "         %-5s %s\n", $1, $5}'

section "Tailscale"
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
[[ "${ts[0]}" == "Running" ]] && pass "tailscaled running" || fail "tailscale state: ${ts[0]} (run: sudo tailscale up --ssh --hostname=ot --advertise-tags=tag:ot)"
[[ "${ts[1],,}" == "${FIRST_HOST,,}" ]] && pass "tailnet hostname ${ts[1]}" || fail "tailnet hostname is '${ts[1]}', expected '$FIRST_HOST'"
[[ ",${ts[2]}," == *",tag:ot,"* ]] && pass "node tagged tag:ot (no key expiry)" || warn "node not tagged tag:ot (tags: '${ts[2]}'); its key will expire"
TS_IP="${ts[3]}"
if tailscale debug prefs 2>/dev/null | grep -q '"RunSSH": true'; then pass "Tailscale SSH enabled"; else warn "Tailscale SSH off (tailscale set --ssh)"; fi
if grep -q "${ts[4]:-no-suffix}" /etc/ot/ot.env 2>/dev/null; then pass "OT_HOSTS includes the tailnet FQDN"; else warn "OT_HOSTS lacks $FIRST_HOST.${ts[4]} (re-run bootstrap.sh)"; fi

section "ot service"
if systemctl is-active --quiet ot.service; then pass "ot.service active ($(readlink /usr/local/lib/ot/current))"; else fail "ot.service not active (journalctl -u ot)"; fi
score="$(systemd-analyze security ot.service --no-pager 2>/dev/null | sed -n 's/.*Overall exposure level for ot.service: \([0-9.]*\).*/\1/p')"
if [[ -n "$score" ]] && awk -v s="$score" 'BEGIN{exit !(s <= 2.0)}'; then pass "sandbox exposure $score (≤ 2.0)"; else fail "sandbox exposure '${score:-?}' (want ≤ 2.0): systemd-analyze security ot.service"; fi
if [[ "$TS_IP" != "?" ]]; then
  health="$(curl -fsS -m 5 -H "Host: $FIRST_HOST" "http://$TS_IP/_/healthz" 2>&1)"
  if grep -q '"status": "ok"' <<<"$health"; then pass "health OK over the tailnet address ($TS_IP)"; else fail "health check failed: $health"; fi
  if grep -q '"low_disk": true' <<<"$health"; then fail "low disk space for /var/lib/ot: $(grep -o '"disk_free_mb": [0-9]*' <<<"$health")"; fi
  who="$(curl -fsS -m 5 -H "Host: $FIRST_HOST" "http://$TS_IP/_/whoami" 2>&1)"
  if grep -q 'Tailscale user' <<<"$who"; then pass "WhoIs works from the sandboxed service"; else fail "WhoIs not working from the service (see http://$FIRST_HOST/_/whoami)"; fi
  code="$(curl -s -o /dev/null -w '%{http_code}' -m 5 -H 'Host: evil.example' "http://$TS_IP/")"
  [[ "$code" == "421" ]] && pass "foreign Host header refused (421)" || fail "foreign Host header got $code, want 421"
fi
lan_ip="$(ip -4 -o addr show scope global | awk '$2 != "tailscale0" {print $4}' | cut -d/ -f1 | head -1)"

section "Backups"
if systemctl is-enabled --quiet ot-backup.timer 2>/dev/null; then
  pass "ot-backup.timer enabled"
  last="$(systemctl show ot-backup.service -p Result --value)"
  [[ "$last" == "success" ]] && pass "last backup run: success" || warn "last backup result: $last (journalctl -u ot-backup)"
else
  warn "backups not configured (docs/operations.md → Backups)"
fi
snaps="$(find /var/lib/ot/snapshots -name 'ot-*.db' 2>/dev/null | wc -l)"
[[ "$snaps" -gt 0 ]] && pass "$snaps local snapshot(s)" || warn "no local snapshots yet"

section "Checks to run from other devices"
cat <<EOF
  * From each household device (Tailscale on): open  http://$FIRST_HOST/_/whoami
    and type  $FIRST_HOST/test  in the address bar. Record results in docs/client-setup.md.
  * From a LAN device with Tailscale OFF, this must time out (LAN is blocked):
        curl -m 5 http://${lan_ip:-<pi-lan-ip>}/
EOF

echo
if [[ $FAILS -eq 0 ]]; then printf '\033[32mAll checks passed.\033[0m\n'; else printf '\033[31m%d check(s) failed.\033[0m\n' "$FAILS"; fi
exit "$FAILS"
