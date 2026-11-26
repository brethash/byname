# shellcheck shell=bash
# Helpers for test/host/run.sh. Runs inside the test container only: it
# rewrites /etc, users and the firewall freely.

SRC="${SRC:-/src}"
DEPLOY="$SRC/deploy"
STUBS="$SRC/test/host/stubs"
TS_STUBS="$SRC/test/host/stubs-tailscale"
export STUB_STATE=/var/lib/host-test/stub
WORK=/var/lib/host-test/work
OUT=/var/lib/host-test/out # rendered firewalls and logs, one per case

PASSES=0
FAILED=()
CASE=""
ARCH=""              # what `uname -m` says (aarch64 for a Pi); empty = the real one
TAILSCALE_INSTALLED=1 # whether tailscale/tailscaled are on PATH

section() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok() { PASSES=$((PASSES + 1)); printf '  \033[32mok\033[0m   %s\n' "$*"; }
bad() { FAILED+=("$CASE: $*"); printf '  \033[31mFAIL\033[0m %s\n' "$*"; }
note() { printf '  \033[33mnote\033[0m %s\n' "$*"; }

# check DESCRIPTION COMMAND...: ok if the command succeeds.
check() {
  local desc=$1
  shift
  if "$@" >/dev/null 2>&1; then ok "$desc"; else bad "$desc"; fi
}

# expect FILE SPEC...: each SPEC is +TEXT (FILE must contain the fixed string
# TEXT), -TEXT (must not), ~REGEX (a line must match the extended regex) or
# !REGEX (no line may).
expect() {
  local file=$1 spec text found
  shift
  for spec in "$@"; do
    text=${spec:1}
    case ${spec:0:1} in
      [+-]) grep -qF -- "$text" "$file" && found=1 || found=0 ;;
      *) grep -qE -- "$text" "$file" && found=1 || found=0 ;;
    esac
    # Passes are counted, not printed: there are hundreds.
    case ${spec:0:1}$found in
      +1 | -0 | '~1' | '!0') PASSES=$((PASSES + 1)) ;;
      +0 | '~0') bad "${file##*/} lacks: $text" ;;
      *) bad "${file##*/} should not have: $text" ;;
    esac
  done
}

# env_is KEY VALUE: KEY=VALUE in /etc/byname/byname.env.
env_is() {
  local got
  got="$(sed -n "s/^$1=//p" /etc/byname/byname.env)"
  if [[ "$got" == "$2" ]]; then PASSES=$((PASSES + 1)); else bad "byname.env $1='$got', want '$2'"; fi
}

# Back to a host bootstrap.sh hasn't touched (users and packages stay).
reset_host() {
  if [[ -d $STUB_STATE ]]; then PATH="$STUBS:$PATH" systemctl stop byname.service >/dev/null 2>&1; fi
  rm -rf /etc/byname /etc/credstore /etc/systemd/system/byname* /etc/systemd/journald.conf.d \
    /etc/systemd/system/nftables.service.d /run/systemd/system/nftables.service.d \
    /etc/ssh/sshd_config.d/01-byname-hardening.conf /etc/ssh/sshd_config.d/99-host-test-broken.conf \
    /etc/apt/apt.conf.d/20auto-upgrades /etc/apt/apt.conf.d/52byname-unattended-upgrades \
    /usr/local/sbin/byname-verify /usr/local/sbin/byname-restore
  nft flush ruleset
  rm -rf "$STUB_STATE"
  mkdir -p "$STUB_STATE/units"
  # sshd is running on a real host (bootstrap.sh reloads it).
  touch "$STUB_STATE/units/ssh.service.active" "$STUB_STATE/units/ssh.service.enabled"
  cp "$WORK/config.txt.orig" /boot/firmware/config.txt
  ARCH="" TAILSCALE_INSTALLED=1
}

# Google sign-in configured, as docs/public-access.md has you do before
# --public-domain or --funnel.
signin() {
  install -d -m 0755 /etc/byname
  install -d -m 0700 /etc/credstore
  printf 'BYNAME_GOOGLE_CLIENT_ID=123-test.apps.googleusercontent.com\nBYNAME_GOOGLE_ALLOW=family.example\n' \
    >> /etc/byname/byname.local.env
  printf 'not-a-real-secret\n' > /etc/credstore/byname.google-client-secret
  chmod 0600 /etc/credstore/byname.google-client-secret
}

stub_path() {
  local p="$STUBS"
  [[ $TAILSCALE_INSTALLED -eq 1 ]] && p+=":$TS_STUBS"
  echo "$p:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin"
}

# run_bootstrap CASE EXPECT ARGS...: run deploy/host/bootstrap.sh with the
# stubs. EXPECT is "ok" (must succeed) or an extended regex the failing run's
# output must match. Output: $OUT/bootstrap-CASE.log, also in $LOG.
run_bootstrap() {
  CASE=$1
  local expect=$2 rc=0
  shift 2
  LOG="$OUT/bootstrap-$CASE.log"
  # Docker bind-mounts /etc/hosts, so `sed -i` (rename) can't replace it: drop
  # the line bootstrap.sh would edit, in place, so it appends one instead.
  grep -v '^127\.0\.1\.1' /etc/hosts > "$WORK/hosts" || true
  cat "$WORK/hosts" > /etc/hosts
  printf '\n\033[1m-- %s:\033[0m bootstrap.sh %s\n' "$CASE" "$*"
  PATH="$(stub_path)" STUB_ARCH="$ARCH" bash "$DEPLOY/host/bootstrap.sh" "$@" > "$LOG" 2>&1 || rc=$?
  if [[ $expect == ok ]]; then
    if [[ $rc -eq 0 ]]; then
      ok "bootstrap.sh succeeded"
    else
      bad "bootstrap.sh exited $rc; last lines:"
      tail -n 8 "$LOG" | sed 's/^/         /'
      return 1
    fi
  else
    if [[ $rc -ne 0 ]] && grep -qE -- "$expect" "$LOG"; then
      ok "bootstrap.sh refused: $(grep -oE -- "$expect" "$LOG" | head -1)"
    else
      bad "bootstrap.sh exited $rc, want a failure matching /$expect/; last lines:"
      tail -n 8 "$LOG" | sed 's/^/         /'
      return 1
    fi
  fi
}

# The rendered firewall is valid, loaded, and has (or hasn't) the given rules.
check_firewall() {
  local f=/etc/byname/firewall.nft
  if [[ ! -f $f ]]; then bad "no $f"; return 1; fi
  cp "$f" "$OUT/firewall-$CASE.nft"
  if nft -c -f "$f" > "$WORK/nft.err" 2>&1; then ok "firewall-$CASE.nft passes nft -c"; else bad "nft -c: $(cat "$WORK/nft.err")"; fi
  check "table inet byname_filter is loaded" nft list table inet byname_filter
  expect "$f" "$@"
}

# run_verify CASE: byname-verify with the stubs; output without colours in $LOG.
# Many checks fail in a container; the test looks at specific lines.
run_verify() {
  CASE=$1
  LOG="$OUT/verify-$1.log"
  PATH="$(stub_path)" byname-verify 2>&1 | sed 's/\x1b\[[0-9;]*m//g' > "$LOG"
  if grep -qE 'All checks passed|check\(s\) failed' "$LOG"; then
    ok "byname-verify ran to the end"
  else
    bad "byname-verify stopped early; last lines:"
    tail -n 5 "$LOG" | sed 's/^/         /'
  fi
  check "byname-verify: no unbound variables or missing commands" bash -c "! grep -qE 'unbound variable|command not found' '$LOG'"
}

# The systemctl stub's log has a line matching the extended regex.
called() { grep -qE -- "$1" "$STUB_STATE/systemctl.log"; }
