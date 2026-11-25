# Operating byname

Everything here runs from the repo on your Mac unless it says "on the host".
The host is a Raspberry Pi at home (first-time setup below) or a cloud server
at AWS or Hetzner ([cloud.md](cloud.md)). After setup, both are run the same
way.

**The service name.** `nym` is the default: people type `nym/wiki`, and the
host is called `nym`. To use another name (`go`, `links`…), pass
`--name <name>` to `bootstrap.sh` (or set `name` in `deploy/cloud`). Then use
that name wherever these docs say `nym` in a hostname or URL, and
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
| `/etc/systemd/system/byname.service.d/10-lan-access.conf` | present only with `--lan-access` |
| `/etc/byname/firewall.nft` | nftables table `inet byname_filter` |
| `/etc/byname/byname-backup.env` | backup bucket/region (optional) |
| `/etc/credstore/byname-backup-aws` | backup AWS key (root 0600, optional) |
| `/var/lib/byname/byname.db` | the database (SQLite, WAL) |
| `/var/lib/byname/snapshots/` | `byname-<UTC time>.db` snapshots (last 14) |
| `/usr/local/sbin/byname-verify`, `byname-restore` | checks and restore helper |
| units | `byname.service`, `byname-firewall.service`, `byname-backup.service` + `.timer` |

## First-time setup on a Raspberry Pi (milestone M0)

For a cloud server, follow [cloud.md](cloud.md) instead.

### 1. Tailscale policy (admin console, before the Pi joins)

Merge [`deploy/tailscale/policy.hujson`](../deploy/tailscale/policy.hujson) into
your tailnet policy: it defines `tag:nym`, lets household members reach it on
port 80, and allows you Tailscale SSH to it. Confirm MagicDNS is on
(admin console → DNS). Make sure no other device is already named `nym`.

### 2. Flash the SD card

Raspberry Pi Imager → **Raspberry Pi OS Lite (64-bit)**. In OS customisation:

- hostname `nym` (or your own service name)
- your own username (not `pi`) and password
- **Services → Enable SSH → Allow public-key authentication only**, paste your key
- **no** Wi-Fi settings (wired Ethernet only)
- your locale and time zone

Use the official 5.1 V / 2.5 A supply. Under-voltage is the usual cause of SD
corruption, and `byname-verify` checks for it.

### 3. Router: fixed address and a name for LAN devices

Boot the Pi on Ethernet and, in your router:

1. **Reserve its DHCP address** so it never changes.
2. **Make `nym` resolve to that address** for home-network devices. Many
   routers already register DHCP hostnames, so `nym` (or `nym.<router-domain>`,
   e.g. `nym.lan`) may work as soon as the Pi has booted. Check from a device
   with Tailscale off: `nslookup nym`. If it doesn't resolve, add a local DNS /
   static host entry `nym` → the reserved address. Note your router's local
   domain (often `lan` or `home`).

Devices running Tailscale keep using MagicDNS for `nym`; this entry is only
for devices without it.

### 4. Bootstrap over the LAN

```sh
scp -r deploy <you>@<pi-lan-ip>:
ssh <you>@<pi-lan-ip> 'sudo ./deploy/host/bootstrap.sh --admin-user <you> --lan-subnet 192.168.1.0/24 --lan-access --lan-domain lan'
```

Use your real LAN subnet and router domain, and add `--name <name>` if you
aren't using `nym`. Leave out `--lan-access` to keep
byname tailnet-only. Running the script again without it turns LAN access off.
The script refuses to load the firewall if your SSH session comes from
outside the LAN subnet. It removes `avahi-daemon`, so `nym.local` stops
resolving. Use the LAN IP or, after the next step, `nym` over Tailscale.

### 5. Join the tailnet

```sh
ssh <you>@<pi-lan-ip>
sudo tailscale up --ssh --hostname=nym --advertise-tags=tag:nym   # open the printed URL
sudo ./deploy/host/bootstrap.sh --admin-user <you> --lan-subnet 192.168.1.0/24 --lan-access --lan-domain lan   # again: fills BYNAME_HOSTS
sudo reboot                                                       # applies Wi-Fi/BT off
```

From now on, `ssh <you>@nym` works from anywhere on the tailnet (Tailscale SSH;
the "check" rule asks you to re-confirm in the browser now and then).

### 6. Deploy and verify

```sh
make deploy TARGET=<you>@nym
ssh <you>@nym sudo byname-verify
```

`byname-verify` checks the whole baseline: radios off, SSH hardening, updates,
journal in RAM, firewall, LAN access consistent across firewall, systemd and
app, Tailscale name and tag, the sandbox exposure score (≤ 2.0), health and
WhoIs over the tailnet, and the DNS-rebinding guard. Its exit code is the
number of failures.

### 7. Check every household device

On each device with Tailscale (app connected, "Use Tailscale DNS" on):

1. open `http://nym/_/whoami`. It should show that person's Tailscale login.
2. create `nym/test` once, then type `nym/test` in the address bar.

On a device **without** Tailscale on the home Wi-Fi, `http://nym/_/whoami`
should say "home network (no Tailscale)", and `nym/test` should open.

Record what happens in [client-setup.md](client-setup.md). If you have a
guest Wi-Fi, from it `curl -m 5 http://<pi-lan-ip>/` should time out.

## Deploying changes

```sh
make deploy TARGET=<you>@nym          # cloud: TARGET=admin@nym
```

This runs the full gate (vet, staticcheck, race tests, govulncheck,
shellcheck), builds static arm64 and amd64 binaries, ships the one that
matches the host, then on the host:

1. verifies the binary's sha256;
2. snapshots the database **with the current binary** (before any migration);
3. switches the `current` symlink atomically and restarts `nym`;
4. polls `/_/healthz` for the new version for 20 s; on failure it switches
   back and restarts the previous version.

It refuses a dirty working tree unless you set `FORCE=1`.

If a release that changed the database schema has to be rolled back, the old
binary refuses the newer schema on purpose. Restore the pre-deploy snapshot
that step 2 wrote: `sudo byname-restore $(ls -1 /var/lib/byname/snapshots/byname-*.db | tail -1)`.

## Backups (S3)

`byname serve` writes a snapshot 60 s after any change and every 24 h. The hourly
`byname-backup.timer` uploads the newest one if it hasn't uploaded it yet.

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

The key can only `PutObject` (with SSE) under `byname/`. It cannot list, read or
delete, and versioning keeps every earlier object even if a key is
overwritten. A stolen Pi or a compromised server therefore can't destroy the
backup history. Cost
for a household's links is a fraction of a cent a month.

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
ls /var/lib/byname/snapshots/
sudo byname-restore /var/lib/byname/snapshots/byname-20261124T120000.000Z.db
```

From S3 (the host's key cannot read, so fetch from your Mac):

```sh
aws s3 ls s3://$B/byname/
aws s3 cp s3://$B/byname/byname-20261124T120000.000Z.db.gz .
scp byname-20261124T120000.000Z.db.gz <you>@nym:
ssh <you>@nym sudo byname-restore byname-20261124T120000.000Z.db.gz
```

`byname-restore` integrity-checks the file, snapshots the current database first
(so the restore itself can be undone), swaps the file and restarts `nym`.

### Restore drill (do this once in M3, then yearly)

Non-destructively, on your Mac:

```sh
aws s3 cp s3://$B/byname/<newest>.db.gz - | gunzip > /tmp/drill.db
bin/byname export --db /tmp/drill.db | head -40     # links are there
```

## Day to day

| Task | Command |
|---|---|
| Logs | `ssh <you>@nym journalctl -u byname -f` (debug detail: `BYNAME_LOG_LEVEL=debug` in `/etc/byname/byname.env`) |
| Health | `curl http://nym/_/healthz` (version, link count, free disk; under-voltage flags on a Pi) |
| Who changed what | `http://nym/_/activity`, or a link's page (`nym/name+`) |
| Undo a bad edit | link page → History → "Restore this version" |
| Export | `http://nym/_/export` or `sudo -u byname /usr/local/lib/byname/current/byname export` |
| Bulk import | `sudo -u byname /usr/local/lib/byname/current/byname import --hosts nym --dry-run file.json` |
| Change settings | edit `/etc/byname/byname.local.env` (yours; `byname.env` is managed), then `sudo systemctl restart byname` |
| Turn LAN access on/off | re-run `bootstrap.sh` with or without `--lan-access` |
| Turn the public address on/off | re-run `bootstrap.sh` with or without `--public` ([public-access.md](public-access.md)) |
| Who is signed in (public address) | `sudo -u byname /usr/local/lib/byname/current/byname sessions`; `… sessions revoke <email>` signs someone out |
| Rotate the AWS key | create a new key, replace `/etc/credstore/byname-backup-aws`, delete the old key |
| Re-check baseline | `sudo byname-verify` |

The OS (and Raspberry Pi firmware) and Tailscale update themselves daily.
The host reboots at 04:00 when an update needs it.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Browser searches for `nym/foo` | See [client-setup.md](client-setup.md); type `http://nym/foo` once, or use the keyword shortcut |
| "byname is not reachable from this network" (403) | The device's address isn't allowed: it's outside the LAN subnet (guest Wi-Fi?) without Tailscale, or LAN access is off and Tailscale is off on that device |
| `nym` doesn't resolve on a device without Tailscale | Router DNS has no `nym` entry; see step 3. Meanwhile `http://<pi-lan-ip>/` works (bootstrap adds the Pi's LAN address to `BYNAME_HOSTS`); re-run bootstrap if that address changed |
| "unknown host" (421) | A name other than those in `BYNAME_HOSTS` (e.g. `nym.home` when you passed `--lan-domain lan`); re-run bootstrap with the right `--lan-domain`, or add it via `BYNAME_HOSTS` in `/etc/byname/byname.local.env` |
| "cross-origin request blocked" (403) | A page on another site tried to change a link. That's working as designed. If it was you, use the byname UI itself |
| `/_/whoami` shows no Tailscale user | WhoIs failed; `journalctl -u byname \| grep whois`; check `/var/run/tailscale/tailscaled.sock` exists |
| Locked out of SSH | Tailscale SSH (`ssh <you>@nym`) bypasses the LAN rule. Otherwise, on a Pi use a keyboard and HDMI, then `sudo systemctl stop byname-firewall`. A cloud server has no console login: build a new one ([cloud.md](cloud.md)) and restore the newest S3 backup (see Restore) |
| Deploy rolled back | Read the journal lines it printed; `ssh <you>@nym journalctl -u byname -n 50` |

## Security model in one paragraph

Only tailnet devices, plus home-LAN devices when `--lan-access` is on, can
reach byname's plain-HTTP address: nftables, systemd `IPAddressAllow` and the app
itself each check that. (The optional public address is separate: Tailscale
Funnel delivers it over a private socket, and everyone outside the tailnet
must sign in with Google or Slack; see [public-access.md](public-access.md).) Anyone who can reach it may create or edit links; there are no logins.
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
