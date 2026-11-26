# Operating byname

Everything here runs from the repo on your Mac unless it says "on the host".
The host is a Raspberry Pi at home (first-time setup below) or a cloud server
at AWS or Hetzner ([cloud.md](cloud.md)). After setup, both are run the same
way.

**The service name.** `me` is the default: people type `me/wiki`, and the
host is called `me`. To use another name (`go`, `links`…), pass
`--name <name>` to `bootstrap.sh` (or set `name` in `deploy/cloud`). Then use
that name wherever these docs say `me` in a hostname or URL, and
`tag:<name>` for the Tailscale tag. The program's own files (`byname.service`,
`/etc/byname`, `byname-verify`) keep their names.

## What lives where on the host

| Path | What |
|---|---|
| `/usr/local/lib/byname/<version>/byname` | deployed binaries (current + 2 previous) |
| `/usr/local/lib/byname/current` | symlink to the live version |
| `/etc/byname/byname.env` | `byname serve` settings (`BYNAME_HOSTS`, …), rewritten by `bootstrap.sh` |
| `/etc/byname/byname.local.env` | your own `BYNAME_*` overrides (kept across bootstrap runs) |
| `/etc/byname/host.conf` | platform (`pi`/`cloud`), name and tag, for `byname-verify` |
| `/etc/systemd/system/byname.service.d/05-tailscale.conf` | present only with Tailscale: starts byname after `tailscaled` (without Tailscale, `tailscaled` is masked) |
| `/etc/systemd/system/byname.service.d/10-lan-access.conf` | present only with `--lan-access` |
| `/etc/byname/firewall.nft` | nftables table `inet byname_filter` |
| `/etc/byname/byname-backup.env` | backup bucket/region (optional) |
| `/etc/credstore/byname-backup-aws` | backup AWS key (root 0600, optional) |
| `/var/lib/byname/byname.db` | the database (SQLite, WAL) |
| `/var/lib/byname/snapshots/` | `byname-<UTC time>.db` snapshots (last 14), and the `pre-restore-<UTC time>.db` copies `byname-restore` keeps (never pruned) |
| `/usr/local/sbin/byname-verify`, `byname-restore` | checks and restore helper |
| units | `byname.service`, `byname-firewall.service`, `byname-backup.service` + `.timer` |

## First-time setup on a Raspberry Pi (milestone M0)

For a cloud server, follow [cloud.md](cloud.md) instead.

### 1. Tailscale policy (admin console, before the Pi joins)

Merge [`deploy/tailscale/policy.hujson`](../deploy/tailscale/policy.hujson) into
your tailnet policy: it defines `tag:me`, lets household members reach it on
port 80, and allows you Tailscale SSH to it. Confirm MagicDNS is on
(admin console → DNS). Make sure no other device is already named `me`.

### 2. Flash the SD card

Raspberry Pi Imager → **Raspberry Pi OS Lite (64-bit)**. In OS customisation:

- hostname `me` (or your own service name)
- your own username (not `pi`) and password
- **Services → Enable SSH → Allow public-key authentication only**, paste your key
- **no** Wi-Fi settings (wired Ethernet only)
- your locale and time zone

Use the official 5.1 V / 2.5 A supply. Under-voltage is the usual cause of SD
corruption, and `byname-verify` checks for it.

### 3. Router: fixed address and a name for LAN devices

Boot the Pi on Ethernet and, in your router:

1. **Reserve its DHCP address** so it never changes.
2. **Make `me` resolve to that address** for home-network devices. Many
   routers already register DHCP hostnames, so `me` (or `me.<router-domain>`,
   e.g. `me.lan`) may work as soon as the Pi has booted. Check from a device
   with Tailscale off: `nslookup me`. If it doesn't resolve, add a local DNS /
   static host entry `me` → the reserved address. Note your router's local
   domain (often `lan` or `home`).

Devices running Tailscale keep using MagicDNS for `me`; this entry is only
for devices without it.

### 4. Bootstrap over the LAN

```sh
git archive HEAD deploy/host deploy/systemd | ssh <you>@<pi-lan-ip> tar -x    # only what bootstrap needs
ssh <you>@<pi-lan-ip> 'sudo ./deploy/host/bootstrap.sh --admin-user <you> --lan-subnet 192.168.1.0/24 --lan-access --lan-domain lan'
```

Use your real LAN subnet and router domain, and add `--name <name>` if you
aren't using `me`. Leave out `--lan-access` to keep
byname tailnet-only. Running the script again without it turns LAN access off.
The script refuses to load the firewall if your SSH session comes from
outside the LAN subnet. It removes `avahi-daemon`, so `me.local` stops
resolving. Use the LAN IP or, after the next step, `me` over Tailscale.

### 5. Join the tailnet

```sh
ssh <you>@<pi-lan-ip>
sudo tailscale up --ssh --hostname=me --advertise-tags=tag:me   # open the printed URL
sudo ./deploy/host/bootstrap.sh --admin-user <you> --lan-subnet 192.168.1.0/24 --lan-access --lan-domain lan   # again: fills BYNAME_HOSTS
sudo reboot                                                       # applies Wi-Fi/BT off
```

From now on, `ssh <you>@me` works from anywhere on the tailnet (Tailscale SSH;
the "check" rule asks you to re-confirm in the browser now and then).

### 6. Deploy and verify

```sh
make deploy TARGET=<you>@me
ssh <you>@me sudo byname-verify
```

`byname-verify` checks the whole baseline: radios off, SSH hardening, updates,
journal in RAM, firewall, LAN access consistent across firewall, systemd and
app, Tailscale name and tag, the sandbox exposure score (≤ 2.0), health and
WhoIs over the tailnet, and the DNS-rebinding guard. Its exit code is the
number of failures.

### 7. Check every household device

On each device with Tailscale (app connected, "Use Tailscale DNS" on):

1. open `http://me/_/whoami`. It should show that person's Tailscale login.
2. create `me/test` once, then type `me/test` in the address bar.

On a device **without** Tailscale on the home Wi-Fi, `http://me/_/whoami`
should say "home network (no Tailscale)", and `me/test` should open.

Record what happens in [client-setup.md](client-setup.md). If you have a
guest Wi-Fi, from it `curl -m 5 http://<pi-lan-ip>/` should time out.

### 8. Optional: sign-in from the internet

Everything above serves the tailnet and the home network, with no sign-in.
To let people anywhere use it, the Pi can publish a public address with
Google or Slack sign-in, exactly as a cloud server does
([cloud.md](cloud.md#your-own-domain-no-tailscale-walkthrough) is the
reference walkthrough, and [public-access.md](public-access.md) has the
details). Sign-in applies only to requests that arrive on the public
address. Tailnet devices, and LAN devices with `--lan-access`, keep using
plain HTTP with no sign-in.

There are two ways. Pick one:

| | `--public-domain` | `--funnel` |
|---|---|---|
| Address | `https://me.example.com/wiki` | `https://me.<tailnet>.ts.net/wiki` |
| Needs | a domain, and TCP 80 and 443 forwarded from your router | Tailscale, with Funnel enabled |
| Doesn't work | behind carrier-grade NAT | never blocked by your router or ISP |

**Which one?** Compare the WAN address your router shows with
`curl -s https://checkip.amazonaws.com` from home. If they differ, or the
router's address starts `100.64`–`100.127`, your ISP uses carrier-grade NAT
and port-forwarding can't work: use `--funnel`. Some ISPs also block inbound
80 or 443 on home connections; the same applies.

**Before you start** (for `--public-domain`):

- Reserve the Pi's address in your router (step 3), then forward **TCP 80 and
  443**, and only those, to it. Never forward 22: SSH on the Pi is for the
  home network and the tailnet.
- Your home IP address probably changes now and then. Use a short TTL (5
  minutes) on the DNS record and keep it up to date with your DNS host's
  dynamic-DNS update, or your router's. byname gets and renews its
  certificate only while the name points at your home. If you'd rather not
  keep a record updated, use `--funnel`.
- Add an `AAAA` record only if your router lets IPv6 in to the Pi.
- A Slack or Google app, with the redirect URL for your public address
  ([public-access.md](public-access.md#2-sign-in-providers)).

**Turn it on.** `SERVER` is the Pi's tailnet name or LAN address. Use the
same flags you used in step 4 (or 5) throughout.

```sh
# 1. Sign-in settings and secrets (the Pi is already bootstrapped):
ssh <you>@SERVER 'sudo tee -a /etc/byname/byname.local.env' <<'EOF'
BYNAME_SLACK_CLIENT_ID=1234567890.1234567890
BYNAME_SLACK_TEAMS=T0123ABCD
EOF
pbpaste | ssh <you>@SERVER 'sudo sh -c "umask 077; cat > /etc/credstore/byname.slack-client-secret"'

# 2. Turn on the public address: add one flag to your step 4/5 command
ssh <you>@SERVER 'sudo ./deploy/host/bootstrap.sh --admin-user <you> --lan-subnet 192.168.1.0/24 --lan-access --lan-domain lan \
  --public-domain me.example.com'          # or: --funnel

# 3. Check
ssh <you>@SERVER sudo byname-verify
```

For Google, use `BYNAME_GOOGLE_CLIENT_ID` and `BYNAME_GOOGLE_ALLOW`, and the
`byname.google-client-secret` file. Without `--lan-access`, leave out that
flag and `--lan-domain`. Bootstrap refuses to open the address until
sign-in is configured. Running it again without the flag turns the public
address off.

`byname-verify` adds a **Public address** section: DNS, the firewall, a real
certificate, the sign-in redirect, and that the secrets are in place. Then
check from a phone on mobile data (Wi-Fi and Tailscale off), as in
[public-access.md](public-access.md#4-turn-it-on): `https://<address>/_/whoami`
must ask you to sign in, and refuse an account that isn't allowed.

**Things specific to a Pi at home**

- **LAN access next to a public domain.** If your router rewrites the source
  address of forwarded connections (it makes them look like they come from
  the router itself), internet clients would look like LAN devices on port
  80 and could skip sign-in. Don't combine `--lan-access` with
  `--public-domain` unless
  `curl -m 5 -i -H 'Host: me' http://me.example.com/_/whoami`, run from
  mobile data, answers with a redirect to https and not the "home network
  (no Tailscale)" page. `byname-verify` lists this check. If in doubt,
  leave `--lan-access` off, or use `--funnel`, which has no open ports.
- **The public address from inside your home.** Some routers can't loop a
  request for your own public IP back inside the network. If
  `https://me.example.com` fails on home Wi-Fi but works on mobile data, use
  `me/wiki` at home (Tailscale or `--lan-access`).
- **Deploys and SSH** still go to the Pi's LAN address or its tailnet name,
  never the public domain.

## Deploying changes

```sh
make deploy TARGET=<you>@me          # cloud: TARGET=admin@me
```

This runs the full gate (vet, staticcheck, race tests, govulncheck,
shellcheck), builds static arm64 and amd64 binaries, ships the one that
matches the host, then on the host:

1. verifies the binary's sha256;
2. snapshots the database **with the current binary** (before any migration);
3. switches the `current` symlink atomically and restarts `byname`;
4. polls `/_/healthz` for the new version (20 tries, up to about 80 s); on
   failure it switches back, puts back the systemd units it replaced, and
   restarts the previous version.

It refuses a dirty working tree unless you set `FORCE=1`.

If a release that changed the database schema has to be rolled back, the old
binary refuses the newer schema on purpose, so the automatic rollback fails
too. Restore the snapshot that step 2 wrote. The deploy prints its path and,
when the rollback fails, the whole `sudo byname-restore …` command to run.
Right after the failed deploy it is also the newest snapshot:

```sh
sudo byname-restore --latest        # or: sudo byname-restore --list, then pick one
```

Changes made since that snapshot are lost.

## Backups (S3)

`byname serve` writes a snapshot 60 s after any change and every 24 h. The hourly
`byname-backup.timer` uploads the newest one if it hasn't uploaded it yet. It
refuses a snapshot that isn't a complete SQLite file, and never replaces an
object already in the bucket.

### One-time AWS setup (from your Mac, with admin credentials)

```sh
B=<bucket-name> R=<region>
aws s3api create-bucket --bucket $B --region $R --create-bucket-configuration LocationConstraint=$R   # omit the last flag in us-east-1
aws s3api put-public-access-block --bucket $B --public-access-block-configuration \
  BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
aws s3api put-bucket-versioning --bucket $B --versioning-configuration Status=Enabled
aws s3api put-bucket-encryption --bucket $B --server-side-encryption-configuration \
  '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"}}]}'
aws s3api put-bucket-lifecycle-configuration --bucket $B --lifecycle-configuration file://deploy/aws/lifecycle.json

sed "s/BUCKET_NAME/$B/" deploy/aws/iam-policy.json > /tmp/byname-backup-policy.json
aws iam create-user --user-name byname-backup
aws iam put-user-policy --user-name byname-backup --policy-name byname-backup-put --policy-document file:///tmp/byname-backup-policy.json
aws iam create-access-key --user-name byname-backup      # note the key id and secret
```

The key can only create new objects (with SSE) under `byname/`. Every upload
sends `If-None-Match: *`, and the policy refuses any `PutObject` without it, so
the key can't replace an existing backup. It can't list, read or delete
either. A stolen Pi or a compromised server can therefore add objects, but
can't destroy or rewrite the backup history.

The lifecycle rule expires each backup a year after its upload, then keeps
that expired version for another year before deleting it. Cost for a
household's links is a fraction of a cent a month.

Set up with an older copy of these files? Deploy this version first (older ones upload
without `If-None-Match`, which the new policy refuses), then re-run the
`put-bucket-lifecycle-configuration` and `put-user-policy` commands above. The
old policy let the key overwrite backups, and the old lifecycle rule deleted
replaced versions after 90 days.

### On the host

```sh
sudo install -m 0600 /dev/null /etc/credstore/byname-backup-aws
sudo nano /etc/credstore/byname-backup-aws
#   [default]
#   aws_access_key_id = AKIA...
#   aws_secret_access_key = ...
sudo cp /etc/byname/byname-backup.env.example /etc/byname/byname-backup.env && sudo nano /etc/byname/byname-backup.env
sudo systemctl enable --now byname-backup.timer
sudo systemctl start byname-backup.service && journalctl -u byname-backup -n 5
```

## Restore

From a local snapshot (on the host):

```sh
sudo byname-restore --list          # oldest first, with times and sizes
sudo byname-restore /var/lib/byname/snapshots/byname-20261124T120000.000Z.db
sudo byname-restore --latest        # the newest snapshot
```

The snapshot directory is readable only by `byname`, so list it with
`--list` (or `sudo ls`): a plain `ls`, or a `$(ls …)` inside a `sudo` command,
runs as you and can't see it.

From S3 (the host's key cannot read, so fetch from your Mac):

```sh
aws s3 ls s3://$B/byname/
aws s3 cp s3://$B/byname/byname-20261124T120000.000Z.db.gz .
scp byname-20261124T120000.000Z.db.gz <you>@me:
ssh <you>@me sudo byname-restore byname-20261124T120000.000Z.db.gz
```

`byname-restore` integrity-checks the file, stops `byname`, keeps the current
database (with its `-wal` and `-shm` files) as
`snapshots/pre-restore-<UTC time>.db`, installs the snapshot and starts
`byname` again. Undo a restore by restoring that copy:
`sudo byname-restore /var/lib/byname/snapshots/pre-restore-<UTC time>.db`.
Keeping the copy doesn't need the byname binary, so a restore works even when
the current database is damaged or newer than the binary. `pre-restore-*`
copies are never pruned or uploaded; delete old ones yourself.

A restore clears the sign-in sessions in the restored copy, so everyone using
the public address signs in again. Otherwise sessions revoked after the
snapshot was taken (`byname sessions revoke`) would come back.

### Restore drill (do this once in M3, then yearly)

Non-destructively, on your Mac:

```sh
aws s3 cp s3://$B/byname/<newest>.db.gz - | gunzip > /tmp/drill.db
bin/byname export --db /tmp/drill.db | head -40     # links are there
```

## Day to day

| Task | Command |
|---|---|
| Logs | `ssh <you>@me journalctl -u byname -f` (debug detail: `BYNAME_LOG_LEVEL=debug` in `/etc/byname/byname.local.env`, then `sudo systemctl restart byname`) |
| Health | `curl http://me/_/healthz` (version, link count, free disk; under-voltage flags on a Pi) |
| Who changed what | `http://me/_/activity`, or a link's page (`me/name+`) |
| Undo a bad edit | link page → History → "Restore this version" |
| Export | `http://me/_/export` or `sudo -u byname /usr/local/lib/byname/current/byname export` |
| Bulk import | `sudo -u byname /usr/local/lib/byname/current/byname import --hosts me --dry-run file.json`. All or nothing: any invalid entry fails the import with a non-zero exit. It honours the link cap (`--max-links`, `BYNAME_MAX_LINKS`); with your own domain, add `--public-url https://…` so links back to it are refused |
| Change settings | edit `/etc/byname/byname.local.env` (yours; `byname.env` is managed), then `sudo systemctl restart byname` |
| Turn LAN access on/off | re-run `bootstrap.sh` with or without `--lan-access` |
| Turn the public address on/off | re-run `bootstrap.sh` with or without `--public-domain` or `--funnel` ([public-access.md](public-access.md)) |
| Who is signed in (public address) | `sudo -u byname /usr/local/lib/byname/current/byname sessions`; `… sessions revoke <email>` signs someone out |
| Rotate the AWS key | create a new key, replace `/etc/credstore/byname-backup-aws`, delete the old key |
| Re-check baseline | `sudo byname-verify` |

The OS (and Raspberry Pi firmware) and Tailscale update themselves daily.
The host reboots at 04:00 when an update needs it.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Browser searches for `me/foo` | See [client-setup.md](client-setup.md); type `http://me/foo` once, or use the keyword shortcut |
| "byname is not reachable from this network" (403) | The device's address isn't allowed: it's outside the LAN subnet (guest Wi-Fi?) without Tailscale, or LAN access is off and Tailscale is off on that device |
| `me` doesn't resolve on a device without Tailscale | Router DNS has no `me` entry; see step 3. Meanwhile `http://<pi-lan-ip>/` works (bootstrap adds the Pi's LAN address to `BYNAME_HOSTS`); re-run bootstrap if that address changed |
| "unknown host" (421) | A name other than those in `BYNAME_HOSTS` (e.g. `me.home` when you passed `--lan-domain lan`); re-run bootstrap with the right `--lan-domain`, or add it via `BYNAME_HOSTS` in `/etc/byname/byname.local.env` |
| "cross-origin request blocked" (403) | A page on another site tried to change a link. That's working as designed. If it was you, use the byname UI itself |
| `/_/whoami` shows no Tailscale user | WhoIs failed; `journalctl -u byname \| grep whois`; check `/var/run/tailscale/tailscaled.sock` exists |
| Locked out of SSH | Tailscale SSH (`ssh <you>@me`) bypasses the LAN rule. Otherwise, on a Pi use a keyboard and HDMI, then `sudo systemctl stop byname-firewall`. A cloud server without Tailscale takes OpenSSH from `ssh_cidrs`: if your address changed, update `ssh_cidrs` and run `tofu apply`. Otherwise it has no console login: build a new one ([cloud.md](cloud.md)) and restore the newest S3 backup (see Restore) |
| Deploy rolled back | Read the journal lines it printed; `ssh <you>@me journalctl -u byname -n 50` |
| Deploy rolled back, and the rollback failed too | The release changed the database schema, which the previous version refuses: run the `sudo byname-restore …` command the deploy printed (see Deploying changes) |
| `byname-restore` says it can't read a file as byname | It reads byname's own files, and any path through a symlink, with byname's rights. Copy the file to a plain path (your home) and restore from there |

## Security model in one paragraph

Only tailnet devices, plus home-LAN devices when `--lan-access` is on, can
reach byname's plain-HTTP address: nftables, systemd `IPAddressAllow` and the app
itself each check that. (The optional public address is separate: Tailscale
Funnel delivers it over a private socket, and everyone outside the tailnet
must sign in with Google or Slack; see [public-access.md](public-access.md).) Anyone who can reach the plain-HTTP address may create or edit links; there are no logins there.
Each change is recorded in an audit log that the database refuses to rewrite,
with the person's Tailscale identity, or just the IP address for LAN devices
without Tailscale. Any earlier version can be restored with one click.
LAN traffic is plain HTTP, so other devices on the home network could in
principle watch or tamper with it; Tailscale traffic is encrypted. What a hostile web page or
DNS-rebinding attacker could otherwise do is blocked by the Host allowlist
and cross-origin checks on every write. Destinations are restricted to plain
`http(s)` URLs with no embedded credentials, and redirects are never cached.
What this does **not** stop is a household member deliberately changing a
link. That is visible in the history and can be undone. If it ever matters,
"locked links" (edits restricted to named tailnet users) is the planned
upgrade.
